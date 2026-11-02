package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

// engineSteps is the engine's step order (migration-engine/pkg/engine.Steps).
var engineSteps = []string{"precheck", "prepare", "presync", "delta", "fence", "switchover", "cutover", "verify", "finalize"}

// recentLines is how many event lines the live view keeps.
const recentLines = 8

// Event kinds the view understands.
const (
	kindStepStarted   = "step_started"
	kindStepSucceeded = "step_succeeded"
	kindStepFailed    = "step_failed"
	kindStepSkipped   = "step_skipped"
	kindMetric        = "metric"
	kindDone          = "done"
	kindStatusChanged = "status_changed"
	kindCanaryStep    = "canary_step"
	kindGateResult    = "gate_result"
	// After the cutover (Run.observe): one per gate evaluation, and the
	// automatic rollback it started, if any.
	kindObserveStep     = "observe_step"
	kindObserveRollback = "observe_rollback"
)

func switchHint(runID string) string {
	return fmt.Sprintf("run \"charlie migrate switch %s\" to cut over", runID)
}

// runView is the latest snapshot patched by the events after it. The server
// sends snapshots only at the start and the end of a stream, so live progress
// comes from the events.
//
// A snapshot already reflects every event up to its updatedAt, and the server
// replays those events right after it. Such a stale event changes only what
// the snapshot lacks: the replication lag, and the canary step being measured
// (the server records a step once its gate has run).
type runView struct {
	run    apiv1.Run
	have   bool
	snapAt time.Time // updatedAt of the latest snapshot
	steps  map[string]apiv1.StepRecord
	// canary holds one row per weight and attempt. The first snapRows rows
	// came from the latest snapshot; the current attempt starts at attempt.
	canary            []apiv1.CanaryStep
	snapRows, attempt int
	// observe holds the observe steps: the latest snapshot's, then live ones.
	observe []apiv1.ObserveStep
	lag     *int64
	lagAt   time.Time
	recent  []string
}

func newRunView() *runView { return &runView{steps: map[string]apiv1.StepRecord{}} }

func (v *runView) apply(m client.Message) {
	switch {
	case m.Snapshot != nil:
		v.snapshot(m.Snapshot)
	case m.Event != nil:
		v.event(m.Event)
	}
}

func (v *runView) snapshot(r *apiv1.Run) {
	var open *apiv1.CanaryStep
	if n := len(v.canary); n > 0 && v.canary[n-1].EndedAt == nil {
		open = &v.canary[n-1]
	}
	v.run, v.have, v.snapAt = *r, true, r.UpdatedAt
	v.steps = make(map[string]apiv1.StepRecord, len(r.Engine.Steps))
	for k, s := range r.Engine.Steps {
		v.steps[k] = s
	}
	var rows []apiv1.CanaryStep
	if r.Canary != nil {
		rows = append(rows, r.Canary.Steps...)
	}
	v.canary, v.snapRows, v.attempt = rows, len(rows), 0
	v.observe = nil
	if r.Observe != nil {
		v.observe = append(v.observe, r.Observe.Steps...)
	}
	// A reconnect snapshot lacks the step being measured, and its
	// canary_step is not replayed.
	if open != nil && v.measuring() && v.row(open.Weight) < 0 {
		v.canary = append(v.canary, *open)
	}
}

// measuring reports whether the run is in a canary step.
func (v *runView) measuring() bool {
	return v.run.Status == apiv1.RunStatusRunning && v.run.Phase == apiv1.RunPhaseCanary
}

// row is the index of the current attempt's canary row for weight w, or -1.
func (v *runView) row(w int) int {
	for i := v.attempt; i < len(v.canary); i++ {
		if v.canary[i].Weight == w {
			return i
		}
	}
	return -1
}

func (v *runView) event(e *apiv1.RunEvent) {
	v.recent = append(v.recent, eventLine(e))
	if len(v.recent) > recentLines {
		v.recent = v.recent[len(v.recent)-recentLines:]
	}
	data := map[string]any{}
	if e.Data != nil {
		data = *e.Data
	}
	if n, ok := num(data, "lagBytes"); ok && (e.Kind == kindMetric || e.Kind == kindDone) {
		lag := int64(n)
		v.lag, v.lagAt = &lag, e.Time
	}
	if e.Time.After(v.snapAt) {
		v.liveEvent(e, data)
	} else {
		v.staleEvent(e, data)
	}
}

