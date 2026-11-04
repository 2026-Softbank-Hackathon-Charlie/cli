package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// fakeServer follows api/openapi.yaml closely enough for the CLI: bearer
// auth, Error bodies, and the SSE contract on the events route.
type fakeServer struct {
	*httptest.Server
	token string // accepted bearer token; "" accepts any

	mu     sync.Mutex
	reqs   []fakeReq
	events func(w http.ResponseWriter, r *http.Request, conn int)
	conns  int

	// Device login. Each POST /v1/auth/device starts a new one with the
	// device code fakeDeviceCode(n), the user code fakeUserCodes[n] and
	// interval (5 when 0). devicePoll answers the n-th poll for a device
	// code with "pending" (also when nil), "approved", "denied", "expired",
	// "500", "403" (a proxy's HTML page), "notoken" (200 without a token) or
	// "drop" (a broken connection). Like the server, a device code that
	// handed out its token answers expired_token from then on.
	starts     int
	interval   int
	polls      map[string]int
	exchanged  map[string]bool
	devicePoll func(deviceCode string, n int) string
}

var fakeUserCodes = []string{"BCDF-GHJK", "LMNP-QRST", "VWXZ-BCDF"}

func fakeDeviceCode(n int) string { return fmt.Sprintf("dev-secret-%d", n) }

// fakeCLIToken is the token an approved poll for deviceCode hands out.
func fakeCLIToken(deviceCode string) string { return "cli-token-for-" + deviceCode }

// fakeTokenExpiry is when every fake CLI token expires.
var fakeTokenExpiry = t0.Add(30 * 24 * time.Hour)

// pollScript answers the polls in order and repeats the last answer.
func pollScript(answers ...string) func(string, int) string {
	return func(_ string, n int) string { return answers[min(n, len(answers)-1)] }
}

