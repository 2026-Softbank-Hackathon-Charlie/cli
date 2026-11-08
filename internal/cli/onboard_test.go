package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const onbID = "onb-3f9a1c2b7d4e"

// onboardFake serves /v1/onboardings with one onboarding in state, and
// records the requests.
type onboardFake struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []fakeReq
	onb    apiv1.Onboarding
	code   map[string]string // route ("approve", "start", ...) -> error code to answer with
	events func(w http.ResponseWriter)
}

var onboardErrStatus = map[string]int{
	"not_implemented": 501, "pr_head_mismatch": 409, "onboarding_state_conflict": 409,
	"onboarding_exists": 409, "onboarding_not_found": 404, "invalid_request": 400,
}

func fixtureOnboarding(state apiv1.OnboardingState) apiv1.Onboarding {
	return apiv1.Onboarding{
		Id: onbID, Repo: "o/moimroom-legacy", ServiceUrl: "https://moimroom-legacy.hopcharlie.dev", App: "moimroom-legacy",
		TargetEnv: "eks", State: state, HumanActions: []apiv1.OnboardingHumanAction{}, LegacyChanges: []apiv1.OnboardingLegacyChange{},
		Report:    apiv1.OnboardingReport{Blockers: []apiv1.OnboardingFinding{}, Warnings: []apiv1.OnboardingFinding{}},
		CreatedBy: "alice", CreatedAt: t0, UpdatedAt: t0,
	}
}

// awaitingApproval is an onboarding with everything the approval shows.
func awaitingApproval() apiv1.Onboarding {
	o := fixtureOnboarding(apiv1.OnboardingStateAwaitingApproval)
	o.Facts = &apiv1.OnboardingFactsSummary{InstanceId: ptr("i-0abc"), Region: ptr("ap-northeast-2"),
		DeployWorkflows: &[]string{".github/workflows/deploy.yml"}, DbImage: ptr("postgres:17.11-alpine")}
	o.Report = apiv1.OnboardingReport{
		Blockers:          []apiv1.OnboardingFinding{},
		Warnings:          []apiv1.OnboardingFinding{{Code: apiv1.OnboardingFindingCode("reindex_required"), Message: "2 indexes are rebuilt", Subject: ptr("public.idx_name")}},
		Drift:             &apiv1.OnboardingDrift{Head: "1111111aaaa", RunningRevision: ptr("2222222bbbb")},
		HoldBudgetSeconds: ptr(float32(40)), HoldSeconds: ptr(float32(18)),
	}
	o.Pr = &apiv1.OnboardingPR{Number: 12, Url: "https://github.com/o/moimroom-legacy/pull/12", HeadSha: "abc1234def"}
	o.Check = &apiv1.OnboardingCheck{App: onbID, Result: "passed", Attempts: 1, StartSeconds: ptr(float32(12.5))}
	o.PlannedLegacyChanges = &[]apiv1.OnboardingPlannedChange{
		{Kind: "workflow_disabled", Target: ".github/workflows/deploy.yml"},
		{Kind: "hba_line_added", Target: "pg_hba.conf", Detail: ptr("host replication hopcharlie_replicator 172.18.0.0/16 scram-sha-256")},
	}
	return o
}

func newOnboardFake(t *testing.T, o apiv1.Onboarding) *onboardFake {
	t.Helper()
	f := &onboardFake{onb: o, code: map[string]string{}}
	mux := http.NewServeMux()
	answer := func(route string, status int, next func(*apiv1.Onboarding)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if code := f.code[route]; code != "" {
				writeError(w, onboardErrStatus[code], code, "refused by the fake")
				return
			}
			if id := r.PathValue("id"); id != "" && id != f.onb.Id {
				writeError(w, 404, "onboarding_not_found", "onboarding not found")
				return
			}
			if next != nil {
				next(&f.onb)
			}
			writeJSON(w, status, f.onb)
		}
	}
	mux.HandleFunc("POST /v1/onboardings", answer("start", 202, nil))
	mux.HandleFunc("GET /v1/onboardings", func(w http.ResponseWriter, r *http.Request) {
		if code := f.code["list"]; code != "" {
			writeError(w, onboardErrStatus[code], code, "refused by the fake")
			return
		}
		writeJSON(w, 200, apiv1.OnboardingList{Onboardings: []apiv1.Onboarding{f.onb}})
	})
	mux.HandleFunc("GET /v1/onboardings/{id}", answer("get", 200, nil))
	mux.HandleFunc("DELETE /v1/onboardings/{id}", answer("delete", 202, func(o *apiv1.Onboarding) { o.Deleting = true }))
	mux.HandleFunc("POST /v1/onboardings/{id}/approve", answer("approve", 202, func(o *apiv1.Onboarding) {
		o.State, o.LegacyDeployStopped = apiv1.OnboardingStateConnecting, true
	}))
	mux.HandleFunc("POST /v1/onboardings/{id}/cutover", answer("cutover", 202, func(o *apiv1.Onboarding) {
		o.State, o.RunId = apiv1.OnboardingStateCuttingOver, ptr("run_9")
	}))
	mux.HandleFunc("POST /v1/onboardings/{id}/decommission", answer("decommission", 202, nil))
	mux.HandleFunc("POST /v1/onboardings/{id}/resume", answer("resume", 202, func(o *apiv1.Onboarding) {
		if o.FailedState != nil {
			o.State, o.FailedState = *o.FailedState, nil
		}
	}))
	mux.HandleFunc("GET /v1/onboardings/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f.events(w)
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{method: r.Method, path: r.URL.EscapedPath(), header: r.Header.Clone(), body: b})
		f.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *onboardFake) harness(t *testing.T) *harness {
	return newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_TOKEN": "tok"})
}

