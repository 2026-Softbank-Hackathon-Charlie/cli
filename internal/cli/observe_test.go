package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

func TestStartWithObserve(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	body := func(args ...string) any {
		t.Helper()
		h.run("", append([]string{"migrate", "start", "pln_1", "--idempotency-key", "k"}, args...)...).ok(t)
		var got any
		_ = json.Unmarshal(f.last(t, "POST", "/v1/migrations").body, &got)
		return got
	}
	want := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	got := body("--observe", "2m", "--observe-step", "30s", "--auto-rollback", "--max-p95", "500")
	if w := want(`{"planRef":"pln_1","observe":{"duration":"2m0s","stepDuration":"30s","autoRollback":true,"gate":{"maxP95Millis":500}}}`); !reflect.DeepEqual(got, w) {
		t.Errorf("observe only: %v", got)
	}
	// The gate flags shape both phases; unset options keep the server's defaults.
	got = body("--canary", "10,50", "--observe", "90s", "--min-requests", "40")
	if w := want(`{"planRef":"pln_1","canary":{"weights":[10,50],"gate":{"minRequests":40}},"observe":{"duration":"1m30s","gate":{"minRequests":40}}}`); !reflect.DeepEqual(got, w) {
		t.Errorf("canary and observe: %v", got)
	}

	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--auto-rollback"}, "--auto-rollback needs --observe"},
		{[]string{"--observe-step", "10s"}, "--observe-step needs --observe"},
		{[]string{"--max-p95", "500"}, "--max-p95 needs --canary or --observe"},
		{[]string{"--observe", "0s"}, "--observe must be positive"},
		{[]string{"--canary-step", "5s", "--observe", "1m"}, "--canary-step needs --canary"},
	} {
		r := h.run("", append([]string{"migrate", "start", "pln_1"}, c.args...)...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("%v: exit %d, stderr %q", c.args, r.code, r.stderr)
		}
	}
}

func observeRun(auto bool) *apiv1.Run {
	r := fixtureRun(apiv1.RunStatusRunning)
	r.Phase, r.UpdatedAt = apiv1.RunPhaseObserve, tsec(0)
	r.Observe = &apiv1.ObserveResult{
		Options: apiv1.ObserveOptions{Duration: ptr("2m0s"), StepDuration: ptr("20s"), AutoRollback: ptr(auto),
			Gate: &apiv1.GateThresholds{MaxErrorRate: ptr[float32](0.01), MaxP95Millis: ptr(800), MinRequests: ptr(20)}},
		Steps: []apiv1.ObserveStep{},
	}
	return &r
}

func observeData(verdict string, passed bool, reason string, requests int) map[string]any {
	return map[string]any{"verdict": verdict, "gate": map[string]any{
		"passed": passed, "reason": reason, "requests": requests, "errorRate": 0, "p95Millis": 120}}
}

func TestFrameObserveAutoRollback(t *testing.T) {
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: observeRun(true)})
	out := v.frame(tsec(5), 0, 0)
	for _, want := range []string{
		"phase observe",
		"Observe 2m0s after cutover, 20s per step; gate error rate <= 1.00%, p95 <= 800ms, at least 20 requests; automatic rollback on",
		"[..] first observe step running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("before any step, frame lacks %q:\n%s", want, out)
		}
	}

	feed(t, v, 20, kindObserveStep, "", observeData("ok", true, "ok", 230))
	feed(t, v, 40, kindObserveStep, "", observeData("inconclusive", false, "only 3 requests", 3))
	feed(t, v, 60, kindObserveStep, "", observeData("violated", false, "p95 1.2s over 800ms", 240))
	feed(t, v, 61, kindObserveRollback, "", map[string]any{"rollbackRunId": "run_2"})
	out = v.frame(tsec(62), 0, 0)
	for _, want := range []string{
		"OBSERVE",
		"1        " + clock(tsec(0)) + "  " + clock(tsec(20)) + "  [ok] passed, 230 requests, 0.00% errors, p95 120ms",
		"[..] inconclusive (only 3 requests), 3 requests",
		"[x] p95 1.2s over 800ms, 240 requests",
		"; [x] violated: p95 1.2s over 800ms; rollback run run_2",
		`automatic rollback started; follow it with "charlie migrate watch run_2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if n := len(v.observe); n != 3 {
		t.Errorf("observe rows %d, want 3", n)
	}
}

func TestFrameObserveViolatedWithoutRollback(t *testing.T) {
	r := observeRun(false)
	r.Status, r.Phase = apiv1.RunStatusSucceeded, apiv1.RunPhaseDone
	f := false
	r.Observe.Passed, r.Observe.Violation = &f, ptr("error rate 5.00% over 1.00%")
	r.Observe.Steps = []apiv1.ObserveStep{{StartedAt: tsec(0), EndedAt: ptr(tsec(20)), Verdict: apiv1.ObserveStepVerdictViolated,
		Gate: &apiv1.GateResult{Passed: false, Reason: "error rate 5.00% over 1.00%", Requests: 300, ErrorRate: 0.05, P95Millis: 90}}}
	v := newRunView()
	v.apply(client.Message{Name: "snapshot", Snapshot: r})
	out := v.frame(tsec(30), 0, 0)
	for _, want := range []string{
		"; [x] violated: error rate 5.00% over 1.00%",
		"[x] error rate 5.00% over 1.00%, 300 requests, 5.00% errors",
		`the target broke the gate after cutover; roll back with "charlie migrate rollback run_1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "automatic rollback on") {
		t.Errorf("automatic rollback shown as on:\n%s", out)
	}
}
