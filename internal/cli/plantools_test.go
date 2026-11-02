package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// toolsFake serves the plan tools routes of api/openapi.yaml and records requests.
type toolsFake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []fakeReq
}

func newToolsFake(t *testing.T) *toolsFake {
	t.Helper()
	f := &toolsFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/plans/draft", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.PlanDraftRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.App != "moimroom" {
			writeError(w, 400, "invalid_request", `invalid draft request: unknown app "`+req.App+`"`)
			return
		}
		writeJSON(w, 200, apiv1.EnginePlan{Id: req.App + "-" + req.Source + "-to-" + req.Target,
			Source: apiv1.EnginePlanEndpoint{Env: req.Source, Namespace: "moimroom", Cluster: "moimroom-db-" + req.Source, Dsn: "postgres://app:${GKE_MOIMROOM_PGPASSWORD}@h/app"},
			Target: apiv1.EnginePlanEndpoint{Env: req.Target, Namespace: "moimroom", Cluster: "moimroom-db-" + req.Target, Dsn: "postgres://app:${EKS_MOIMROOM_PGPASSWORD}@h/app"}})
	})
	mux.HandleFunc("POST /v1/plans", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, fixturePlan("pln_9", apiv1.PlanStatusPendingApproval))
	})
	mux.HandleFunc("PUT /v1/plans/{ref}/attachments/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "pln_9" {
			writeError(w, 409, "plan_state_conflict", "plan is not in the required status: conflict: plan is approved")
			return
		}
		b, _ := io.ReadAll(r.Body)
		writeJSON(w, 200, apiv1.PlanAttachment{Name: r.PathValue("name"), Size: int64(len(b)), Sha256: "5f2b"})
	})
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.AppList{Apps: []apiv1.App{{Name: "moimroom", Namespace: "moimroom",
			Database: apiv1.AppDatabase{Name: "app", User: "app", Schemas: []string{"app"}},
			Environments: []apiv1.AppEnvironment{
				{Env: "gke", CnpgCluster: "moimroom-db-gke", DbHost: "moimroom-db-gke-rw.moimroom.gke.ts.net:5432", Bucket: ptr("gcs:moimroom-photos-gke")},
				{Env: "onprem", CnpgCluster: "moimroom-db-onprem", DbHost: "moimroom-db-onprem-rw.moimroom.onprem.ts.net:5432"},
			}}}})
	})
	mux.HandleFunc("GET /v1/written-records/{stream}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.WrittenRecordStats{Stream: r.PathValue("stream"), Count: 1234, LatestAt: ptr(t0)})
	})
	mux.HandleFunc("GET /v1/migrations/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		run := fixtureRun(apiv1.RunStatusRolledBack)
		run.RunId = r.PathValue("id")
		sse{w}.snapshot(run)
	})
	mux.HandleFunc("POST /v1/migrations/{id}/rollback", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "run_1" {
			writeError(w, 409, "run_state_conflict", "run is running; only a succeeded run can be rolled back")
			return
		}
		resp := apiv1.StartMigrationResponse{RunId: "run_2", PlanRef: "pln_8"}
		resp.Links.Self, resp.Links.Events = "/v1/migrations/run_2", "/v1/migrations/run_2/events"
		writeJSON(w, 202, resp)
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *toolsFake) last(t *testing.T, method, path string) fakeReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			return f.reqs[i]
		}
	}
	t.Fatalf("no %s %s request", method, path)
	return fakeReq{}
}