func (f *onboardFake) sent(method, path string) []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.reqs {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func TestOnboardStart(t *testing.T) {
	f := newOnboardFake(t, fixtureOnboarding(apiv1.OnboardingStateDiscovering))
	h := f.harness(t)
	r := h.run("", "onboard", "start", "--repo", "o/moimroom-legacy", "--url", "https://moimroom-legacy.hopcharlie.dev", "--env", "gke").ok(t)
	if got := f.sent("POST", "/v1/onboardings"); len(got) != 1 ||
		string(got[0].body) != `{"repo":"o/moimroom-legacy","serviceUrl":"https://moimroom-legacy.hopcharlie.dev","targetEnv":"gke"}` {
		t.Fatalf("requests %+v", got)
	}
	want := "Started onboarding " + onbID + " of app moimroom-legacy from o/moimroom-legacy.\nFollow it with: charlie onboard status " + onbID + " --follow\n"
	if r.stdout != want {
		t.Errorf("stdout %q", r.stdout)
	}
	for _, args := range [][]string{
		{"--repo", "o/x"},
		{"--repo", "nope", "--url", "https://x.hopcharlie.dev"},
		{"--repo", "o/x", "--url", "http://x.hopcharlie.dev"},
	} {
		if r := h.run("", append([]string{"onboard", "start"}, args...)...); r.code != ExitUsage {
			t.Errorf("%v: exit %d", args, r.code)
		}
	}
	f.code["start"] = "not_implemented"
	r = h.run("", "onboard", "start", "--repo", "o/x", "--url", "https://x.hopcharlie.dev")
	if r.code != ExitError || r.stderr != "error: not_implemented: refused by the fake (HTTP 501)\n"+
		"The server has the onboarding API but does not carry it out yet (ADR 0042 is being built).\n" {
		t.Errorf("501: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestOnboardListAndStatus(t *testing.T) {
	o := awaitingApproval()
	o.HumanActions = []apiv1.OnboardingHumanAction{
		{Kind: apiv1.OnboardingHumanActionKindInstallGithubApp, Url: ptr("https://github.com/apps/hopcharlie-platform/installations/new"), Done: true},
		{Kind: apiv1.OnboardingHumanActionKindDeleteDnsRecord, Record: &apiv1.OnboardingDNSRecord{Name: "moimroom-legacy.hopcharlie.dev", Type: "A", Value: "203.0.113.10"}},
	}
	f := newOnboardFake(t, o)
	h := f.harness(t)
	r := h.run("", "onboard", "list").ok(t)
	if !strings.Contains(r.stdout, onbID) || !strings.Contains(r.stdout, "awaiting_approval") || !strings.Contains(r.stdout, "#12") {
		t.Errorf("list:\n%s", r.stdout)
	}
	r = h.run("", "onboard", "status", onbID).ok(t)
	for _, want := range []string{
		"[ok] Install the platform's GitHub App on the repository: https://github.com/apps/hopcharlie-platform/installations/new",
		"[  ] Delete the DNS record moimroom-legacy.hopcharlie.dev A 203.0.113.10",
		"Drift: what runs differs from the default branch head 1111111; running image built from 2222222",
		"reindex_required [public.idx_name]: 2 indexes are rebuilt",
		"Contract PR #12 at abc1234: https://github.com/o/moimroom-legacy/pull/12",
		"Diff: https://github.com/o/moimroom-legacy/pull/12/files",
		"Test deployment " + onbID + ": passed (attempt 1 of 2), started in 12.5s, hold budget 40s, estimated write hold 18.0s",
		"2. hba_line_added pg_hba.conf\n     host replication hopcharlie_replicator 172.18.0.0/16 scram-sha-256",
		"Waiting for you; after doing it, check now with: charlie onboard resume " + onbID,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "deploy workflows are disabled") {
		t.Error("legacy deploy notice before approval")
	}

	f.onb.State, f.onb.LegacyDeployStopped, f.onb.HumanActions = apiv1.OnboardingStateReady, true, nil
	r = h.run("", "onboard", "status", onbID).ok(t)
	if !strings.Contains(r.stdout, legacyStoppedNotice) || !strings.Contains(r.stdout, "Cut over with: charlie onboard cutover "+onbID) ||
		strings.Contains(r.stdout, "Approving adds") {
		t.Errorf("ready:\n%s", r.stdout)
	}
	f.onb.State, f.onb.FailedState, f.onb.Failure = apiv1.OnboardingStateFailed, ptr(apiv1.OnboardingStateSyncing), ptr("streaming stopped")
	r = h.run("", "onboard", "status", onbID).ok(t)
	if !strings.Contains(r.stdout, "failed (in syncing)") || !strings.Contains(r.stdout, "streaming stopped") ||
		!strings.Contains(r.stdout, "charlie onboard resume "+onbID) {
		t.Errorf("failed:\n%s", r.stdout)
	}
	r = h.run("", "--json", "onboard", "status", onbID).ok(t)
	if got := decodeOut[apiv1.Onboarding](t, r.stdout); got.Id != onbID || got.State != apiv1.OnboardingStateFailed {
		t.Errorf("--json %+v", got)
	}
	if r := h.run("", "onboard", "status", "onb-nope"); r.code != ExitError || !strings.Contains(r.stderr, "onboarding_not_found") {
		t.Errorf("not found: exit %d, %q", r.code, r.stderr)
	}
}

func TestOnboardStatusFollow(t *testing.T) {
	f := newOnboardFake(t, fixtureOnboarding(apiv1.OnboardingStateSyncing))
	f.events = func(w http.ResponseWriter) {
		snap := func(o apiv1.Onboarding) {
			b, _ := json.Marshal(o)
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
		}
		ev := func(seq int64, kind apiv1.OnboardingEventKind, state apiv1.OnboardingState, msg string) {
			b, _ := json.Marshal(apiv1.OnboardingEvent{Seq: seq, OnboardingId: onbID, Time: t0, State: state, Kind: kind, Message: &msg})
			fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", kind, seq, b)
		}
		snap(fixtureOnboarding(apiv1.OnboardingStateSyncing))
		ev(1, apiv1.OnboardingEventKindStep, apiv1.OnboardingStateSyncing, "replication lag 0.4s")
		ready := fixtureOnboarding(apiv1.OnboardingStateDone)
		ev(2, apiv1.OnboardingEventKindState, apiv1.OnboardingStateDone, "")
		snap(ready)
	}
	r := f.harness(t).run("", "onboard", "status", onbID, "--follow").ok(t)
	for _, want := range []string{"State:        syncing", "step syncing: replication lag 0.4s", ">> done", "state done",
		"Done. Add environments with: charlie app env add moimroom-legacy ENV"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("follow lacks %q:\n%s", want, r.stdout)
		}
	}
	r = f.harness(t).run("", "--json", "onboard", "status", onbID, "--follow").ok(t)
	if n := strings.Count(r.stdout, "\n"); n != 4 || !strings.HasPrefix(r.stdout, `{"event":"snapshot"`) {
		t.Errorf("--json follow (%d lines):\n%s", n, r.stdout)
	}
}

func TestOnboardApprove(t *testing.T) {
	f := newOnboardFake(t, awaitingApproval())
	h := f.harness(t)
	r := h.run("nope\n", "onboard", "approve", onbID)
	if r.code != ExitError || !strings.Contains(r.stderr, "was not approved") || len(f.sent("POST", "/v1/onboardings/"+onbID+"/approve")) != 0 {
		t.Fatalf("wrong name: exit %d, %q", r.code, r.stderr)
	}
	for _, want := range []string{"Contract PR #12 at abc1234", "started in 12.5s, hold budget 40s",
		"1. workflow_disabled .github/workflows/deploy.yml",
		"The source's deploy workflows (.github/workflows/deploy.yml) are disabled from now"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("plan lacks %q:\n%s", want, r.stdout)
		}
	}
	r = h.run("moimroom-legacy\n", "onboard", "approve", onbID).ok(t)
	if got := f.sent("POST", "/v1/onboardings/"+onbID+"/approve"); len(got) != 1 || string(got[0].body) != `{"headSha":"abc1234def"}` {
		t.Fatalf("approve requests %+v", got)
	}
	if !strings.Contains(r.stdout, "Approved PR #12 at abc1234. Onboarding "+onbID+" is connecting.") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// Not in awaiting_approval: nothing is sent.
	r = h.run("", "onboard", "approve", onbID, "--yes")
	if r.code != ExitError || !strings.Contains(r.stderr, "it can be approved only in awaiting_approval") {
		t.Errorf("connecting: exit %d, %q", r.code, r.stderr)
	}

	f.onb = awaitingApproval()
	f.code["approve"] = "pr_head_mismatch"
	r = h.run("", "--json", "onboard", "approve", onbID, "--yes")
	if r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "pr_head_mismatch") ||
		!strings.Contains(r.stderr, "Review it again with: charlie onboard status "+onbID+", then approve again.") {
		t.Errorf("mismatch: exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestOnboardCutoverResumeDecommission(t *testing.T) {
	f := newOnboardFake(t, fixtureOnboarding(apiv1.OnboardingStateReady))
	h := f.harness(t)
	r := h.run("", "onboard", "cutover", onbID).ok(t)
	if r.stdout != "Started the cutover of app moimroom-legacy: run run_9.\nFollow it with: charlie migrate watch run_9\n" {
		t.Errorf("cutover %q", r.stdout)
	}
	f.code["cutover"] = "onboarding_state_conflict"
	if r := h.run("", "onboard", "cutover", onbID); r.code != ExitError || !strings.Contains(r.stderr, "charlie onboard status "+onbID) {
		t.Errorf("conflict %q", r.stderr)
	}

	f.onb = fixtureOnboarding(apiv1.OnboardingStateFailed)
	f.onb.FailedState = ptr(apiv1.OnboardingStateChecking)
	r = h.run("", "onboard", "resume", onbID).ok(t)
	if r.stdout != "Resumed onboarding "+onbID+": it is checking.\n" {
		t.Errorf("resume %q", r.stdout)
	}

	f.onb = fixtureOnboarding(apiv1.OnboardingStateDecommissioning)
	f.onb.Facts = &apiv1.OnboardingFactsSummary{InstanceId: ptr("i-0abc"), Region: ptr("ap-northeast-2"), Bucket: ptr("moimroom-files")}
	r = h.run("no\n", "onboard", "decommission", onbID)
	if r.code != ExitError || !strings.Contains(r.stderr, "This stops instance i-0abc in ap-northeast-2") ||
		!strings.Contains(r.stderr, "and the bucket moimroom-files stay") || len(f.sent("POST", "/v1/onboardings/"+onbID+"/decommission")) != 0 {
		t.Errorf("decommission refused: exit %d, %q", r.code, r.stderr)
	}
	h.run("moimroom-legacy\n", "onboard", "decommission", onbID).ok(t)
	h.run("", "onboard", "decommission", onbID, "--yes").ok(t)
	if n := len(f.sent("POST", "/v1/onboardings/"+onbID+"/decommission")); n != 2 {
		t.Errorf("decommission sent %d times", n)
	}
}

func TestOnboardDeletePromptByState(t *testing.T) {
	f := newOnboardFake(t, awaitingApproval())
	h := f.harness(t)
	r := h.run("x\n", "onboard", "delete", onbID)
	if r.code != ExitError || !strings.Contains(r.stderr, "PR #12 is closed. Nothing on the source was changed.") {
		t.Errorf("early: exit %d, %q", r.code, r.stderr)
	}

	f.onb = fixtureOnboarding(apiv1.OnboardingStateFailed)
	f.onb.FailedState = ptr(apiv1.OnboardingStateSyncing)
	f.onb.LegacyChanges = []apiv1.OnboardingLegacyChange{
		{Kind: "workflow_disabled", Target: ".github/workflows/deploy.yml", At: t0},
		{Kind: "tailscale_device_joined", Target: "moimroom-legacy-vm", At: t0},
	}
	r = h.run("moimroom-legacy\n", "onboard", "delete", onbID).ok(t)
	if !strings.Contains(r.stderr, "undone in reverse order:\n  - tailscale_device_joined moimroom-legacy-vm\n  - workflow_disabled .github/workflows/deploy.yml\n") ||
		!strings.Contains(r.stderr, "the registered app moimroom-legacy are deleted. A merged PR stays merged.") {
		t.Errorf("late prompt:\n%s", r.stderr)
	}
	if len(f.sent("DELETE", "/v1/onboardings/"+onbID)) != 1 || !strings.Contains(r.stdout, "Deleting onboarding "+onbID) {
		t.Errorf("stdout %q", r.stdout)
	}
	h.run("", "onboard", "delete", onbID, "--yes").ok(t)
	if n := len(f.sent("GET", "/v1/onboardings/"+onbID)); n != 2 {
		t.Errorf("--yes read the onboarding: %d GETs", n)
	}
}