// staleEvent applies a replayed event that the latest snapshot already
// reflects, so it can only rebuild the canary step being measured. A
// snapshot row for its weight means the event is from this attempt; a
// provisional row (after the snapshot rows) that a later stale event
// contradicts was from an earlier attempt.
func (v *runView) staleEvent(e *apiv1.RunEvent, data map[string]any) {
	w, _ := num(data, "weight")
	switch e.Kind {
	case kindCanaryStep:
		if w > 0 && v.row(int(w)) >= 0 {
			return
		}
		v.canary = v.canary[:v.snapRows]
		if w > 0 && v.measuring() {
			v.canary = append(v.canary, apiv1.CanaryStep{Weight: int(w), StartedAt: e.Time})
		}
	case kindGateResult: // that step ended before the snapshot
		if v.row(int(w)) >= v.snapRows {
			v.canary = v.canary[:v.snapRows]
		}
	case kindStatusChanged: // a canary attempt started before the snapshot
		if p, _ := data["phase"].(string); apiv1.RunPhase(p) == apiv1.RunPhaseCanary {
			v.canary = v.canary[:v.snapRows]
		}
	}
}

// liveEvent applies an event that happened after the latest snapshot.
func (v *runView) liveEvent(e *apiv1.RunEvent, data map[string]any) {
	step := str(e.Step)
	at := e.Time
	switch e.Kind {
	case kindStepStarted:
		if step != "" {
			v.steps[step] = apiv1.StepRecord{Status: apiv1.StepStatusRunning, StartedAt: &at}
		}
	case kindStepSucceeded, kindStepFailed:
		if step == "" {
			break
		}
		rec := v.steps[step]
		rec.EndedAt = &at
		if ms, ok := num(data, "durationMillis"); ok && rec.StartedAt == nil {
			start := at.Add(-time.Duration(ms) * time.Millisecond)
			rec.StartedAt = &start
		}
		rec.Status = apiv1.StepStatusSucceeded
		if e.Kind == kindStepFailed {
			rec.Status, rec.Error = apiv1.StepStatusFailed, e.Message
		}
		v.steps[step] = rec
	case kindStepSkipped: // already succeeded in an earlier attempt
		if rec := v.steps[step]; step != "" && rec.Status != apiv1.StepStatusSucceeded {
			rec.Status = apiv1.StepStatusSucceeded
			v.steps[step] = rec
		}
	case kindMetric, kindDone:
		if n, ok := num(data, "downtimeMillis"); ok {
			d := int64(n)
			v.run.Result.DowntimeMillis = &d
		}
	case kindStatusChanged:
		if p, _ := data["phase"].(string); apiv1.RunPhase(p) == apiv1.RunPhaseCanary && v.run.Phase != apiv1.RunPhaseCanary {
			v.attempt = len(v.canary) // a new canary attempt
		}
		if s, ok := data["status"].(string); ok {
			v.run.Status = apiv1.RunStatus(s)
		}
		if p, ok := data["phase"].(string); ok {
			v.run.Phase = apiv1.RunPhase(p)
		}
		if r, ok := data["reason"].(string); ok && r != "" {
			v.run.StatusReason = &r
		}
	case kindCanaryStep:
		w, _ := num(data, "weight")
		if w > 0 && v.row(int(w)) >= 0 {
			break // replayed
		}
		if n := len(v.canary); n > 0 && v.canary[n-1].EndedAt == nil {
			v.canary[n-1].EndedAt = &at
		}
		if w > 0 {
			v.canary = append(v.canary, apiv1.CanaryStep{Weight: int(w), StartedAt: at})
		} else { // all reads went back to the source: the attempt is over
			v.attempt = len(v.canary)
		}
	case kindGateResult:
		var gate apiv1.GateResult
		w, _ := num(data, "weight")
		if !decodeAs(data["gate"], &gate) {
			break
		}
		i := v.row(int(w))
		if i < 0 { // the step failed before its canary_step, e.g. the gateway was down
			v.canary = append(v.canary, apiv1.CanaryStep{Weight: int(w), StartedAt: at})
			i = len(v.canary) - 1
		}
		v.canary[i].Gate = &gate
		if v.canary[i].EndedAt == nil {
			v.canary[i].EndedAt = &at
		}
	case kindObserveStep:
		var gate apiv1.GateResult
		verdict, _ := data["verdict"].(string)
		st := apiv1.ObserveStep{StartedAt: at, EndedAt: &at, Verdict: apiv1.ObserveStepVerdict(verdict)}
		if decodeAs(data["gate"], &gate) {
			st.Gate = &gate
		}
		obs := v.observeResult()
		if d := obs.Options.StepDuration; d != nil {
			if step, err := time.ParseDuration(*d); err == nil {
				st.StartedAt = at.Add(-step)
			}
		}
		v.observe = append(v.observe, st)
		if st.Verdict == apiv1.ObserveStepVerdictViolated {
			f, reason := false, ""
			if st.Gate != nil {
				reason = st.Gate.Reason
			}
			obs.Passed, obs.Violation = &f, &reason
		}
	case kindObserveRollback:
		obs := v.observeResult()
		if id, ok := data["rollbackRunId"].(string); ok && id != "" {
			obs.RollbackRunId = &id
		}
		if e, ok := data["error"].(string); ok && e != "" {
			obs.RollbackError = &e
		}
	}
}

