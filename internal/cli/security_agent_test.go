package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// secAgentServer answers /v1/security and /v1/agent/sessions the way
// api/openapi.yaml describes them (tests only).
type secAgentServer struct {
	*httptest.Server
	final apiv1.AgentSessionStatus // status of the session's final snapshot

	mu      sync.Mutex
	queries map[string]string // path -> last raw query
	lastIDs []string          // Last-Event-ID per event stream connection
}

func fixtureReport() apiv1.SecurityReport {
	gke := apiv1.SeverityCounts{Total: 1, High: 1}
	acct := apiv1.SeverityCounts{Total: 2, Critical: 1, Info: 1}
	return apiv1.SecurityReport{
		GeneratedAt: t0,
		Summary: apiv1.SecuritySummary{BySeverity: apiv1.SeverityCounts{Total: 3, Critical: 1, High: 1, Info: 1},
			ByEnvironment: map[string]apiv1.SeverityCounts{"gke": gke, "aws-account": acct, "gcp-project": {}}},
		Sources: []apiv1.SecuritySource{
			{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke", Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Findings: 1, Message: ptr("Kubernetes v1.33.2, 12 running pods checked")},
			{Name: "aws-iam", Provider: apiv1.SecurityProviderAws, Environment: "aws-account", Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Findings: 1},
			{Name: "aws-guardduty", Provider: apiv1.SecurityProviderAws, Environment: "aws-account", Status: apiv1.SecuritySourceStatusError, CheckedAt: t0, Findings: 0,
				Message: ptr("guardduty ListDetectors: access denied (AccessDeniedException); the server's AWS identity needs the SecurityAudit policy")},
			{Name: "aws-securityhub", Provider: apiv1.SecurityProviderAws, Environment: "aws-account", Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Findings: 1},
			{Name: "gcp-scc", Provider: apiv1.SecurityProviderGcp, Environment: "gcp-project", Status: apiv1.SecuritySourceStatusNotConfigured, CheckedAt: t0, Message: ptr("Security Command Center is not connected")},
		},
		Findings: []apiv1.SecurityFinding{
			{Id: "sf_1", Environment: "aws-account", Provider: apiv1.SecurityProviderAws, Source: "aws-iam", Check: "aws.root-mfa", Category: apiv1.SecurityCategoryIdentity,
				Severity: apiv1.SecuritySeverityCritical, Title: "Root user has no MFA", Resource: "arn:aws:iam::123456789012:root", Detail: "d", Recommendation: "r", ObservedAt: t0},
			{Id: "sf_2", Environment: "gke", Provider: apiv1.SecurityProviderGcp, Source: "kubernetes", Check: "k8s.privileged", Category: apiv1.SecurityCategoryWorkload,
				Severity: apiv1.SecuritySeverityHigh, Title: "Privileged container", Resource: "shop/Deployment/web/app", Detail: "d", Recommendation: "r", ObservedAt: t0},
			{Id: "sf_3", Environment: "aws-account", Provider: apiv1.SecurityProviderAws, Source: "aws-securityhub", Check: "aws.securityhub-enabled", Category: apiv1.SecurityCategoryLogging,
				Severity: apiv1.SecuritySeverityInfo, Title: "Security Hub is not enabled", Resource: "securityhub/ap-northeast-2", Detail: "d", Recommendation: "r", ObservedAt: t0},
		},
	}
}

func fixtureSession(status apiv1.AgentSessionStatus) apiv1.AgentSession {
	s := apiv1.AgentSession{Id: "ags_1", Kind: apiv1.AgentSessionKindPlan, Title: "plan gke -> eks", Actor: "planner", Model: ptr("claude-test"),
		PlanRef: ptr("pln_1"), Status: status, CreatedAt: t0,
		Totals: apiv1.AgentSessionTotals{Events: 3, ToolCalls: 1, InputTokens: 1200, OutputTokens: 90, DurationMillis: 250}}
	if status != apiv1.AgentSessionStatusRunning {
		end := t0.Add(65 * time.Second)
		s.EndedAt, s.Summary = &end, ptr("submitted pln_1")
	}
	return s
}

func fixtureAgentEvents() []apiv1.AgentEvent {
	var input, output any = map[string]any{"env": "gke", "api_key": "[redacted]"}, "403 forbidden: role agent may not approve"
	return []apiv1.AgentEvent{
		{Seq: 1, SessionId: "ags_1", Time: t0.Add(time.Second), Type: apiv1.AgentEventTypeToolCall, Tool: ptr("list_envs"), Input: input,
			DurationMillis: ptr[int64](250), Usage: &apiv1.TokenUsage{InputTokens: 1200, OutputTokens: 90}},
		{Seq: 2, SessionId: "ags_1", Time: t0.Add(2 * time.Second), Type: apiv1.AgentEventTypeToolResult, Tool: ptr("list_envs"), Status: ptr("ok"),
			Output: []any{map[string]any{"name": "gke", "reachable": true}}},
		{Seq: 3, SessionId: "ags_1", Time: t0.Add(3 * time.Second), Type: apiv1.AgentEventTypeBlocked, Tool: ptr("approve_plan"), Output: output},
	}
}

func newSecAgentServer(t *testing.T) *secAgentServer {
	t.Helper()
	s := &secAgentServer{final: apiv1.AgentSessionStatusSucceeded, queries: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/security", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, fixtureReport()) })
	mux.HandleFunc("GET /v1/agent/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.AgentSessionList{Sessions: []apiv1.AgentSession{fixtureSession(apiv1.AgentSessionStatusSucceeded)}})
	})
	mux.HandleFunc("GET /v1/agent/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "ags_1" {
			writeError(w, 404, "agent_session_not_found", "agent session not found")
			return
		}
		writeJSON(w, 200, fixtureSession(apiv1.AgentSessionStatusSucceeded))
	})
	mux.HandleFunc("GET /v1/agent/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "ags_1" {
			writeError(w, 404, "agent_session_not_found", "agent session not found")
			return
		}
		if r.URL.Query().Get("format") == "json" {
			writeJSON(w, 200, apiv1.AgentEventList{Events: fixtureAgentEvents()})
			return
		}
		s.mu.Lock()
		conn := len(s.lastIDs)
		s.lastIDs = append(s.lastIDs, r.Header.Get("Last-Event-ID"))
		final := s.final
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(name string, seq int64, v any) {
			b, _ := json.Marshal(v)
			if seq > 0 {
				fmt.Fprintf(w, "id: %d\n", seq)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
			w.(http.Flusher).Flush()
		}
		evs := fixtureAgentEvents()
		send("snapshot", 0, fixtureSession(apiv1.AgentSessionStatusRunning))
		switch conn {
		case 0: // drops after two events
			send(string(evs[0].Type), 1, evs[0])
			send(string(evs[1].Type), 2, evs[1])
			panic(http.ErrAbortHandler)
		default: // replays one too many, then the session finishes
			send(string(evs[1].Type), 2, evs[1])
			send(string(evs[2].Type), 3, evs[2])
			if final == "" {
				<-r.Context().Done() // still running
				return
			}
			send("snapshot", 0, fixtureSession(final))
		}
	})
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			writeError(w, 401, "unauthorized", "missing or invalid bearer token")
			return
		}
		s.mu.Lock()
		s.queries[r.URL.Path] = r.URL.RawQuery
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *secAgentServer) query(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[path]
}

