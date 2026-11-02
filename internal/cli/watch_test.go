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