// observeResult is the run's observe result, made when only events have
// told about it so far.
func (v *runView) observeResult() *apiv1.ObserveResult {
	if v.run.Observe == nil {
		v.run.Observe = &apiv1.ObserveResult{Steps: []apiv1.ObserveStep{}}
	}
	return v.run.Observe
}

// num reads a JSON number from event data.
func num(data map[string]any, key string) (float64, bool) {
	f, ok := data[key].(float64)
	return f, ok
}

// decodeAs converts a decoded JSON value into a typed one.
func decodeAs(v any, out any) bool {
	if v == nil {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && json.Unmarshal(b, out) == nil
}

// eventLine is one human line for an event.
func eventLine(e *apiv1.RunEvent) string {
	step, msg := str(e.Step), str(e.Message)
	data := map[string]any{}
	if e.Data != nil {
		data = *e.Data
	}
	var s string
	switch e.Kind {
	case kindStepStarted:
		s = "[..] " + step + " started"
	case kindStepSucceeded:
		s = "[ok] " + step + " succeeded"
		if ms, ok := num(data, "durationMillis"); ok {
			s += " in " + human(time.Duration(ms)*time.Millisecond)
		}
	case kindStepFailed:
		s = "[x] " + step + " failed: " + msg
	case kindStepSkipped:
		s = "[ok] " + step + " skipped, already succeeded"
	default:
		s = e.Kind
		if step != "" {
			s += " " + step
		}
		if msg != "" {
			s += ": " + msg
		}
		if n, ok := num(data, "lagBytes"); ok {
			s += " (" + bytesHuman(int64(n)) + ")"
		} else if n, ok := num(data, "downtimeMillis"); ok {
			s += " (" + human(time.Duration(n)*time.Millisecond) + ")"
		}
	}
	return clock(e.Time) + "  " + strings.Join(strings.Fields(s), " ")
}

func (v *runView) elapsed(now time.Time) time.Duration {
	if v.run.EndedAt != nil {
		return v.run.EndedAt.Sub(v.run.StartedAt)
	}
	return now.Sub(v.run.StartedAt)
}

// frame renders the view. now drives running durations. width and maxLines,
// when positive, make the frame fit a terminal; recent events give way first.
func (v *runView) frame(now time.Time, width, maxLines int) string {
	r := &v.run
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s  %s  plan %s\n", r.RunId, route(r.Source.Env, r.Target.Env), r.PlanRef)
	fmt.Fprintf(&b, "Status %s %s  phase %s  elapsed %s  started %s by %s\n",
		runMark(r.Status), r.Status, r.Phase, human(v.elapsed(now)), stamp(r.StartedAt), r.StartedBy)
	if reason := str(r.StatusReason); reason != "" {
		fmt.Fprintf(&b, "Reason %s\n", reason)
	}
	b.WriteString("\n")

	rows := [][]string{{"STEP", "STATUS", "DURATION", "ERROR"}}
	for _, name := range engineSteps {
		rec := v.steps[name]
		st := rec.Status
		if st == "" {
			st = apiv1.StepStatusPending
		}
		dur := ""
		if rec.StartedAt != nil {
			end := now
			if rec.EndedAt != nil {
				end = *rec.EndedAt
			}
			dur = human(end.Sub(*rec.StartedAt))
		}
		rows = append(rows, []string{name, stepMark(st) + " " + string(st), dur, str(rec.Error)})
	}
	b.WriteString(table(rows))
	b.WriteString("\n")

	res := r.Result
	lag := ""
	if v.lag != nil {
		lag = bytesHuman(*v.lag) + " at " + clock(v.lagAt)
	}
	failed := count(res.FailedRequests)
	if res.Requests != nil {
		failed = fmt.Sprintf("%s of %d requests", cell(failed), *res.Requests)
	}
	verified := ""
	if res.Verified != nil {
		verified = yesNo(*res.Verified)
	}
	b.WriteString(fields([][2]string{
		{"Downtime", millis(downtime(r))},
		{"Replication lag", lag},
		{"Failed requests", failed},
		{"Missing", fmt.Sprintf("rows %s, writes %s, files %s", cell(count(res.MissingRows)), cell(count(res.MissingWrites)), cell(count(res.MissingFiles)))},
		{"Verified", verified},
	}))

	if r.Canary != nil || len(v.canary) > 0 {
		b.WriteString("\n")
		if r.Canary != nil {
			fmt.Fprintf(&b, "Canary %s\n", canaryPlan(r.Canary))
		}
		rows := [][]string{{"CANARY", "WEIGHT", "STARTED", "DURATION", "GATE"}}
		for i, s := range v.canary {
			dur := ""
			end := now
			if s.EndedAt != nil {
				end = *s.EndedAt
			}
			dur = human(end.Sub(s.StartedAt))
			rows = append(rows, []string{fmt.Sprint(i + 1), fmt.Sprintf("%d%%", s.Weight), clock(s.StartedAt), dur, gateText(s)})
		}
		b.WriteString(table(rows))
	}

	if r.Observe != nil || len(v.observe) > 0 {
		b.WriteString("\n")
		if r.Observe != nil {
			fmt.Fprintf(&b, "Observe %s\n", observePlan(r.Observe))
		}
		rows := [][]string{{"OBSERVE", "STARTED", "ENDED", "GATE"}}
		for i, s := range v.observe {
			ended := ""
			if s.EndedAt != nil {
				ended = clock(*s.EndedAt)
			}
			rows = append(rows, []string{fmt.Sprint(i + 1), clock(s.StartedAt), ended, observeText(s)})
		}
		if len(v.observe) > 0 {
			b.WriteString(table(rows))
		} else if r.Phase == apiv1.RunPhaseObserve {
			b.WriteString("[..] first observe step running\n")
		}
	}

	tail := ""
	switch {
	case r.Status == apiv1.RunStatusAwaitingSwitch:
		tail = "\n" + switchHint(r.RunId) + "\n"
	case r.Observe != nil && r.Observe.RollbackRunId != nil:
		tail = fmt.Sprintf("\nautomatic rollback started; follow it with \"charlie migrate watch %s\"\n", *r.Observe.RollbackRunId)
	case r.Observe != nil && r.Observe.Passed != nil && !*r.Observe.Passed && r.Status == apiv1.RunStatusSucceeded:
		tail = fmt.Sprintf("\nthe target broke the gate after cutover; roll back with \"charlie migrate rollback %s\"\n", r.RunId)
	}
	recent := v.recent
	if maxLines > 0 {
		room := maxLines - strings.Count(b.String(), "\n") - strings.Count(tail, "\n") - 2
		recent = recent[len(recent)-max(0, min(room, len(recent))):]
	}
	if len(recent) > 0 {
		b.WriteString("\nRecent events\n" + strings.Join(recent, "\n") + "\n")
	}
	b.WriteString(tail)
	return fit(b.String(), width, maxLines)
}

func canaryPlan(c *apiv1.CanaryResult) string {
	o := c.Options
	ws := make([]string, len(o.Weights))
	for i, w := range o.Weights {
		ws[i] = fmt.Sprintf("%d%%", w)
	}
	s := strings.Join(ws, ", ") + " of reads"
	if o.StepDuration != nil {
		s += ", " + *o.StepDuration + " per step"
	}
	s += gatePlan(o.Gate)
	if c.Passed != nil {
		if *c.Passed {
			s += "; [ok] passed"
		} else {
			s += "; [x] failed"
		}
	}
	return s
}

// gatePlan describes gate thresholds, or nothing when the server's defaults apply.
func gatePlan(gt *apiv1.GateThresholds) string {
	if gt == nil {
		return ""
	}
	var parts []string
	if gt.MaxErrorRate != nil {
		parts = append(parts, "error rate <= "+percent(*gt.MaxErrorRate))
	}
	if gt.MaxP95Millis != nil {
		parts = append(parts, fmt.Sprintf("p95 <= %dms", *gt.MaxP95Millis))
	}
	if gt.MinRequests != nil {
		parts = append(parts, fmt.Sprintf("at least %d requests", *gt.MinRequests))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; gate " + strings.Join(parts, ", ")
}

// observePlan is the observe options and outcome on one line.
func observePlan(o *apiv1.ObserveResult) string {
	opts := o.Options
	s := "after cutover"
	if opts.Duration != nil {
		s = *opts.Duration + " " + s
	}
	if opts.StepDuration != nil {
		s += ", " + *opts.StepDuration + " per step"
	}
	s += gatePlan(opts.Gate)
	if opts.AutoRollback != nil && *opts.AutoRollback {
		s += "; automatic rollback on"
	}
	switch {
	case o.Passed != nil && *o.Passed:
		s += "; [ok] passed"
	case o.Passed != nil:
		s += "; [x] violated: " + str(o.Violation)
	case o.StoppedReason != nil:
		s += "; stopped: " + *o.StoppedReason
	}
	if o.RollbackRunId != nil {
		s += "; rollback run " + *o.RollbackRunId
	}
	if o.RollbackError != nil {
		s += "; rollback could not start: " + *o.RollbackError
	}
	return s
}

// observeText is one observe step's gate verdict.
func observeText(s apiv1.ObserveStep) string {
	if s.Gate == nil {
		return string(s.Verdict)
	}
	gt := s.Gate
	nums := fmt.Sprintf("%d requests, %s errors, p95 %s", gt.Requests, percent(gt.ErrorRate),
		human(time.Duration(float64(gt.P95Millis)*float64(time.Millisecond))))
	switch s.Verdict {
	case apiv1.ObserveStepVerdictOk:
		return "[ok] passed, " + nums
	case apiv1.ObserveStepVerdictInconclusive:
		return "[..] inconclusive (" + gt.Reason + "), " + nums
	}
	return "[x] " + gt.Reason + ", " + nums
}

func gateText(s apiv1.CanaryStep) string {
	gt := s.Gate
	if gt == nil {
		if s.EndedAt == nil {
			return "[..] measuring"
		}
		return ""
	}
	nums := fmt.Sprintf("%d requests, %s errors, p95 %s", gt.Requests, percent(gt.ErrorRate),
		human(time.Duration(float64(gt.P95Millis)*float64(time.Millisecond))))
	if gt.Passed {
		return "[ok] passed, " + nums
	}
	return "[x] " + gt.Reason + ", " + nums
}

// fit cuts lines to width-1 columns (so the terminal never wraps them) and
// the frame to maxLines lines; zero means no limit.
func fit(s string, width, maxLines int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	if width > 1 {
		for i, l := range lines {
			if r := []rune(l); len(r) > width-1 {
				lines[i] = string(r[:width-1])
			}
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// screen redraws a frame in place with ANSI cursor movement.
type screen struct {
	w     io.Writer
	lines int // height of the previous frame
}

func (s *screen) draw(frame string) {
	var b strings.Builder
	if s.lines > 0 {
		fmt.Fprintf(&b, "\r\x1b[%dA", s.lines)
	}
	b.WriteString("\x1b[J")
	b.WriteString(frame)
	s.lines = strings.Count(frame, "\n")
	_, _ = io.WriteString(s.w, b.String())
}

// jsonMessage is one line of "migrate watch --json".
type jsonMessage struct {
	Event string `json:"event"`
	ID    int64  `json:"id,omitempty"`
	Data  any    `json:"data"`
}

// watch follows a run and turns its final status into the exit code.
func (g *globals) watch(cmd *cobra.Command, c *client.Client, runID string) error {
	ctx := cmd.Context()
	v := newRunView()
	p := g.out()
	var err error
	switch {
	case g.json:
		err = c.Watch(ctx, runID, func(m client.Message) error {
			v.apply(m)
			if m.Snapshot != nil {
				return p.jsonLine(jsonMessage{Event: m.Name, Data: m.Snapshot})
			}
			return p.jsonLine(jsonMessage{Event: m.Name, ID: m.Seq, Data: m.Event})
		})
	case ansiTerminal(g.app.Stdout):
		err = g.watchScreen(cmd, c, runID, v, p)
	default:
		err = g.watchLines(cmd, c, runID, v, p)
	}
	if err != nil {
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: fmt.Sprintf(
				"interrupted; run %s keeps going on the server. Reattach with: charlie migrate watch %s", runID, runID)}
		}
		return err
	}
	if v.run.Status == apiv1.RunStatusSucceeded {
		return nil
	}
	msg := fmt.Sprintf("run %s ended %s", runID, v.run.Status)
	if reason := str(v.run.StatusReason); reason != "" {
		msg += ": " + reason
	}
	return &exitError{code: ExitRunFailed, msg: msg}
}

// watchScreen redraws the full view on every message and once a second.
func (g *globals) watchScreen(cmd *cobra.Command, c *client.Client, runID string, v *runView, p *printer) error {
	scr := &screen{w: g.app.Stdout}
	var mu sync.Mutex
	draw := func() {
		mu.Lock()
		defer mu.Unlock()
		if v.have {
			cols, rows := termSize(g.app.Stdout)
			scr.draw(p.paint(v.frame(time.Now(), cols, rows-1)))
		}
	}
	stop, ticked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ticked)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				draw()
			}
		}
	}()
	err := c.Watch(cmd.Context(), runID, func(m client.Message) error {
		mu.Lock()
		v.apply(m)
		mu.Unlock()
		draw()
		return nil
	})
	close(stop)
	<-ticked
	draw()
	return err
}