func (s *secAgentServer) harness(t *testing.T) *harness {
	return newHarness(t, map[string]string{"CHARLIE_SERVER": s.URL, "CHARLIE_TOKEN": "tok"})
}

func TestSecurityCommand(t *testing.T) {
	s := newSecAgentServer(t)
	h := s.harness(t)

	out := h.run("", "security").ok(t).stdout
	for _, want := range []string{
		"3 findings (1 critical, 1 high, 0 medium, 0 low, 1 info)",
		"aws-account: 1 critical, 0 high, 0 medium, 0 low, 1 info",
		"gke: 0 critical, 1 high, 0 medium, 0 low, 0 info",
		"[ ] not_configured", "[x] error", "SecurityAudit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("security lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "aws-account:") < strings.Index(out, "gke:") {
		t.Errorf("environments not in source order:\n%s", out)
	}
	// Each table has its own header; check each one on its own.
	_, acct, _ := strings.Cut(out, "\naws-account: ")
	aligned(t, acct, "RESOURCE", "Root user has no MFA", "arn:aws:iam::123456789012:root")
	_, sources, _ := strings.Cut(out, "\nSOURCE ")
	sources = "SOURCE " + sources
	aligned(t, sources, "STATUS", "aws-guardduty", "[x] error")
	aligned(t, sources, "FINDINGS", "kubernetes ", "1")
	if q := s.query("/v1/security"); q != "" {
		t.Errorf("unfiltered query = %q", q)
	}

	h.run("", "security", "--environment", "gke", "--severity", "high", "--refresh").ok(t)
	if q := s.query("/v1/security"); q != "environment=gke&refresh=true&severity=high" {
		t.Errorf("filtered query = %q", q)
	}
	if r := h.run("", "security", "--severity", "urgent"); r.code != ExitUsage || !strings.Contains(r.stderr, "--severity must be") {
		t.Errorf("bad severity: exit %d, %q", r.code, r.stderr)
	}

	var rep apiv1.SecurityReport
	out = h.run("", "--json", "security").ok(t).stdout
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Findings) != 3 || rep.Sources[2].Status != apiv1.SecuritySourceStatusError {
		t.Errorf("--json: %v\n%s", err, out)
	}
}