func (f *toolsFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func toolsHarness(t *testing.T) (*toolsFake, *harness) {
	f := newToolsFake(t)
	return f, newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_TOKEN": "tok"})
}

func TestPlanNew(t *testing.T) {
	f, h := toolsHarness(t)

	r := h.run("", "plan", "new", "--app", "moimroom", "--from", "gke", "--to", "eks", "--stream", "moimroom-demo", "--no-files").ok(t)
	var draft apiv1.EnginePlan
	if err := json.Unmarshal([]byte(r.stdout), &draft); err != nil || draft.Id != "moimroom-gke-to-eks" {
		t.Fatalf("draft output: %q (%v)", r.stdout, err)
	}
	var sent map[string]any
	_ = json.Unmarshal(f.last(t, "POST", "/v1/plans/draft").body, &sent)
	if sent["app"] != "moimroom" || sent["source"] != "gke" || sent["target"] != "eks" || sent["files"] != false || sent["writtenRecordsStream"] != "moimroom-demo" {
		t.Fatalf("draft request: %v", sent)
	}
	// files is left to the server unless --no-files.
	h.run("", "plan", "new", "--app", "moimroom", "--from", "gke", "--to", "eks").ok(t)
	if body := string(f.last(t, "POST", "/v1/plans/draft").body); strings.Contains(body, "files") || strings.Contains(body, "writtenRecordsStream") {
		t.Fatalf("draft request without flags: %s", body)
	}

	out := filepath.Join(t.TempDir(), "plan.json")
	r = h.run("", "plan", "new", "--app", "moimroom", "--from", "gke", "--to", "eks", "-o", out).ok(t)
	if !strings.Contains(r.stdout, "Wrote plan moimroom-gke-to-eks to "+out) || !strings.Contains(r.stdout, "charlie plan submit -f "+out) {
		t.Fatalf("stdout: %q", r.stdout)
	}
	written, err := os.ReadFile(out)
	if err != nil || !json.Valid(written) || !strings.Contains(string(written), `"moimroom-gke-to-eks"`) {
		t.Fatalf("written draft: %q (%v)", written, err)
	}

	r = h.run("", "plan", "new", "--app", "moimroom", "--from", "gke", "--to", "eks", "--submit").ok(t)
	if !strings.Contains(r.stdout, "Submitted plan pln_9.") {
		t.Fatalf("submit stdout: %q", r.stdout)
	}
	var submitted apiv1.EnginePlan
	if err := json.Unmarshal(f.last(t, "POST", "/v1/plans").body, &submitted); err != nil || submitted.Id != "moimroom-gke-to-eks" {
		t.Fatalf("submitted body: %v", err)
	}
	r = h.run("", "--json", "plan", "new", "--app", "moimroom", "--from", "gke", "--to", "eks", "--submit").ok(t)
	if rec := decodeOut[apiv1.PlanRecord](t, r.stdout); rec.Ref != "pln_9" {
		t.Fatalf("--json submit: %q", r.stdout)
	}

	before := f.count()
	if r := h.run("", "plan", "new", "--app", "moimroom", "--from", "gke"); r.code != ExitUsage || !strings.Contains(r.stderr, "--to are required") {
		t.Fatalf("missing --to: exit %d %q", r.code, r.stderr)
	}
	if f.count() != before {
		t.Fatal("a usage error must not call the server")
	}
	if r := h.run("", "plan", "new", "--app", "shop", "--from", "gke", "--to", "eks"); r.code != ExitError || !strings.Contains(r.stderr, "invalid_request") {
		t.Fatalf("unknown app: exit %d %q", r.code, r.stderr)
	}
}

func TestPlanAttach(t *testing.T) {
	f, h := toolsHarness(t)
	file := filepath.Join(t.TempDir(), "target.yaml")
	manifest := "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\n"
	if err := os.WriteFile(file, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "plan", "attach", "pln_9", "--name", "target.yaml", "-f", file).ok(t)
	if !strings.Contains(r.stdout, "Attached target.yaml to plan pln_9 (48 B, sha256 5f2b)") || !strings.Contains(r.stdout, `"attachment:target.yaml"`) {
		t.Fatalf("stdout: %q", r.stdout)
	}
	req := f.last(t, "PUT", "/v1/plans/pln_9/attachments/target.yaml")
	if string(req.body) != manifest || req.header.Get("Content-Type") != "application/octet-stream" || req.header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("upload: %q %v", req.body, req.header)
	}
	r = h.run(manifest, "--json", "plan", "attach", "pln_9", "--name", "from-stdin.yaml", "-f", "-").ok(t)
	if a := decodeOut[apiv1.PlanAttachment](t, r.stdout); a.Name != "from-stdin.yaml" || a.Size != int64(len(manifest)) {
		t.Fatalf("--json: %q", r.stdout)
	}
	if r := h.run("", "plan", "attach", "pln_9", "-f", file); r.code != ExitUsage {
		t.Fatalf("missing --name: exit %d", r.code)
	}
	if r := h.run("", "plan", "attach", "pln_1", "--name", "target.yaml", "-f", file); r.code != ExitError || !strings.Contains(r.stderr, "plan_state_conflict") {
		t.Fatalf("approved plan: exit %d %q", r.code, r.stderr)
	}
}

func TestAppsAndWrittenRecords(t *testing.T) {
	_, h := toolsHarness(t)
	r := h.run("", "apps").ok(t)
	for _, want := range []string{"APP", "moimroom", "app@app", "moimroom-db-onprem-rw.moimroom.onprem.ts.net:5432", "gcs:moimroom-photos-gke"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("apps output lacks %q:\n%s", want, r.stdout)
		}
	}
	if lines := strings.Count(r.stdout, "\n"); lines != 3 {
		t.Errorf("apps output has %d lines, want a header and one per environment:\n%s", lines, r.stdout)
	}
	r = h.run("", "--json", "apps").ok(t)
	if l := decodeOut[apiv1.AppList](t, r.stdout); len(l.Apps) != 1 || len(l.Apps[0].Environments) != 2 {
		t.Fatalf("--json apps: %q", r.stdout)
	}

	r = h.run("", "written-records", "moimroom-demo").ok(t)
	if !strings.Contains(r.stdout, "Stream:   moimroom-demo") || !strings.Contains(r.stdout, "Records:  1234") || !strings.Contains(r.stdout, "Latest:   "+stamp(t0)) {
		t.Fatalf("written-records: %q", r.stdout)
	}
	r = h.run("", "--json", "written-records", "moimroom-demo").ok(t)
	if st := decodeOut[apiv1.WrittenRecordStats](t, r.stdout); st.Count != 1234 {
		t.Fatalf("--json written-records: %q", r.stdout)
	}
	if r := h.run("", "written-records"); r.code != ExitUsage {
		t.Fatalf("no stream: exit %d", r.code)
	}
}