type fakeReq struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func newFake(t *testing.T, token string) *fakeServer {
	t.Helper()
	f := &fakeServer{token: token, polls: map[string]int{}, exchanged: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.Principal{Name: "alice", Role: apiv1.RoleOperator})
	})
	mux.HandleFunc("POST /v1/auth/device", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n, interval := f.starts, cmp.Or(f.interval, 5)
		f.starts++
		f.mu.Unlock()
		writeJSON(w, 200, apiv1.DeviceLoginStart{DeviceCode: fakeDeviceCode(n), UserCode: fakeUserCodes[n%len(fakeUserCodes)],
			VerificationUri: "https://console.hopcharlie.dev/#device", ExpiresIn: 600, Interval: interval})
	})
	mux.HandleFunc("POST /v1/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.DeviceTokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		n, script, used := f.polls[req.DeviceCode], f.devicePoll, f.exchanged[req.DeviceCode]
		f.polls[req.DeviceCode]++
		f.mu.Unlock()
		answer := "pending"
		if script != nil {
			answer = script(req.DeviceCode, n)
		}
		if used {
			answer = "expired"
		}
		switch answer {
		case "pending":
			writeError(w, 400, "authorization_pending", "not approved yet; ask again in 5 seconds")
		case "approved", "notoken":
			f.mu.Lock()
			f.exchanged[req.DeviceCode] = true
			f.mu.Unlock()
			token := fakeCLIToken(req.DeviceCode)
			if answer == "notoken" {
				token = ""
			}
			writeJSON(w, 200, apiv1.Session{Token: token, ExpiresAt: fakeTokenExpiry,
				Principal: apiv1.Principal{Name: "alice", Role: apiv1.RoleAgent}})
		case "denied":
			writeError(w, 400, "access_denied", "an administrator denied this device login")
		case "expired":
			writeError(w, 400, "expired_token", "the device login is unknown, expired or already used; start a new one")
		case "500":
			writeError(w, 500, "internal", "internal error")
		case "403":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(403)
			fmt.Fprint(w, "<html><body>Forbidden by proxy</body></html>")
		case "drop":
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/envs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fixtureEnvs())
	})
	mux.HandleFunc("GET /v1/envs/{name}", func(w http.ResponseWriter, r *http.Request) {
		for _, e := range fixtureEnvs().Environments {
			if e.Name == r.PathValue("name") {
				writeJSON(w, 200, e)
				return
			}
		}
		writeError(w, 404, "env_not_found", "environment not found")
	})
	mux.HandleFunc("POST /v1/plans", func(w http.ResponseWriter, r *http.Request) {
		p := fixturePlan("pln_2", apiv1.PlanStatusPendingApproval)
		w.Header().Set("Location", "/v1/plans/pln_2")
		writeJSON(w, 201, p)
	})
	mux.HandleFunc("GET /v1/plans", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.PlanList{Plans: []apiv1.PlanRecord{
			fixturePlan("pln_2", apiv1.PlanStatusPendingApproval), fixturePlan("pln_1", apiv1.PlanStatusStarted)}})
	})
	mux.HandleFunc("GET /v1/plans/{ref}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("ref") {
		case "pln_1":
			writeJSON(w, 200, fixturePlan("pln_1", apiv1.PlanStatusStarted))
		case "pln_2":
			writeJSON(w, 200, fixturePendingWithAttachment())
		default:
			writeError(w, 404, "plan_not_found", "plan not found")
		}
	})
	mux.HandleFunc("POST /v1/plans/{ref}/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") == "pln_1" {
			writeError(w, 409, "plan_state_conflict", "plan is started, not pending_approval")
			return
		}
		writeJSON(w, 200, fixturePlan(r.PathValue("ref"), apiv1.PlanStatusApproved))
	})
	mux.HandleFunc("POST /v1/plans/{ref}/reject", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fixturePlan(r.PathValue("ref"), apiv1.PlanStatusRejected))
	})
	mux.HandleFunc("POST /v1/migrations", func(w http.ResponseWriter, r *http.Request) {
		resp := apiv1.StartMigrationResponse{RunId: "run_1", PlanRef: "pln_1"}
		resp.Links.Self, resp.Links.Events = "/v1/migrations/run_1", "/v1/migrations/run_1/events"
		writeJSON(w, 202, resp)
	})
	mux.HandleFunc("GET /v1/migrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.RunList{Runs: []apiv1.Run{fixtureRun(apiv1.RunStatusSucceeded), fixtureRun(apiv1.RunStatusRunning)}})
	})
	mux.HandleFunc("GET /v1/migrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fixtureRun(apiv1.RunStatusRunning))
	})
	for _, action := range []string{"switch", "abort", "resume"} {
		mux.HandleFunc("POST /v1/migrations/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, fixtureRun(apiv1.RunStatusRunning))
		})
	}
	mux.HandleFunc("GET /v1/migrations/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n, script := f.conns, f.events
		f.conns++
		f.mu.Unlock()
		if script == nil {
			writeError(w, 404, "run_not_found", "run not found")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		script(w, r, n)
	})
	mux.HandleFunc("GET /v1/traffic", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fixtureTraffic())
	})
	mux.HandleFunc("GET /v1/matrix", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.Matrix{Environments: []string{"gke", "eks"}, Routes: []apiv1.MatrixRoute{
			{Source: "gke", Target: "eks", RunId: ptr("run_1"), Status: ptr(apiv1.RunStatusSucceeded), FinishedAt: ptr(t0.Add(5 * time.Minute)),
				Result: &apiv1.RunResult{DowntimeMillis: ptr[int64](2100), FailedRequests: ptr[int64](0), MissingRows: ptr[int64](0), MissingWrites: ptr[int64](0), MissingFiles: ptr[int64](0)}},
			{Source: "eks", Target: "gke"},
		}})
	})
	mux.HandleFunc("GET /v1/audit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.AuditList{NextBefore: ptr[int64](41), Entries: []apiv1.AuditEntry{
			{Id: 42, Time: t0, Actor: "alice", Role: "operator", Action: "POST /v1/migrations", Target: "/v1/migrations", Status: 202},
			{Id: 41, Time: t0, Actor: "planner", Role: "agent", Action: "POST /v1/plans/{ref}/approve", Target: "/v1/plans/pln_1/approve", Status: 403},
		}})
	})

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		public := r.URL.Path == "/v1/auth/device" || r.URL.Path == "/v1/auth/device/token"
		if f.token != "" && !public && r.Header.Get("Authorization") != "Bearer "+f.token {
			writeError(w, 401, "unauthorized", "missing or invalid bearer token")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

// last returns the last request to path.
func (f *fakeServer) last(t *testing.T, method, path string) fakeReq {
	t.Helper()
	reqs := f.requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].method == method && reqs[i].path == path {
			return reqs[i]
		}
	}
	t.Fatalf("no %s %s request; got %v", method, path, reqs)
	return fakeReq{}
}

