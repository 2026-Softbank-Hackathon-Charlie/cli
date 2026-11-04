package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

// Engine kinds the view only prints.
const (
	kindLog      = "log"
	kindRollback = "rollback"
)

func TestWatchExitCodes(t *testing.T) {
	for status, want := range map[apiv1.RunStatus]int{
		apiv1.RunStatusSucceeded:   ExitOK,
		apiv1.RunStatusRolledBack:  ExitRunFailed,
		apiv1.RunStatusHalted:      ExitRunFailed,
		apiv1.RunStatusFailed:      ExitRunFailed,
		apiv1.RunStatusAborted:     ExitRunFailed,
		apiv1.RunStatusInterrupted: ExitRunFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newFake(t, "tok")
			f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
				s := sse{w}
				s.snapshot(fixtureRun(apiv1.RunStatusRunning))
				s.event(1, kindStepStarted, "prepare", "", nil)
				s.event(2, kindStatusChanged, "", "run ended", map[string]any{"status": string(status), "phase": "done"})
				s.snapshot(fixtureRun(status))
			}
			r := loggedIn(t, f).run("", "migrate", "watch", "run_1")
			if r.code != want {
				t.Fatalf("exit %d, want %d\n%s%s", r.code, want, r.stdout, r.stderr)
			}
			if want == ExitRunFailed && !strings.Contains(r.stderr, "run run_1 ended "+string(status)+": verify found 3 missing rows") {
				t.Errorf("stderr %q", r.stderr)
			}
			// The final view is printed for logs.
			if !strings.Contains(r.stdout, "rows 0, writes 0, files 0") {
				t.Errorf("no final view:\n%s", r.stdout)
			}
		})
	}
}

func TestWatchDedupAcrossReconnect(t *testing.T) {
	f := newFake(t, "tok")
	f.events = func(w http.ResponseWriter, r *http.Request, conn int) {
		s := sse{w}
		switch conn {
		case 0:
			s.snapshot(fixtureRun(apiv1.RunStatusRunning))
			s.event(1, kindStepStarted, "presync", "", nil)
			s.event(2, kindStepSucceeded, "presync", "", map[string]any{"durationMillis": 4200})
			s.event(3, kindMetric, "delta", "replication lag", map[string]any{"lagBytes": 2048})
			s.drop()
		case 1: // replays more than asked, then ends while waiting for /switch
			s.snapshot(fixtureRun(apiv1.RunStatusRunning))
			s.event(2, kindStepSucceeded, "presync", "", map[string]any{"durationMillis": 4200})
			s.event(3, kindMetric, "delta", "replication lag", map[string]any{"lagBytes": 2048})
			s.event(4, kindStepSucceeded, "delta", "", nil)
			s.event(5, kindStatusChanged, "", "awaiting_switch (awaiting_switch)", map[string]any{"status": "awaiting_switch", "phase": "awaiting_switch"})
		default:
			s.snapshot(fixtureRun(apiv1.RunStatusAwaitingSwitch))
			s.event(6, kindStatusChanged, "", "running (cutover)", map[string]any{"status": "running", "phase": "cutover"})
			s.event(7, kindMetric, "cutover", "write downtime", map[string]any{"downtimeMillis": 2100})
			s.snapshot(fixtureRun(apiv1.RunStatusSucceeded))
		}
	}
	r := loggedIn(t, f).run("", "migrate", "watch", "run_1").ok(t)
	for line, want := range map[string]int{
		"[ok] presync succeeded in 4.2s":          1,
		"metric delta: replication lag (2.0 KiB)": 1,
		"[ok] delta succeeded":                    1,
		switchHint("run_1"):                       1,
		"write downtime (2.1s)":                   1,
	} {
		if got := strings.Count(r.stdout, line); got != want {
			t.Errorf("%q printed %d times, want %d\n%s", line, got, want, r.stdout)
		}
	}
	if want := []string{"", "3", "5"}; !reflect.DeepEqual(f.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", f.lastEventIDs(), want)
	}
}