func TestMigrateRollback(t *testing.T) {
	f, h := toolsHarness(t)
	r := h.run("", "migrate", "rollback", "run_1", "--reason", "eks bill too high").ok(t)
	if !strings.Contains(r.stdout, "Started rollback run run_2 of run run_1 (plan pln_8).") || !strings.Contains(r.stdout, "charlie migrate watch run_2") {
		t.Fatalf("stdout: %q", r.stdout)
	}
	// autoSwitch is left to the server's default (true) unless the flag is given.
	if body := string(f.last(t, "POST", "/v1/migrations/run_1/rollback").body); body != `{"reason":"eks bill too high"}` {
		t.Fatalf("request body: %s", body)
	}
	h.run("", "migrate", "rollback", "run_1", "--auto-switch=false").ok(t)
	if body := string(f.last(t, "POST", "/v1/migrations/run_1/rollback").body); body != `{"autoSwitch":false}` {
		t.Fatalf("request body: %s", body)
	}
	r = h.run("", "--json", "migrate", "rollback", "run_1").ok(t)
	if resp := decodeOut[apiv1.StartMigrationResponse](t, r.stdout); resp.RunId != "run_2" {
		t.Fatalf("--json: %q", r.stdout)
	}
	if r := h.run("", "migrate", "rollback", "run_3"); r.code != ExitError || !strings.Contains(r.stderr, "run_state_conflict") {
		t.Fatalf("running run: exit %d %q", r.code, r.stderr)
	}
	if r := h.run("", "migrate", "rollback"); r.code != ExitUsage {
		t.Fatalf("no run id: exit %d", r.code)
	}
}

// The rollback run is watched like any other: a failed rollback exits 3.
func TestMigrateRollbackWatch(t *testing.T) {
	f, h := toolsHarness(t)
	r := h.run("", "migrate", "rollback", "run_1", "--watch")
	if r.code != ExitRunFailed || !strings.Contains(r.stdout, "Started rollback run run_2") || !strings.Contains(r.stderr, "run run_2 ended rolled_back") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	f.last(t, "GET", "/v1/migrations/run_2/events")
}

func decodeOut[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, s)
	}
	return v
}

// The approver approves the revision and attachments the CLI printed, and
// nothing uploaded after that.
func TestPlanApproveSendsTheReviewedRevision(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	sha := fixturePendingWithAttachment().Attachments
	want := (*sha)[0].Sha256

	out := h.run("", "plan", "show", "pln_2").ok(t).stdout
	for _, s := range []string{"Revision:", "2", "target.yaml", "2.0 KiB", want} {
		if !strings.Contains(out, s) {
			t.Errorf("show lacks %q:\n%s", s, out)
		}
	}

	out = h.run("", "plan", "approve", "pln_2").ok(t).stdout
	if !strings.Contains(out, "Approving plan pln_2 at revision 2 with these attachments:") || !strings.Contains(out, "target.yaml") || !strings.Contains(out, want) {
		t.Errorf("approve output:\n%s", out)
	}
	if got := string(f.last(t, "POST", "/v1/plans/pln_2/approve").body); got != `{"revision":2}` {
		t.Errorf("approve body %s", got)
	}

	// --revision pins what the operator reviewed earlier.
	h.run("", "plan", "approve", "pln_2", "--revision", "2").ok(t)
	before := len(f.requests())
	r := h.run("", "plan", "approve", "pln_2", "--revision", "1")
	if r.code != ExitError || !strings.Contains(r.stderr, "changed since you reviewed it (revision 1, now 2)") {
		t.Errorf("stale --revision: exit %d, stderr %q", r.code, r.stderr)
	}
	for _, req := range f.requests()[before:] {
		if req.method == "POST" {
			t.Errorf("a stale --revision still sent %s %s", req.method, req.path)
		}
	}
	if r := h.run("", "plan", "approve", "pln_2", "--revision", "0"); r.code != ExitUsage {
		t.Errorf("--revision 0: exit %d, stderr %q", r.code, r.stderr)
	}

	// With --json, stdout is the approved plan alone.
	r = h.run("", "--json", "plan", "approve", "pln_2").ok(t)
	var rec apiv1.PlanRecord
	if err := json.Unmarshal([]byte(r.stdout), &rec); err != nil || rec.Ref != "pln_2" || !strings.Contains(r.stderr, "Approving plan pln_2 at revision 2 with") {
		t.Errorf("json approve: %v\nstdout %s\nstderr %s", err, r.stdout, r.stderr)
	}
}