func (f *fakeServer) lastEventIDs() []string {
	var ids []string
	for _, r := range f.requests() {
		if strings.HasSuffix(r.path, "/events") {
			ids = append(ids, r.header.Get("Last-Event-ID"))
		}
	}
	return ids
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiv1.Error{Error: apiv1.ErrorBody{Code: code, Message: msg}})
}

// sse writes the stream the way the contract describes it.
type sse struct{ w http.ResponseWriter }

func (s sse) snapshot(r apiv1.Run) {
	b, _ := json.Marshal(r)
	fmt.Fprintf(s.w, "event: snapshot\ndata: %s\n\n", b)
	s.w.(http.Flusher).Flush()
}

func (s sse) event(seq int64, kind, step, msg string, data map[string]any) {
	e := apiv1.RunEvent{Seq: seq, Time: t0.Add(time.Duration(seq) * time.Second), RunId: "run_1", Kind: kind}
	if step != "" {
		e.Step = &step
	}
	if msg != "" {
		e.Message = &msg
	}
	if data != nil {
		e.Data = &data
	}
	b, _ := json.Marshal(e)
	fmt.Fprintf(s.w, "event: %s\nid: %d\ndata: %s\n\n", kind, seq, b)
	s.w.(http.Flusher).Flush()
}

// drop breaks the connection mid-stream.
func (s sse) drop() { panic(http.ErrAbortHandler) }

func fixtureEnvs() apiv1.EnvironmentList {
	dbs := []apiv1.DatabaseCluster{{Namespace: "shop", Name: "pg", Phase: ptr("Cluster in healthy state"), Instances: 2, ReadyInstances: 2, CurrentPrimary: ptr("pg-1")}}
	return apiv1.EnvironmentList{Environments: []apiv1.Environment{
		{Name: "gke", Provider: apiv1.ProviderGcp, DisplayName: "GCP GKE", Region: ptr("asia-northeast3"), GatewayBackend: "gke", Live: true,
			Status: apiv1.EnvironmentStatus{Reachable: true, CheckedAt: t0, KubernetesVersion: ptr("v1.31.2"),
				Nodes: &apiv1.NodeSummary{Total: 3, Ready: 3, Architectures: []string{"amd64"}}, Databases: &dbs}},
		{Name: "onprem", Provider: apiv1.ProviderOnprem, DisplayName: "On-prem k3s", GatewayBackend: "onprem",
			Status: apiv1.EnvironmentStatus{Reachable: false, CheckedAt: t0, Error: ptr("dial tcp: i/o timeout")}},
	}}
}

// fixturePendingWithAttachment is pln_2 after one manifest upload.
func fixturePendingWithAttachment() apiv1.PlanRecord {
	p := fixturePlan("pln_2", apiv1.PlanStatusPendingApproval)
	p.Revision = 2
	p.Attachments = &[]apiv1.PlanAttachment{{Name: "target.yaml", Size: 2048, Sha256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}}
	return p
}

func fixturePlan(ref string, status apiv1.PlanStatus) apiv1.PlanRecord {
	p := apiv1.PlanRecord{Ref: ref, Status: status, Revision: 1, SubmittedBy: "planner", SubmittedAt: t0,
		Summary: apiv1.PlanSummary{PlanId: "shop-gke-eks", Files: true, GatewayMode: "http", FenceGate: true, MaxDowntime: "3m",
			Source: apiv1.PlanEndpoint{Env: "gke", Namespace: "shop", Cluster: "pg", DbHost: ptr("10.0.1.5:5432")},
			Target: apiv1.PlanEndpoint{Env: "eks", Namespace: "shop", Cluster: "pg", DbHost: ptr("pg.eks.example.com:5432")}}}
	if status == apiv1.PlanStatusStarted {
		p.RunId = ptr("run_1")
	}
	return p
}