func TestWatchJSON(t *testing.T) {
	f := newFake(t, "tok")
	f.events = func(w http.ResponseWriter, r *http.Request, conn int) {
		s := sse{w}
		s.snapshot(fixtureRun(apiv1.RunStatusRunning))
		if conn == 0 {
			s.event(1, kindLog, "precheck", "checking", nil)
			s.drop()
		}
		s.event(1, kindLog, "precheck", "checking", nil)
		s.event(2, kindRollback, "fence", "rolling back to source", nil)
		s.snapshot(fixtureRun(apiv1.RunStatusRolledBack))
	}
	r := loggedIn(t, f).run("", "--json", "migrate", "watch", "run_1")
	if r.code != ExitRunFailed {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		var m struct {
			Event string          `json:"event"`
			ID    int64           `json:"id"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		names = append(names, fmt.Sprintf("%s:%d", m.Event, m.ID))
		if m.Event == "snapshot" {
			var run apiv1.Run
			if json.Unmarshal(m.Data, &run) != nil || run.RunId != "run_1" {
				t.Errorf("snapshot data %s", m.Data)
			}
		}
	}
	if want := []string{"snapshot:0", "log:1", "snapshot:0", "rollback:2", "snapshot:0"}; !reflect.DeepEqual(names, want) {
		t.Errorf("messages %v, want %v", names, want)
	}
}

func TestWatchInterrupted(t *testing.T) {
	f := newFake(t, "tok")
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		sse{w}.snapshot(fixtureRun(apiv1.RunStatusRunning))
		<-r.Context().Done()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := loggedIn(t, f).runCtx(ctx, "", "migrate", "watch", "run_1")
	if r.code != ExitInterrupted || !strings.Contains(r.stderr, "Reattach with: charlie migrate watch run_1") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestWatchUnknownRun(t *testing.T) {
	f := newFake(t, "tok") // no events script: the route answers 404
	r := loggedIn(t, f).run("", "migrate", "watch", "run_x")
	if r.code != ExitError || !strings.Contains(r.stderr, "run_not_found") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestStartWithWatch(t *testing.T) {
	f := newFake(t, "tok")
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		s := sse{w}
		s.snapshot(fixtureRun(apiv1.RunStatusRunning))
		s.snapshot(fixtureRun(apiv1.RunStatusSucceeded))
	}
	r := loggedIn(t, f).run("", "migrate", "start", "pln_1", "--watch").ok(t)
	if !strings.Contains(r.stdout, "Started run run_1") || !strings.Contains(r.stdout, "[ok] succeeded, phase done") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestFrame(t *testing.T) {
	run := fixtureRun(apiv1.RunStatusRunning)
	run.Canary = &apiv1.CanaryResult{Options: apiv1.CanaryOptions{Weights: []int{10, 50}, StepDuration: ptr("20s"),
		Gate: &apiv1.GateThresholds{MaxErrorRate: ptr[float32](0.01), MaxP95Millis: ptr(800), MinRequests: ptr(20)}}}
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: &run})
	ev := func(seq int64, kind, step, msg string, data map[string]any) {
		e := apiv1.RunEvent{Seq: seq, Time: t0.Add(time.Duration(seq) * time.Second), RunId: "run_1", Kind: kind, Step: &step, Message: &msg}
		if data != nil {
			e.Data = &data
		}
		e = normalize(t, e) // numbers in data become float64, as from the wire
		v.apply(client.Message{Name: kind, Seq: seq, Event: &e})
	}
	ev(1, kindStepStarted, "prepare", "", nil)
	ev(2, kindStepSucceeded, "prepare", "", map[string]any{"durationMillis": 1000})
	ev(3, kindStepStarted, "presync", "", nil)
	ev(4, kindMetric, "presync", "replication lag", map[string]any{"lagBytes": 1536})
	ev(5, kindCanaryStep, "", "10% of reads to eks", map[string]any{"weight": 10})
	ev(6, kindGateResult, "", "gate passed", map[string]any{"weight": 10, "gate": map[string]any{"passed": true, "reason": "ok", "requests": 230, "errorRate": 0, "p95Millis": 120}})
	ev(7, kindCanaryStep, "", "50% of reads to eks", map[string]any{"weight": 50})
	for seq := int64(8); seq <= 20; seq++ {
		ev(seq, kindLog, "presync", fmt.Sprintf("copied batch %d", seq), nil)
	}
	ev(21, kindStatusChanged, "", "awaiting_switch", map[string]any{"status": "awaiting_switch", "phase": "awaiting_switch"})

	now := t0.Add(90 * time.Second)
	out := v.frame(now, 0, 0)
	for _, want := range []string{
		"Run run_1  gke -> eks  plan pln_1",
		"Status [..] awaiting_switch  phase awaiting_switch  elapsed 1m30s",
		"prepare     [ok] succeeded  1.0s",
		"presync     [..] running    1m27s",
		"finalize    [ ] pending",
		"Replication lag:  1.5 KiB at " + clock(t0.Add(4*time.Second)),
		"Canary 10%, 50% of reads, 20s per step; gate error rate <= 1.00%, p95 <= 800ms, at least 20 requests",
		"[ok] passed, 230 requests, 0.00% errors, p95 120ms",
		"[..] measuring",
		switchHint("run_1"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	steps := []int{}
	for _, s := range engineSteps {
		steps = append(steps, strings.Index(out, "\n"+s+" "))
	}
	for i := 1; i < len(steps); i++ {
		if steps[i-1] < 0 || steps[i] <= steps[i-1] {
			t.Fatalf("steps out of order %v:\n%s", steps, out)
		}
	}
	if got := strings.Count(out, "copied batch"); got != recentLines-1 {
		t.Errorf("recent events %d, want %d", got, recentLines-1)
	}

	// A short terminal drops recent events first and keeps the hint.
	small := v.frame(now, 60, 30)
	if n := strings.Count(small, "\n"); n > 30 {
		t.Errorf("frame has %d lines for a 30-line limit", n)
	}
	for _, l := range strings.Split(small, "\n") {
		if len(l) > 59 {
			t.Errorf("line longer than the terminal: %q", l)
		}
	}
	if !strings.Contains(small, switchHint("run_1")) {
		t.Errorf("hint dropped:\n%s", small)
	}
}

// A small canary step with too few requests is inconclusive, not a failure:
// the platform moved on to the next step (platform ADR 0005).
func TestFrameInconclusiveCanaryStep(t *testing.T) {
	run := fixtureRun(apiv1.RunStatusRunning)
	run.Canary = &apiv1.CanaryResult{Options: apiv1.CanaryOptions{Weights: []int{1, 10, 25, 50, 75, 90, 100}, StepDuration: ptr("10s")}}
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: &run})
	ev := func(seq int64, kind, msg string, data map[string]any) {
		step := ""
		e := apiv1.RunEvent{Seq: seq, Time: tsec(int(seq)), RunId: "run_1", Kind: kind, Step: &step, Message: &msg, Data: &data}
		e = normalize(t, e)
		v.apply(client.Message{Name: kind, Seq: seq, Event: &e})
	}
	ev(1, kindCanaryStep, "1% of reads to eks", map[string]any{"weight": 1})
	ev(2, kindGateResult, "gate at 1% inconclusive", map[string]any{"weight": 1, "verdict": "inconclusive",
		"gate": map[string]any{"passed": false, "reason": "only 2 requests reached the target (need 20); continuing to the next step", "requests": 2, "errorRate": 0, "p95Millis": 3}})
	ev(3, kindCanaryStep, "10% of reads to eks", map[string]any{"weight": 10})
	ev(4, kindGateResult, "gate at 10% passed", map[string]any{"weight": 10, "verdict": "ok",
		"gate": map[string]any{"passed": true, "reason": "33 requests", "requests": 33, "errorRate": 0, "p95Millis": 3}})

	out := v.frame(tsec(30), 0, 0)
	for _, want := range []string{
		"Canary 1%, 10%, 25%, 50%, 75%, 90%, 100% of reads, 10s per step",
		"[..] inconclusive (only 2 requests reached the target (need 20); continuing to the next step), 2 requests",
		"[ok] passed, 33 requests",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[x]") {
		t.Errorf("an inconclusive step is shown as failed:\n%s", out)
	}

	// The same from a snapshot (e.g. after a reconnect), and a server without
	// verdict still shows a failed gate as failed.
	inc, failed := apiv1.CanaryStepVerdictInconclusive, apiv1.GateResult{Passed: false, Reason: "error rate 50.00% is above 1.00%", Requests: 40, ErrorRate: 0.5}
	if got := gateText(apiv1.CanaryStep{Weight: 1, Verdict: &inc, Gate: &apiv1.GateResult{Reason: "only 0 requests"}}); !strings.HasPrefix(got, "[..] inconclusive") {
		t.Errorf("snapshot inconclusive: %q", got)
	}
	if got := gateText(apiv1.CanaryStep{Weight: 10, Gate: &failed}); !strings.HasPrefix(got, "[x] error rate 50.00%") {
		t.Errorf("no verdict, failed gate: %q", got)
	}
}

func tsec(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

// feed applies one event at t0+sec seconds, with sec as its seq.
func feed(t *testing.T, v *runView, sec int, kind, step string, data map[string]any) {
	t.Helper()
	e := apiv1.RunEvent{Seq: int64(sec), Time: tsec(sec), RunId: "run_1", Kind: kind}
	if step != "" {
		e.Step = &step
	}
	if data != nil {
		e.Data = &data
	}
	e = normalize(t, e) // numbers in data become float64, as from the wire
	v.apply(client.Message{Name: kind, Seq: e.Seq, Event: &e})
}

func weight(w int) map[string]any { return map[string]any{"weight": w} }

func gateData(w int, passed bool) map[string]any {
	return map[string]any{"weight": w, "gate": map[string]any{"passed": passed, "reason": "r", "requests": 230, "errorRate": 0, "p95Millis": 120}}
}

func status(s apiv1.RunStatus, p apiv1.RunPhase) map[string]any {
	return map[string]any{"status": string(s), "phase": string(p)}
}

// canaryRows lists the view's canary rows as weight@start-end, in seconds
// after t0, with the gate verdict.
func canaryRows(v *runView) string {
	var rows []string
	for _, s := range v.canary {
		r := fmt.Sprintf("%d%%@%g-", s.Weight, s.StartedAt.Sub(t0).Seconds())
		if s.EndedAt != nil {
			r += fmt.Sprintf("%g", s.EndedAt.Sub(t0).Seconds())
		}
		if s.Gate != nil {
			r += map[bool]string{true: " [ok]", false: " [x]"}[s.Gate.Passed]
		}
		rows = append(rows, r)
	}
	return strings.Join(rows, ", ")
}

func canaryRun(status apiv1.RunStatus, phase apiv1.RunPhase, updated int, steps ...apiv1.CanaryStep) *apiv1.Run {
	r := fixtureRun(status)
	r.Phase, r.UpdatedAt = phase, tsec(updated)
	r.Canary = &apiv1.CanaryResult{Options: apiv1.CanaryOptions{Weights: []int{10, 50}}, Steps: append([]apiv1.CanaryStep{}, steps...)}
	return &r
}

func doneStep(w, start, end int) apiv1.CanaryStep {
	return apiv1.CanaryStep{Weight: w, StartedAt: tsec(start), EndedAt: ptr(tsec(end)), Gate: &apiv1.GateResult{Passed: true, Reason: "r"}}
}

// The first snapshot already shows every stored event, and the server then
// replays them. They once added a second row per canary weight.
func TestViewSkipsEventsTheSnapshotShows(t *testing.T) {
	run := canaryRun(apiv1.RunStatusAwaitingSwitch, apiv1.RunPhaseAwaitingSwitch, 50, doneStep(10, 5, 25), doneStep(50, 26, 46))
	run.Engine.Steps["presync"] = apiv1.StepRecord{Status: apiv1.StepStatusSucceeded, StartedAt: ptr(tsec(1)), EndedAt: ptr(tsec(4))}
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: run})
	feed(t, v, 1, kindStepStarted, "presync", nil)
	feed(t, v, 4, kindStepSucceeded, "presync", map[string]any{"durationMillis": 3000})
	feed(t, v, 5, kindCanaryStep, "", weight(10))
	feed(t, v, 25, kindGateResult, "", gateData(10, false)) // differs from the snapshot, which wins
	feed(t, v, 26, kindCanaryStep, "", weight(50))
	feed(t, v, 46, kindGateResult, "", gateData(50, true))
	feed(t, v, 47, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseCanary))
	feed(t, v, 48, kindMetric, "delta", map[string]any{"lagBytes": 512, "downtimeMillis": 9000})
	feed(t, v, 50, kindStatusChanged, "", status(apiv1.RunStatusAwaitingSwitch, apiv1.RunPhaseAwaitingSwitch))

	if got, want := canaryRows(v), "10%@5-25 [ok], 50%@26-46 [ok]"; got != want {
		t.Errorf("canary rows %q, want %q", got, want)
	}
	if v.run.Status != apiv1.RunStatusAwaitingSwitch || v.run.Result.DowntimeMillis != nil {
		t.Errorf("replayed events changed the run: %s, downtime %v", v.run.Status, v.run.Result.DowntimeMillis)
	}
	out := v.frame(tsec(60), 0, 0)
	for _, want := range []string{
		"presync     [ok] succeeded  3.0s",
		"Replication lag:  512 B at " + clock(tsec(48)), // the snapshot has no lag
		"[ok] presync succeeded in 3.0s",                // replayed events are still listed
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}

	// Events after the snapshot apply.
	feed(t, v, 61, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseCutover))
	feed(t, v, 62, kindStepStarted, "fence", nil)
	if v.run.Phase != apiv1.RunPhaseCutover || v.steps["fence"].Status != apiv1.StepStatusRunning {
		t.Errorf("live events not applied: phase %s, fence %+v", v.run.Phase, v.steps["fence"])
	}
}

// The server records a canary step only once its gate has run, so the step
// being measured comes from its replayed canary_step.
func TestViewRebuildsMeasuredCanaryStep(t *testing.T) {
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: canaryRun(apiv1.RunStatusRunning, apiv1.RunPhaseCanary, 30, doneStep(10, 5, 25))})
	feed(t, v, 5, kindCanaryStep, "", weight(10))
	feed(t, v, 25, kindGateResult, "", gateData(10, true))
	feed(t, v, 26, kindCanaryStep, "", weight(50))
	if got, want := canaryRows(v), "10%@5-25 [ok], 50%@26-"; got != want {
		t.Fatalf("canary rows %q, want %q", got, want)
	}
	// A reconnect snapshot lacks the step too, and nothing up to seq 26 is
	// replayed again.
	v.apply(client.Message{Name: "snapshot", Snapshot: canaryRun(apiv1.RunStatusRunning, apiv1.RunPhaseCanary, 40, doneStep(10, 5, 25))})
	feed(t, v, 46, kindGateResult, "", gateData(50, true))
	feed(t, v, 46, kindGateResult, "", gateData(50, true))
	if got, want := canaryRows(v), "10%@5-25 [ok], 50%@26-46 [ok]"; got != want {
		t.Errorf("canary rows %q, want %q", got, want)
	}
}

func TestViewCanaryAttempts(t *testing.T) {
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: canaryRun(apiv1.RunStatusRunning, apiv1.RunPhaseCanary, 20)})
	// Replayed: an aborted attempt, then the resumed one, which is measuring
	// 10%. The snapshot holds only the current attempt's finished steps.
	feed(t, v, 1, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseCanary))
	feed(t, v, 2, kindCanaryStep, "", weight(10))
	feed(t, v, 3, kindGateResult, "", gateData(10, true))
	feed(t, v, 4, kindCanaryStep, "", weight(50))
	feed(t, v, 5, kindStatusChanged, "", status(apiv1.RunStatusAborted, apiv1.RunPhaseDone))
	feed(t, v, 6, kindCanaryStep, "", weight(0))
	feed(t, v, 10, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseSync))
	feed(t, v, 11, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseCanary))
	feed(t, v, 12, kindCanaryStep, "", weight(10))
	if got, want := canaryRows(v), "10%@12-"; got != want {
		t.Fatalf("after the replay: canary rows %q, want %q", got, want)
	}

	// Live: a repeated canary_step adds no row, and a resumed attempt
	// starts new rows.
	feed(t, v, 31, kindGateResult, "", gateData(10, true))
	feed(t, v, 32, kindCanaryStep, "", weight(50))
	feed(t, v, 33, kindCanaryStep, "", weight(50))
	feed(t, v, 40, kindStatusChanged, "", status(apiv1.RunStatusAborted, apiv1.RunPhaseDone))
	feed(t, v, 41, kindCanaryStep, "", weight(0))
	feed(t, v, 50, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseSync))
	feed(t, v, 51, kindStatusChanged, "", status(apiv1.RunStatusRunning, apiv1.RunPhaseCanary))
	feed(t, v, 52, kindCanaryStep, "", weight(10))
	feed(t, v, 53, kindCanaryStep, "", weight(10))
	if got, want := canaryRows(v), "10%@12-31 [ok], 50%@32-41, 10%@52-"; got != want {
		t.Errorf("canary rows %q, want %q", got, want)
	}
}