// watchLines prints one line per message, for logs and pipes, and the full
// view once the run has ended.
func (g *globals) watchLines(cmd *cobra.Command, c *client.Client, runID string, v *runView, p *printer) error {
	first, hinted := true, false
	err := c.Watch(cmd.Context(), runID, func(m client.Message) error {
		v.apply(m)
		r := &v.run
		var line string
		switch {
		case m.Snapshot != nil && first:
			line = fmt.Sprintf("Run %s  %s  plan %s: %s %s, phase %s\n", r.RunId, route(r.Source.Env, r.Target.Env), r.PlanRef, runMark(r.Status), r.Status, r.Phase)
			first = false
		case m.Snapshot != nil:
			line = fmt.Sprintf("%s  snapshot: %s %s, phase %s\n", clock(r.UpdatedAt), runMark(r.Status), r.Status, r.Phase)
		default:
			line = eventLine(m.Event) + "\n"
		}
		if r.Status == apiv1.RunStatusAwaitingSwitch {
			if !hinted {
				line += switchHint(runID) + "\n"
			}
			hinted = true
		} else {
			hinted = false
		}
		return p.text(line)
	})
	if err == nil && v.have {
		v.recent = nil // already printed line by line
		err = p.text("\n" + v.frame(time.Now(), 0, 0))
	}
	return err
}