func fixtureRun(status apiv1.RunStatus) apiv1.Run {
	end := t0.Add(1200 * time.Millisecond)
	r := apiv1.Run{RunId: "run_1", PlanRef: "pln_1", PlanId: "shop-gke-eks", Status: status, Phase: apiv1.RunPhaseSync,
		Source:    apiv1.PlanEndpoint{Env: "gke", Namespace: "shop", Cluster: "pg"},
		Target:    apiv1.PlanEndpoint{Env: "eks", Namespace: "shop", Cluster: "pg"},
		StartedBy: "alice", StartedAt: t0, UpdatedAt: t0,
		Engine: apiv1.EngineState{RunId: "run_1", PlanId: "shop-gke-eks", CreatedAt: t0, UpdatedAt: t0, Outcome: apiv1.EngineOutcomeRunning,
			Steps: map[string]apiv1.StepRecord{"precheck": {Status: apiv1.StepStatusSucceeded, StartedAt: &t0, EndedAt: &end}}}}
	if status != apiv1.RunStatusRunning && status != apiv1.RunStatusAwaitingSwitch {
		done := t0.Add(5 * time.Minute)
		r.Phase, r.EndedAt = apiv1.RunPhaseDone, &done
		r.Result = apiv1.RunResult{DowntimeMillis: ptr[int64](2100), FailedRequests: ptr[int64](0), Requests: ptr[int64](1500),
			MissingRows: ptr[int64](0), MissingWrites: ptr[int64](0), MissingFiles: ptr[int64](0), Verified: ptr(status == apiv1.RunStatusSucceeded)}
		if status != apiv1.RunStatusSucceeded {
			r.StatusReason = ptr("verify found 3 missing rows")
		}
	}
	return r
}

func fixtureTraffic() apiv1.TrafficState {
	return apiv1.TrafficState{WriteBackend: "gke", ReadWeights: map[string]int{"gke": 90, "eks": 10}, MaxHold: "30s",
		InFlight: apiv1.InFlight{Reads: 3, Writes: 1}, UpdatedAt: t0,
		Backends: []apiv1.BackendState{{Name: "gke", Url: "http://gke.internal", Healthy: true}, {Name: "eks", Url: "http://eks.internal", Healthy: false, LastError: ptr("connection refused")}},
		Stats:    map[string]apiv1.BackendStats{"gke": {WindowSeconds: 60, Requests: 1200, Errors: 3, ErrorRate: 0.0025, Rps: 20, P50Millis: 12, P95Millis: 80, P99Millis: 140}},
		Totals:   map[string]apiv1.BackendTotals{"gke": {Requests: 12000, Errors: 7}}}
}

// harness runs the CLI in-process with its own config dir, environment and
// clock. Sleep returns at once and moves the clock on.
type harness struct {
	t   *testing.T
	dir string
	env map[string]string

	now     time.Time
	slept   []time.Duration
	onSleep func(n int) // called after the n-th sleep (from 1), e.g. to cancel
	http    *http.Client
}

type result struct {
	code           int
	stdout, stderr string
}

func newHarness(t *testing.T, env map[string]string) *harness {
	if env == nil {
		env = map[string]string{}
	}
	return &harness{t: t, dir: t.TempDir(), env: env, now: t0, http: http.DefaultClient}
}

func (h *harness) run(stdin string, args ...string) result {
	return h.runCtx(context.Background(), stdin, args...)
}

func (h *harness) runCtx(ctx context.Context, stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	app := &App{
		Version:    "1.2.3",
		Stdin:      strings.NewReader(stdin),
		Stdout:     &out,
		Stderr:     &errb,
		Getenv:     func(k string) string { return h.env[k] },
		ConfigDir:  func() (string, error) { return h.dir, nil },
		HTTPClient: h.http,
		Now:        func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.slept = append(h.slept, d)
			h.now = h.now.Add(d)
			if h.onSleep != nil {
				h.onSleep(len(h.slept))
			}
			return ctx.Err()
		},
	}
	code := app.Run(ctx, args)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// loggedIn is a harness whose environment points at f.
func loggedIn(t *testing.T, f *fakeServer) *harness {
	return newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_TOKEN": f.token})
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	return r
}