func TestAgentListAndShow(t *testing.T) {
	s := newSecAgentServer(t)
	h := s.harness(t)

	out := h.run("", "agent", "list", "--plan", "pln_1", "--run", "run_1", "--limit", "5").ok(t).stdout
	if q := s.query("/v1/agent/sessions"); q != "limit=5&planRef=pln_1&runId=run_1" {
		t.Errorf("list query = %q", q)
	}
	aligned(t, out, "STATUS", "ags_1", "[ok] succeeded")
	aligned(t, out, "TOKENS", "ags_1", "1200/90")
	aligned(t, out, "DURATION", "ags_1", "1m05s")
	if r := h.run("", "agent", "list", "--limit", "0"); r.code != ExitUsage {
		t.Errorf("--limit 0: exit %d", r.code)
	}

	out = h.run("", "agent", "show", "ags_1").ok(t).stdout
	for _, want := range []string{"plan gke -> eks", "submitted pln_1", "3 events, 1 tool calls, 1200/90 tokens, 250ms in tools, 1m05s in all",
		`{"api_key":"[redacted]","env":"gke"}`, "[x] blocked", "403 forbidden: role agent may not approve"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	aligned(t, out, "TOOL", "[ok] tool_result", "list_envs")
	if q := s.query("/v1/agent/sessions/ags_1/events"); q != "format=json" {
		t.Errorf("events query = %q", q)
	}

	var d struct {
		Session apiv1.AgentSession `json:"session"`
		Events  []apiv1.AgentEvent `json:"events"`
	}
	out = h.run("", "--json", "agent", "show", "ags_1").ok(t).stdout
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Session.Id != "ags_1" || len(d.Events) != 3 {
		t.Errorf("show --json: %v\n%s", err, out)
	}
	if r := h.run("", "agent", "show", "ags_x"); r.code != ExitError || !strings.Contains(r.stderr, "agent_session_not_found") {
		t.Errorf("unknown session: exit %d, %q", r.code, r.stderr)
	}
}

func TestAgentWatch(t *testing.T) {
	s := newSecAgentServer(t)
	r := s.harness(t).run("", "agent", "watch", "ags_1").ok(t)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	want := []string{
		`Agent session ags_1  plan  "plan gke -> eks"  by planner (claude-test): [..] running`,
		`[..] tool_call list_envs 250ms {"api_key":"[redacted]","env":"gke"} (1200/90 tokens)`,
		`[ok] tool_result list_envs ok [{"name":"gke","reachable":true}]`,
		`reconnected: [..] running`,
		`[x] blocked approve_plan 403 forbidden: role agent may not approve`,
		``,
		`Agent session ags_1 ended [ok] succeeded: submitted pln_1`,
		`3 events, 1 tool calls, 1200/90 tokens, 250ms in tools, 1m05s in all`,
	}
	if len(lines) != len(want) {
		t.Fatalf("watch printed:\n%s", r.stdout)
	}
	for i, w := range want {
		if !strings.HasSuffix(lines[i], w) {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], w)
		}
	}
	if fmt.Sprint(s.lastIDs) != "[ 2]" {
		t.Errorf("Last-Event-ID per connection = %q", s.lastIDs)
	}

	s = newSecAgentServer(t)
	s.final = apiv1.AgentSessionStatusFailed
	r = s.harness(t).run("", "--json", "agent", "watch", "ags_1")
	if r.code != ExitRunFailed || !strings.Contains(r.stderr, "agent session ags_1 ended failed: submitted pln_1") {
		t.Errorf("failed session: exit %d, %q", r.code, r.stderr)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		var m struct {
			Event string `json:"event"`
			ID    int64  `json:"id"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		names = append(names, fmt.Sprintf("%s:%d", m.Event, m.ID))
	}
	if got := strings.Join(names, ","); got != "snapshot:0,tool_call:1,tool_result:2,snapshot:0,blocked:3,snapshot:0" {
		t.Errorf("--json messages = %s", got)
	}

	s = newSecAgentServer(t)
	s.final = "" // never finishes
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r = s.harness(t).runCtx(ctx, "", "agent", "watch", "ags_1")
	if r.code != ExitInterrupted || !strings.Contains(r.stderr, "Reattach with: charlie agent watch ags_1") {
		t.Errorf("interrupted: exit %d, %q", r.code, r.stderr)
	}
	if r := s.harness(t).run("", "agent", "watch", "ags_x"); r.code != ExitError || !strings.Contains(r.stderr, "agent_session_not_found") {
		t.Errorf("unknown session: exit %d, %q", r.code, r.stderr)
	}
}
