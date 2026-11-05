package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

const (
	testSHA = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	devSHA  = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// deployFake serves the app and deployment routes of api/openapi.yaml: the
// registered app moimroom with its added instance moimroom--dev, the apps
// file's app shop, and the SSE contract on the deployment events route.
type deployFake struct {
	*httptest.Server

	mu     sync.Mutex
	reqs   []fakeReq
	events func(w http.ResponseWriter, r *http.Request, conn int)
	conns  int
	added  []apiv1.AppInstance // instances of moimroom added since the fixture

	// redeployCode, when set, is the 409 error code of every redeploy, or
	// "ignore" to queue a build as a server without redeploy would.
	redeployCode string
	// App secrets of moimroom, by instance ("" app-wide) and name; nil
	// answers 503 secrets_not_configured. See secretRoutes.
	secrets map[string]map[string]fakeSecret
}

// registeredApp is moimroom as the server lists it now: the fixture and the
// instances added since.
func (f *deployFake) registeredApp() apiv1.App {
	a := fixtureRegisteredApp()
	f.mu.Lock()
	defer f.mu.Unlock()
	a.Deploy.Instances = append(a.Deploy.Instances, f.added...)
	return a
}

func newDeployFake(t *testing.T) *deployFake {
	t.Helper()
	f := &deployFake{secrets: fixtureSecrets()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.AppList{Apps: []apiv1.App{fixtureFileApp(), f.registeredApp()}})
	})
	mux.HandleFunc("GET /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "moimroom":
			writeJSON(w, 200, f.registeredApp())
		case "shop":
			writeJSON(w, 200, fixtureFileApp())
		default:
			writeError(w, 404, "app_not_found", "app not found")
		}
	})
	mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.AppCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Repo == "org/broken" {
			writeSpecInvalid(w)
			return
		}
		a := fixtureRegisteredApp()
		a.Deploy.Repo = req.Repo
		if req.Branch != nil {
			a.Deploy.Branch = *req.Branch
		}
		if req.AutoDeploy != nil {
			a.Deploy.AutoDeploy = *req.AutoDeploy
		}
		w.Header().Set("Location", "/v1/apps/moimroom")
		writeJSON(w, 201, apiv1.AppCreated{App: a, Deployment: fixtureDeployment("dep_1", apiv1.DeploymentStatusQueued)})
	})
	mux.HandleFunc("DELETE /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "moimroom":
			w.WriteHeader(202)
		case "shop":
			writeError(w, 409, "app_not_registered", "app shop is only in PLATFORM_APPS_FILE")
		default:
			writeError(w, 404, "app_not_found", "app not found")
		}
	})
	mux.HandleFunc("POST /v1/apps/{name}/instances", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.AppInstanceCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case r.PathValue("name") == "shop":
			writeError(w, 409, "app_not_registered", "app shop is only in PLATFORM_APPS_FILE")
		case r.PathValue("name") != "moimroom":
			writeError(w, 404, "app_not_found", "app not found")
		case req.Branch == "broken":
			writeSpecInvalid(w)
		case req.Branch == "main" || req.Branch == "dev":
			writeError(w, 409, "branch_exists", "app moimroom already deploys branch "+req.Branch)
		default:
			d := fixtureInstanceDeployment("dep_4", req.Branch, apiv1.DeploymentStatusQueued)
			in := apiv1.AppInstance{Name: d.Instance, Branch: req.Branch, AutoDeploy: req.AutoDeploy == nil || *req.AutoDeploy,
				Spec: fixtureSpec(), SpecCommit: devSHA, Url: ptr("https://" + d.Instance + ".hopcharlie.dev"), CreatedAt: t0, CreatedBy: "alice",
				Status: []apiv1.AppEnvironmentStatus{
					{Env: "gke", Status: apiv1.DeploymentEnvironmentStatusPending},
					{Env: "eks", Status: apiv1.DeploymentEnvironmentStatusPending},
				}}
			// The branch is stored before the answer, so the app lists the
			// instance from now on.
			f.mu.Lock()
			f.added = append(f.added, in)
			f.mu.Unlock()
			w.Header().Set("Location", "/v1/apps/moimroom")
			writeJSON(w, 201, apiv1.AppInstanceCreated{Instance: in, Deployment: d})
		}
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/instances/{instance}", func(w http.ResponseWriter, r *http.Request) {
		switch name, instance := r.PathValue("name"), r.PathValue("instance"); {
		case name == "shop":
			writeError(w, 409, "app_not_registered", "app shop is only in PLATFORM_APPS_FILE")
		case name != "moimroom":
			writeError(w, 404, "app_not_found", "app not found")
		case instance == "moimroom--dev":
			w.WriteHeader(202)
		case instance == "moimroom":
			writeError(w, 409, "default_instance", "the default instance goes only with the app")
		default:
			writeError(w, 404, "instance_not_found", "instance not found")
		}
	})
	mux.HandleFunc("POST /v1/apps/validate", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.AppSpecValidateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		v := apiv1.AppSpecValidation{Valid: true, Commit: ptr(testSHA), Spec: ptr(fixtureSpec()), Errors: []apiv1.AppSpecFieldError{}}
		if str(req.Repo) == "org/broken" {
			v = fixtureInvalid()
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/apps/{name}/deployments", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.DeploymentCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if str(req.Ref) == "broken" {
			writeSpecInvalid(w)
			return
		}
		d := fixtureDeployment("dep_2", apiv1.DeploymentStatusQueued)
		switch {
		case req.Branch == nil || *req.Branch == "main":
		case *req.Branch == "dev":
			d = fixtureInstanceDeployment("dep_2", "dev", apiv1.DeploymentStatusQueued)
		default:
			writeError(w, 400, "invalid_request", fmt.Sprintf("app moimroom does not deploy branch %q", *req.Branch))
			return
		}
		d.Trigger = apiv1.DeploymentTriggerManual
		if req.Redeploy != nil && *req.Redeploy {
			f.mu.Lock()
			code := f.redeployCode
			f.mu.Unlock()
			switch code {
			case "nothing_to_redeploy":
				writeError(w, 409, code, "instance "+d.Instance+" has no healthy deployment to deploy again")
				return
			case "deployment_in_progress":
				writeError(w, 409, code, "instance "+d.Instance+" has a deployment that has not finished")
				return
			}
			if code != "ignore" { // "ignore": a server from before redeploy
				d.Trigger = apiv1.DeploymentTriggerRedeploy
			}
		}
		if req.Ref != nil {
			d.Ref = *req.Ref
		}
		if req.Environments != nil {
			d.Environments = d.Environments[1:] // eks
		}
		w.Header().Set("Location", "/v1/deployments/dep_2")
		writeJSON(w, 202, d)
	})
	mux.HandleFunc("GET /v1/apps/{name}/deployments", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "shop" {
			writeJSON(w, 200, apiv1.DeploymentList{Deployments: []apiv1.Deployment{}})
			return
		}
		switch b := r.URL.Query().Get("branch"); b {
		case "", "main":
		case "dev":
			writeJSON(w, 200, apiv1.DeploymentList{Deployments: []apiv1.Deployment{fixtureInstanceDeployment("dep_3", b, apiv1.DeploymentStatusHealthy)}})
			return
		default:
			writeError(w, 400, "invalid_request", "app moimroom does not deploy branch "+b)
			return
		}
		later := fixtureDeployment("dep_2", apiv1.DeploymentStatusBuilding)
		later.CreatedAt = t0.Add(time.Hour)
		writeJSON(w, 200, apiv1.DeploymentList{Deployments: []apiv1.Deployment{later, fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy)}})
	})
	mux.HandleFunc("GET /v1/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "dep_1":
			writeJSON(w, 200, fixtureDeployment("dep_1", apiv1.DeploymentStatusFailed))
		case "dep_2":
			writeJSON(w, 200, fixtureDeployment("dep_2", apiv1.DeploymentStatusBuilding))
		default:
			writeError(w, 404, "deployment_not_found", "deployment not found")
		}
	})
	mux.HandleFunc("GET /v1/deployments/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n, script := f.conns, f.events
		f.conns++
		f.mu.Unlock()
		if id := r.PathValue("id"); script == nil || (id != "dep_1" && id != "dep_2" && id != "dep_4") {
			writeError(w, 404, "deployment_not_found", "deployment not found")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		script(w, r, n)
	})
	f.secretRoutes(mux)
	mux.HandleFunc("GET /v1/github/repos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiv1.GitHubRepoList{Repos: []apiv1.GitHubRepo{
			{FullName: "org/moimroom", Private: true, DefaultBranch: "main", HtmlUrl: "https://github.com/org/moimroom"},
			{FullName: "org/site", DefaultBranch: "trunk", HtmlUrl: "https://github.com/org/site"},
		}})
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			writeError(w, 401, "unauthorized", "missing or invalid bearer token")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *deployFake) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

func (f *deployFake) last(t *testing.T, method, path string) fakeReq {
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

func (f *deployFake) sent(method, path string) int {
	n := 0
	for _, r := range f.requests() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (f *deployFake) lastEventIDs() []string {
	var ids []string
	for _, r := range f.requests() {
		if strings.HasSuffix(r.path, "/events") {
			ids = append(ids, r.header.Get("Last-Event-ID"))
		}
	}
	return ids
}

func (f *deployFake) harness(t *testing.T) *harness {
	return newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_TOKEN": "tok"})
}

// dsse writes a deployment event stream the way the contract describes it.
type dsse struct{ w http.ResponseWriter }

func (s dsse) snapshot(d apiv1.Deployment) {
	b, _ := json.Marshal(d)
	fmt.Fprintf(s.w, "event: snapshot\ndata: %s\n\n", b)
	s.w.(http.Flusher).Flush()
}

func (s dsse) event(seq int64, typ apiv1.DeploymentEventType, phase apiv1.DeploymentStatus, env, msg string) {
	e := apiv1.DeploymentEvent{Seq: seq, DeploymentId: "dep_1", Time: tsec(int(seq)), Type: typ}
	if phase != "" {
		e.Phase = &phase
	}
	if env != "" {
		e.Env = &env
	}
	if msg != "" {
		e.Message = &msg
	}
	b, _ := json.Marshal(e)
	fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", seq, typ, b)
	s.w.(http.Flusher).Flush()
}

func writeSpecInvalid(w http.ResponseWriter) {
	writeJSON(w, 422, apiv1.AppSpecInvalid{
		Error:      apiv1.ErrorBody{Code: "invalid_app_spec", Message: "platform.yaml breaks 3 rules"},
		Validation: fixtureInvalid(),
	})
}

func fixtureInvalid() apiv1.AppSpecValidation {
	return apiv1.AppSpecValidation{Valid: false, Commit: ptr(testSHA), Errors: []apiv1.AppSpecFieldError{
		{Path: "", Message: "services[0]: Dockerfile api/Dockerfile is not in the repository"},
		{Path: "services[0].port", Message: "must be between 1 and 65535"},
		{Path: "services[1].public", Message: "only one service may be public; services[0] is"},
	}}
}

func fixtureSpec() apiv1.AppSpec {
	return apiv1.AppSpec{Name: "moimroom",
		Services: []apiv1.AppSpecService{{Name: "web", Path: ".", Dockerfile: "Dockerfile", Port: 8080, Public: true, Replicas: 1,
			HealthCheck: ptr("/healthz"), Env: &map[string]string{"LOG_LEVEL": "info"}}},
		Resources: &[]apiv1.AppSpecResource{{Name: "db", Type: "postgres"}, {Name: "photos", Type: "s3"}}}
}

func fixtureFileApp() apiv1.App {
	return apiv1.App{Name: "shop", Namespace: "shop", Database: apiv1.AppDatabase{Name: "shop", User: "shop", Schemas: []string{}},
		Environments: []apiv1.AppEnvironment{
			{Env: "gke", CnpgCluster: "shop-db-gke", DbHost: "shop-db-gke-rw.shop.gke.ts.net:5432"},
			{Env: "eks", CnpgCluster: "shop-db-eks", DbHost: "shop-db-eks-rw.shop.eks.ts.net:5432", Bucket: ptr("s3:shop-uploads")},
		}}
}

func fixtureRegisteredApp() apiv1.App {
	last := fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy)
	url := ptr("https://moimroom.hopcharlie.dev")
	status := []apiv1.AppEnvironmentStatus{
		{Env: "gke", Status: apiv1.DeploymentEnvironmentStatusHealthy, Commit: ptr(testSHA), SyncStatus: ptr("Synced"), HealthStatus: ptr("Healthy")},
		{Env: "eks", Status: apiv1.DeploymentEnvironmentStatusDegraded, Commit: ptr(testSHA), SyncStatus: ptr("Synced"), HealthStatus: ptr("Degraded")},
	}
	devLast := fixtureInstanceDeployment("dep_3", "dev", apiv1.DeploymentStatusHealthy)
	return apiv1.App{Name: "moimroom", Namespace: "moimroom", Database: apiv1.AppDatabase{Name: "app", User: "app", Schemas: []string{}},
		Environments: []apiv1.AppEnvironment{},
		Deploy: &apiv1.AppDeploy{Repo: "org/moimroom", Branch: "main", AutoDeploy: true, Environments: []string{"gke", "eks"},
			CreatedAt: t0, CreatedBy: "alice", Spec: fixtureSpec(), SpecCommit: testSHA, Url: url,
			LastDeployment: &last, Status: status,
			Instances: []apiv1.AppInstance{
				{Name: "moimroom", Branch: "main", Default: true, AutoDeploy: true, Spec: fixtureSpec(), SpecCommit: testSHA, Url: url,
					CreatedAt: t0, CreatedBy: "alice", LastDeployment: &last, Status: status},
				{Name: "moimroom--dev", Branch: "dev", AutoDeploy: false, Spec: fixtureSpec(), SpecCommit: devSHA, Url: ptr("https://moimroom--dev.hopcharlie.dev"),
					CreatedAt: t0.Add(time.Hour), CreatedBy: "bob", LastDeployment: &devLast,
					Status: []apiv1.AppEnvironmentStatus{
						{Env: "gke", Status: apiv1.DeploymentEnvironmentStatusHealthy, Commit: ptr(devSHA)},
						{Env: "eks", Status: apiv1.DeploymentEnvironmentStatusPending},
					}},
			}}}
}

func fixtureDeployment(id string, status apiv1.DeploymentStatus) apiv1.Deployment {
	d := apiv1.Deployment{Id: id, App: "moimroom", Branch: "main", Instance: "moimroom", Ref: "main", Commit: testSHA, Trigger: apiv1.DeploymentTriggerRegister,
		TriggeredBy: "alice", Status: status, CreatedAt: t0, Images: []apiv1.DeploymentImage{},
		Environments: []apiv1.DeploymentEnvironment{
			{Env: "gke", Status: apiv1.DeploymentEnvironmentStatusPending},
			{Env: "eks", Status: apiv1.DeploymentEnvironmentStatusPending},
		}}
	if status != apiv1.DeploymentStatusQueued {
		d.StartedAt = &t0
	}
	switch status {
	case apiv1.DeploymentStatusHealthy:
		d.FinishedAt = ptr(t0.Add(95 * time.Second))
		d.Images = []apiv1.DeploymentImage{{Service: "web", Image: "123456789012.dkr.ecr.ap-northeast-2.amazonaws.com/hopcharlie/apps/moimroom/web@sha256:4c2e1a"}}
		for i, e := range d.Environments {
			d.Environments[i] = apiv1.DeploymentEnvironment{Env: e.Env, Status: apiv1.DeploymentEnvironmentStatusHealthy,
				SyncStatus: ptr("Synced"), HealthStatus: ptr("Healthy"), Url: ptr("https://moimroom-" + e.Env + ".hopcharlie.dev")}
		}
	case apiv1.DeploymentStatusFailed:
		d.FinishedAt = ptr(t0.Add(40 * time.Second))
		d.Error = ptr("build web: exit status 1")
	case apiv1.DeploymentStatusSuperseded:
		d.FinishedAt = ptr(t0.Add(30 * time.Second))
	}
	return d
}

// fixtureInstanceDeployment deploys the head of branch into the instance
// added for it.
func fixtureInstanceDeployment(id, branch string, status apiv1.DeploymentStatus) apiv1.Deployment {
	d := fixtureDeployment(id, status)
	d.Branch, d.Instance, d.Ref, d.Commit = branch, "moimroom--"+strings.ReplaceAll(branch, "/", "-"), branch, devSHA
	for i, e := range d.Environments {
		if e.Url != nil {
			d.Environments[i].Url = ptr("https://" + d.Instance + "-" + e.Env + ".hopcharlie.dev")
		}
	}
	return d
}

func TestAppAdd(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "app", "add", "--repo", "org/moimroom", "--env", "gke", "--env", "eks").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps").body); body != `{"environments":["gke","eks"],"repo":"org/moimroom"}` {
		t.Errorf("body %s", body) // branch and autoDeploy are left to the server's defaults
	}
	for _, want := range []string{
		"Registered app moimroom from org/moimroom (branch main, auto deploy on).",
		"Queued deployment dep_1 of moimroom at main (9fceb02) to gke, eks.",
		"Watch it with: charlie deploy watch dep_1",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}

	r = h.run("", "app", "add", "--repo", "org/moimroom", "--branch", "dev", "--env", "gke,eks", "--no-auto-deploy").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps").body); body != `{"autoDeploy":false,"branch":"dev","environments":["gke","eks"],"repo":"org/moimroom"}` {
		t.Errorf("body %s", body)
	}
	if !strings.Contains(r.stdout, "(branch dev, auto deploy off)") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	r = h.run("", "--json", "app", "add", "--repo", "org/moimroom", "--env", "eks").ok(t)
	if c := decodeOut[apiv1.AppCreated](t, r.stdout); c.App.Name != "moimroom" || c.Deployment.Id != "dep_1" {
		t.Errorf("--json: %+v", c)
	}
}

// A 422 lists every rule platform.yaml breaks, with its path.
func TestAppSpecInvalidIsShownWithPaths(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	want := "error: invalid_app_spec: platform.yaml breaks 3 rules (HTTP 422)\n" +
		"  services[0]: Dockerfile api/Dockerfile is not in the repository\n" +
		"  services[0].port: must be between 1 and 65535\n" +
		"  services[1].public: only one service may be public; services[0] is\n"
	for _, args := range [][]string{
		{"app", "add", "--repo", "org/broken", "--env", "eks"},
		{"app", "add", "--repo", "org/broken", "--env", "eks", "--watch"},
		{"app", "branch", "add", "moimroom", "broken"},
		{"app", "branch", "add", "moimroom", "broken", "--watch"},
		{"deploy", "moimroom", "--ref", "broken"},
		{"--json", "deploy", "moimroom", "--ref", "broken"},
	} {
		r := h.run("", args...)
		if r.code != ExitError || r.stderr != want || r.stdout != "" {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if n := f.sent("GET", "/v1/deployments/dep_1/events"); n != 0 {
		t.Errorf("watched after a 422: %d", n)
	}
}

func TestAppAddWatchFollowsTheFirstDeployment(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, conn int) {
		s := dsse{w}
		switch conn {
		case 0: // drops while the image builds
			s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusQueued))
			s.event(1, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusBuilding, "", "")
			s.event(2, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#1 [internal] load build definition from Dockerfile")
			s.event(3, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#2 DONE 0.4s\r")
			panic(http.ErrAbortHandler)
		default: // replays one too many, then the deployment finishes
			s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
			s.event(3, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#2 DONE 0.4s\r")
			s.event(4, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusCommitting, "", "apps/moimroom/gke.yaml, apps/moimroom/eks.yaml")
			s.event(5, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusSyncing, "", "")
			s.event(6, apiv1.DeploymentEventTypeEnvironment, apiv1.DeploymentStatusSyncing, "eks", "Synced, Healthy")
			s.event(7, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusHealthy, "", "")
			s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy))
		}
	}
	r := f.harness(t).run("", "app", "add", "--repo", "org/moimroom", "--env", "gke,eks", "--watch").ok(t)
	for line, want := range map[string]int{
		"Registered app moimroom": 1,
		"Deployment dep_1  app moimroom  main (9fceb02) to gke, eks: [ ] queued":               1,
		clock(tsec(1)) + "  [..] building\n":                                                   1,
		clock(tsec(2)) + "  building  #1 [internal] load build definition from Dockerfile\n":   1,
		clock(tsec(3)) + "  building  #2 DONE 0.4s\n":                                          1,
		clock(tsec(4)) + "  [..] committing: apps/moimroom/gke.yaml, apps/moimroom/eks.yaml\n": 1,
		clock(tsec(6)) + "  env eks: Synced, Healthy\n":                                        1,
		clock(tsec(7)) + "  [ok] healthy\n":                                                    1,
		"Phases [ok] queued  [ok] building  [ok] committing  [ok] syncing  [ok] healthy\n":     1,
		"elapsed 1m35s": 1,
		"[ok] live at https://moimroom.hopcharlie.dev\n": 1,
		"Watch it with": 0,
	} {
		if got := strings.Count(r.stdout, line); got != want {
			t.Errorf("%q printed %d times, want %d\n%s", line, got, want, r.stdout)
		}
	}
	aligned(t, r.stdout, "HEALTH", "https://moimroom-eks", "Healthy")
	if want := []string{"", "3"}; !reflect.DeepEqual(f.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", f.lastEventIDs(), want)
	}
}

func TestDeployWatchExitCodes(t *testing.T) {
	for status, want := range map[apiv1.DeploymentStatus]int{
		apiv1.DeploymentStatusHealthy:    ExitOK,
		apiv1.DeploymentStatusFailed:     ExitRunFailed,
		apiv1.DeploymentStatusSuperseded: ExitRunFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newDeployFake(t)
			f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
				s := dsse{w}
				s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
				s.event(1, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "npm ERR! missing script: build")
				msg := ""
				if status == apiv1.DeploymentStatusFailed {
					msg = "build web: exit status 1"
				}
				s.event(2, apiv1.DeploymentEventTypeDone, status, "", msg)
				s.snapshot(fixtureDeployment("dep_1", status))
			}
			r := f.harness(t).run("", "deploy", "watch", "dep_1")
			if r.code != want {
				t.Fatalf("exit %d, want %d\n%s%s", r.code, want, r.stdout, r.stderr)
			}
			if want == ExitOK {
				return
			}
			end := "deployment dep_1 ended " + string(status)
			if status == apiv1.DeploymentStatusFailed {
				end += ": build web: exit status 1"
			}
			if strings.TrimSpace(r.stderr) != end {
				t.Errorf("stderr %q, want %q", r.stderr, end)
			}
			phases := "Phases [ok] queued  [x] building  [ ] committing  [ ] syncing  [x] " + string(status)
			if !strings.Contains(r.stdout, phases) {
				t.Errorf("no %q in:\n%s", phases, r.stdout)
			}
			if strings.Contains(r.stdout, "live at") {
				t.Errorf("a URL for a deployment that did not become healthy:\n%s", r.stdout)
			}
		})
	}
}

func TestDeployWatchJSON(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, conn int) {
		s := dsse{w}
		s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
		if conn == 0 {
			s.event(1, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "step 1")
			panic(http.ErrAbortHandler)
		}
		s.event(1, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "step 1")
		s.event(2, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusFailed, "", "build web: exit status 1")
		s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusFailed))
	}
	r := f.harness(t).run("", "--json", "deploy", "moimroom", "--watch")
	if r.code != ExitRunFailed {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if d := decodeOut[apiv1.Deployment](t, lines[0]); d.Id != "dep_2" {
		t.Errorf("first line is not the created deployment: %s", lines[0])
	}
	var names []string
	for _, l := range lines[1:] {
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
			if d := decodeOut[apiv1.Deployment](t, string(m.Data)); d.Id != "dep_1" {
				t.Errorf("snapshot data %s", m.Data)
			}
		}
	}
	if want := []string{"snapshot:0", "log:1", "snapshot:0", "done:2", "snapshot:0"}; !reflect.DeepEqual(names, want) {
		t.Errorf("messages %v, want %v", names, want)
	}
}

func TestDeployWatchInterrupted(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		dsse{w}.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
		<-r.Context().Done()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := f.harness(t).runCtx(ctx, "", "deploy", "watch", "dep_1")
	if r.code != ExitInterrupted || !strings.Contains(r.stderr, "Reattach with: charlie deploy watch dep_1") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if r := f.harness(t).run("", "deploy", "watch", "dep_x"); r.code != ExitError || !strings.Contains(r.stderr, "deployment_not_found") {
		t.Errorf("unknown deployment: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestDeployCommands(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "deploy", "moimroom").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/deployments").body); body != `{}` {
		t.Errorf("body %s", body)
	}
	if !strings.Contains(r.stdout, "Queued deployment dep_2 of moimroom at main (9fceb02) to gke, eks.\nWatch it with: charlie deploy watch dep_2\n") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	r = h.run("", "deploy", "moimroom", "--ref", "v1.2", "--env", "eks").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/deployments").body); body != `{"environments":["eks"],"ref":"v1.2"}` {
		t.Errorf("body %s", body)
	}
	if !strings.Contains(r.stdout, "at v1.2 (9fceb02) to eks.") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	out := h.run("", "deploy", "list", "moimroom", "--limit", "5").ok(t).stdout
	if q := f.last(t, "GET", "/v1/apps/moimroom/deployments").query; q != "limit=5" {
		t.Errorf("query %q", q)
	}
	aligned(t, out, "STATUS", "dep_1", "[ok] healthy")
	aligned(t, out, "BRANCH", "dep_1", "main")
	aligned(t, out, "ENVS", "dep_1", "[ok] gke, [ok] eks")
	aligned(t, out, "DURATION", "dep_1", "1m35s")
	aligned(t, out, "ENVS", "dep_2", "[ ] gke, [ ] eks")
	if strings.Index(out, "dep_2") > strings.Index(out, "dep_1") {
		t.Errorf("not newest first:\n%s", out)
	}
	if out := h.run("", "deploy", "list", "shop").ok(t).stdout; out != "No deployments of shop.\n" {
		t.Errorf("empty list: %q", out)
	}

	out = h.run("", "deploy", "show", "dep_1").ok(t).stdout
	for _, want := range []string{"Deployment:  dep_1", "Branch:      main", "Instance:    moimroom", "Status:      [x] failed", "Commit:      " + testSHA, "Trigger:     register by alice",
		"Duration:    40.0s", "Error:       build web: exit status 1", "ENV  STATUS"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy show lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Follow it with") {
		t.Errorf("a finished deployment has no follow hint:\n%s", out)
	}
	if out := h.run("", "deploy", "show", "dep_2").ok(t).stdout; !strings.Contains(out, "Follow it with: charlie deploy watch dep_2") {
		t.Errorf("deploy show of a running deployment:\n%s", out)
	}
	if r := h.run("", "deploy", "show", "dep_x"); r.code != ExitError || !strings.Contains(r.stderr, "deployment_not_found") {
		t.Errorf("unknown deployment: exit %d %q", r.code, r.stderr)
	}

	check := func(args []string, into, want any) {
		t.Helper()
		out := h.run("", append([]string{"--json"}, args...)...).ok(t).stdout
		if err := json.Unmarshal([]byte(out), into); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !reflect.DeepEqual(reflect.ValueOf(into).Elem().Interface(), want) {
			t.Errorf("%v: got %+v, want %+v", args, into, want)
		}
	}
	check([]string{"deploy", "show", "dep_1"}, &apiv1.Deployment{}, normalize(t, fixtureDeployment("dep_1", apiv1.DeploymentStatusFailed)))
	later := fixtureDeployment("dep_2", apiv1.DeploymentStatusBuilding)
	later.CreatedAt = t0.Add(time.Hour)
	check([]string{"deploy", "list", "moimroom"}, &apiv1.DeploymentList{},
		normalize(t, apiv1.DeploymentList{Deployments: []apiv1.Deployment{later, fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy)}}))
	check([]string{"app", "repos"}, &apiv1.GitHubRepoList{}, apiv1.GitHubRepoList{Repos: []apiv1.GitHubRepo{
		{FullName: "org/moimroom", Private: true, DefaultBranch: "main", HtmlUrl: "https://github.com/org/moimroom"},
		{FullName: "org/site", DefaultBranch: "trunk", HtmlUrl: "https://github.com/org/site"}}})
}

func TestDeployLogs(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	logs := clock(tsec(2)) + "  building  #1 FROM node:22\n" + clock(tsec(3)) + "  building  npm ERR! missing script: build\n"
	finished := func(w http.ResponseWriter, r *http.Request, _ int) {
		s := dsse{w}
		s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusFailed))
		s.event(1, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusBuilding, "", "")
		s.event(2, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#1 FROM node:22")
		s.event(3, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "npm ERR! missing script: build")
		s.event(4, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusFailed, "", "build web: exit status 1")
		s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusFailed))
	}
	f.events = finished

	// Reading the log of a failed deployment is not a failure.
	r := h.run("", "deploy", "logs", "dep_1").ok(t)
	if r.stdout != logs || r.stderr != "" {
		t.Errorf("stdout %q\nstderr %q", r.stdout, r.stderr)
	}
	// Following it ends the way the deployment did.
	r = h.run("", "deploy", "logs", "dep_1", "--follow")
	if r.code != ExitRunFailed || r.stdout != logs || !strings.Contains(r.stderr, "deployment dep_1 ended failed: build web: exit status 1") {
		t.Errorf("--follow: exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	r = h.run("", "--json", "deploy", "logs", "dep_1").ok(t)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("--json: %q", r.stdout)
	}
	for i, l := range lines {
		m := decodeOut[jsonMessage](t, l)
		if m.Event != "log" || m.ID != int64(i+2) {
			t.Errorf("--json line %q", l)
		}
	}

	// A deployment still going: the stored lines, then a hint.
	defer func(q time.Duration) { logsQuiet = q }(logsQuiet)
	logsQuiet = 50 * time.Millisecond
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		s := dsse{w}
		s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
		s.event(2, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#1 FROM node:22")
		s.event(3, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "npm ERR! missing script: build")
		<-r.Context().Done()
	}
	r = h.run("", "deploy", "logs", "dep_1").ok(t)
	if r.stdout != logs || !strings.Contains(r.stderr, "deployment dep_1 is building; follow it with: charlie deploy logs dep_1 --follow") {
		t.Errorf("running: stdout %q\nstderr %q", r.stdout, r.stderr)
	}

	// --follow resumes a dropped stream without repeating lines.
	f.events = func(w http.ResponseWriter, r *http.Request, conn int) {
		if conn == 0 {
			s := dsse{w}
			s.snapshot(fixtureDeployment("dep_1", apiv1.DeploymentStatusBuilding))
			s.event(2, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", "#1 FROM node:22")
			panic(http.ErrAbortHandler)
		}
		finished(w, r, conn)
	}
	f.mu.Lock()
	f.conns, f.reqs = 0, nil
	f.mu.Unlock()
	r = h.run("", "deploy", "logs", "dep_1", "-f")
	if r.code != ExitRunFailed || r.stdout != logs {
		t.Errorf("resume: exit %d\nstdout %q", r.code, r.stdout)
	}
	if want := []string{"", "2"}; !reflect.DeepEqual(f.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", f.lastEventIDs(), want)
	}
}

func TestAppRm(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("moimroom\n", "app", "rm", "moimroom").ok(t)
	if !strings.Contains(r.stderr, "deletes app moimroom from gke, eks with its databases and buckets, together with its added instance moimroom--dev. The data cannot be recovered.") ||
		!strings.HasSuffix(r.stderr, "Type the app name to confirm: \n") {
		t.Errorf("prompt %q", r.stderr)
	}
	if r.stdout != "Deleted app moimroom. Argo CD is removing its workloads and data.\n" || f.sent("DELETE", "/v1/apps/moimroom") != 1 {
		t.Errorf("stdout %q, %d deletes", r.stdout, f.sent("DELETE", "/v1/apps/moimroom"))
	}

	for _, answer := range []string{"y\n", "", "moim\n"} {
		r := h.run(answer, "app", "rm", "moimroom")
		if r.code != ExitError || !strings.Contains(r.stderr, "app moimroom was not deleted; type its name to confirm, or pass --yes") {
			t.Errorf("answer %q: exit %d, stderr %q", answer, r.code, r.stderr)
		}
	}
	if n := f.sent("DELETE", "/v1/apps/moimroom"); n != 1 {
		t.Errorf("deleted without confirmation: %d deletes", n)
	}

	gets := f.sent("GET", "/v1/apps/moimroom")
	r = h.run("", "app", "rm", "moimroom", "--yes").ok(t)
	if f.sent("DELETE", "/v1/apps/moimroom") != 2 || f.sent("GET", "/v1/apps/moimroom") != gets || strings.Contains(r.stderr, "confirm") {
		t.Errorf("--yes asked or did not delete: stderr %q", r.stderr)
	}
	if r := h.run("", "--json", "app", "rm", "moimroom", "-y").ok(t); r.stdout != "" {
		t.Errorf("--json printed %q for a response without a body", r.stdout)
	}

	if r := h.run("shop\n", "app", "rm", "shop"); r.code != ExitError || !strings.Contains(r.stderr, "only in the server's apps file") || strings.Contains(r.stderr, "confirm") {
		t.Errorf("file app: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "rm", "shop", "--yes"); r.code != ExitError || !strings.Contains(r.stderr, "app_not_registered") {
		t.Errorf("file app --yes: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "rm", "nope"); r.code != ExitError || !strings.Contains(r.stderr, "app_not_found") {
		t.Errorf("unknown app: exit %d, stderr %q", r.code, r.stderr)
	}
}

const validSpec = `name: moimroom
services:
  - name: web
    port: 8080
    public: true
    healthCheck: /healthz
    env: {LOG_LEVEL: info}
resources:
  - {name: db, type: postgres}
  - {name: photos, type: s3}
`

// The local check uses pkg/appspec and needs no server or login.
func TestAppValidateLocal(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("platform.yaml", validSpec)
	r := h.run("", "app", "validate", "-f", good).ok(t)
	for _, want := range []string{good + " is valid: app moimroom, 1 service, resources db (postgres), photos (s3).", "LOG_LEVEL=info"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	aligned(t, r.stdout, "HEALTH CHECK", "web", "/healthz")
	aligned(t, r.stdout, "DOCKERFILE", "web", "Dockerfile")

	// A worker has no port (platform ADR 0024): its PORT shows as -.
	worker := write("worker.yaml", strings.Replace(validSpec, "resources:", "  - name: jobs\n    memory: 512Mi\n    cpu: 250m\nresources:", 1))
	r = h.run("", "app", "validate", "-f", worker).ok(t)
	aligned(t, r.stdout, "PORT", "web", "8080")
	aligned(t, r.stdout, "PORT", "jobs", "-")

	bad := write("bad.yaml", `name: Moimroom
services:
  - name: web
    port: 0
    public: true
    env: {PORT: "8080"}
  - name: api
    port: 9000
    public: true
`)
	want := bad + ` is invalid (4 errors):
  name: must be 3 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit
  services[0].public: needs a port: the traffic gateway sends requests to it
  services[0].env.PORT: is set by the platform (PORT, DATABASE_URL, PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD, JDBC_DATABASE_URL and S3_* are reserved)
  services[1].public: only one service may be public; services[0] is
`
	if r := h.run("", "app", "validate", "-f", bad); r.code != ExitError || r.stdout != want || r.stderr != "" {
		t.Errorf("invalid: exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	// A file that does not decode has one error without a path.
	if r := h.run("name: x\nservices: []\nbogus: 1\n", "app", "validate", "-f", "-"); r.code != ExitError ||
		r.stdout != "platform.yaml from stdin is invalid (1 error):\n  platform.yaml: json: unknown field \"bogus\"\n" {
		t.Errorf("unknown key: exit %d, stdout %q", r.code, r.stdout)
	}

	r = h.run("", "--json", "app", "validate", "-f", bad)
	v := decodeOut[apiv1.AppSpecValidation](t, r.stdout)
	if r.code != ExitError || v.Valid || len(v.Errors) != 4 || v.Errors[1] != (apiv1.AppSpecFieldError{Path: "services[0].public", Message: "needs a port: the traffic gateway sends requests to it"}) || v.Spec == nil || v.Spec.Name != "Moimroom" {
		t.Errorf("--json invalid: exit %d %+v", r.code, v)
	}
	r = h.run(validSpec, "--json", "app", "validate", "-f", "-").ok(t)
	if v := decodeOut[apiv1.AppSpecValidation](t, r.stdout); !v.Valid || v.Errors == nil || len(v.Errors) != 0 || !reflect.DeepEqual(*v.Spec, normalize(t, fixtureSpec())) {
		t.Errorf("--json valid: %s", r.stdout)
	}

	// The default file is ./platform.yaml.
	t.Chdir(dir)
	if r := h.run("", "app", "validate").ok(t); !strings.HasPrefix(r.stdout, "platform.yaml is valid") {
		t.Errorf("default file: %q", r.stdout)
	}
	if err := os.Remove(good); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "app", "validate"); r.code != ExitError || !strings.Contains(r.stderr, "platform.yaml") {
		t.Errorf("missing file: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestAppValidateRemote(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "app", "validate", "--remote", "--repo", "org/moimroom").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/validate").body); body != `{"repo":"org/moimroom"}` {
		t.Errorf("body %s", body)
	}
	if !strings.HasPrefix(r.stdout, "platform.yaml in org/moimroom (main) at 9fceb02 is valid: app moimroom, 1 service") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	r = h.run("", "app", "validate", "--remote", "--repo", "org/broken", "--branch", "dev")
	if body := string(f.last(t, "POST", "/v1/apps/validate").body); body != `{"branch":"dev","repo":"org/broken"}` {
		t.Errorf("body %s", body)
	}
	want := "platform.yaml in org/broken (dev) at 9fceb02 is invalid (3 errors):\n" +
		"  services[0]: Dockerfile api/Dockerfile is not in the repository\n" +
		"  services[0].port: must be between 1 and 65535\n" +
		"  services[1].public: only one service may be public; services[0] is\n"
	if r.code != ExitError || r.stdout != want {
		t.Errorf("invalid: exit %d\nstdout %q", r.code, r.stdout)
	}
	r = h.run("", "--json", "app", "validate", "--remote", "--repo", "org/broken")
	if v := decodeOut[apiv1.AppSpecValidation](t, r.stdout); r.code != ExitError || !reflect.DeepEqual(v, normalize(t, fixtureInvalid())) {
		t.Errorf("--json: exit %d %+v", r.code, v)
	}
}

func TestAppListShowAndRepos(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	out := h.run("", "app", "list").ok(t).stdout
	reg, file, ok := strings.Cut(out, "In the apps file, for migrations\n")
	if !ok || !strings.HasPrefix(reg, "Registered from GitHub\n") {
		t.Fatalf("app list sections:\n%s", out)
	}
	aligned(t, reg, "INSTANCES", "gke", "2")
	aligned(t, reg, "REPO", "gke", "org/moimroom")
	aligned(t, reg, "BRANCH", "gke", "main")
	aligned(t, reg, "STATUS", "gke", "[ok] healthy")
	aligned(t, reg, "STATUS", "eks", "[x] degraded")
	aligned(t, reg, "COMMIT", "eks", "9fceb02")
	aligned(t, reg, "URL", "eks", "https://moimroom.hopcharlie.dev")
	aligned(t, file, "ENVS", "shop", "gke, eks")
	if strings.Contains(file, "moimroom") {
		t.Errorf("a registered-only app in the apps file section:\n%s", file)
	}
	if l := decodeOut[apiv1.AppList](t, h.run("", "--json", "app", "list").ok(t).stdout); !reflect.DeepEqual(l, normalize(t, apiv1.AppList{Apps: []apiv1.App{fixtureFileApp(), fixtureRegisteredApp()}})) {
		t.Errorf("--json app list: %+v", l)
	}
	// "apps" still lists what plans can be drafted for: the apps file.
	if out := h.run("", "apps").ok(t).stdout; strings.Contains(out, "moimroom") || strings.Count(out, "\n") != 3 {
		t.Errorf("apps:\n%s", out)
	}

	out = h.run("", "app", "show", "moimroom").ok(t).stdout
	if q := f.last(t, "GET", "/v1/apps/moimroom/deployments").query; q != "limit=5" {
		t.Errorf("deployments query %q", q)
	}
	for _, want := range []string{
		"Repo:          org/moimroom", "Auto deploy:   yes", "Environments:  gke, eks", "URL:           https://moimroom.hopcharlie.dev",
		"platform.yaml at 9fceb02: app moimroom, 1 service, resources db (postgres), photos (s3)",
		"Latest deployments", "Not registered", "In the apps file",
	} {
		if got := strings.Contains(out, want); got != (want != "Not registered" && want != "In the apps file") {
			t.Errorf("app show: %q present = %v:\n%s", want, got, out)
		}
	}
	aligned(t, out, "PORT", "web", "8080")
	aligned(t, out, "SYNC", "eks  [x] degraded", "Synced")
	_, rest, _ := strings.Cut(out, "\nInstances\n")
	instances, latest, _ := strings.Cut(rest, "Latest deployments\n")
	aligned(t, instances, "BRANCH", "moimroom--dev", "dev")
	aligned(t, instances, "DEFAULT", "moimroom--dev", "no")
	aligned(t, instances, "STATUS", "moimroom--dev", "[ok] gke, [ ] eks")
	aligned(t, instances, "URL", "moimroom--dev", "https://moimroom--dev.hopcharlie.dev")
	aligned(t, instances, "DEFAULT", "moimroom ", "yes")
	aligned(t, latest, "STATUS", "dep_2", "[..] building")
	aligned(t, latest, "BRANCH", "dep_2", "main")

	out = h.run("", "app", "show", "shop").ok(t).stdout
	for _, want := range []string{"In the apps file, for migrations (database shop@shop)", "shop-db-eks-rw.shop.eks.ts.net:5432",
		"Not registered from GitHub. Register its repository with: charlie app add --repo OWNER/NAME --env ENV"} {
		if !strings.Contains(out, want) {
			t.Errorf("app show shop lacks %q:\n%s", want, out)
		}
	}
	if f.sent("GET", "/v1/apps/shop/deployments") != 0 {
		t.Errorf("listed deployments of an app that has none")
	}
	d := decodeOut[appDetail](t, h.run("", "--json", "app", "show", "moimroom").ok(t).stdout)
	if d.App.Name != "moimroom" || len(d.Deployments) != 2 {
		t.Errorf("--json app show: %+v", d)
	}
	if out := h.run("", "--json", "app", "show", "shop").ok(t).stdout; !strings.Contains(out, `"deployments": []`) {
		t.Errorf("--json app show shop:\n%s", out)
	}
	if r := h.run("", "app", "show", "nope"); r.code != ExitError || !strings.Contains(r.stderr, "app_not_found") {
		t.Errorf("unknown app: exit %d %q", r.code, r.stderr)
	}

	out = h.run("", "app", "repos").ok(t).stdout
	aligned(t, out, "VISIBILITY", "org/moimroom", "private")
	aligned(t, out, "DEFAULT BRANCH", "org/site", "trunk")
}

func TestDeployUsageErrors(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	for _, args := range [][]string{
		{"app"},
		{"app", "bogus"},
		{"app", "add", "--env", "eks"},
		{"app", "add", "--repo", "org/moimroom"},
		{"app", "add", "--repo", "moimroom", "--env", "eks"},
		{"app", "add", "--repo", "org/moim/room", "--env", "eks"},
		{"app", "show"},
		{"app", "rm"},
		{"app", "validate", "extra"},
		{"app", "validate", "--repo", "org/moimroom"},
		{"app", "validate", "--branch", "dev"},
		{"app", "validate", "--remote"},
		{"app", "validate", "--remote", "--repo", "org/moimroom", "-f", "x.yaml"},
		{"app", "branch"},
		{"app", "branch", "bogus"},
		{"app", "branch", "add", "moimroom"},
		{"app", "branch", "add", "moimroom", "dev", "extra"},
		{"app", "branch", "list"},
		{"app", "branch", "rm", "moimroom"},
		{"deploy", "list", "moimroom", "--branch"},
		{"deploy"},
		{"deploy", "a", "b"},
		{"deploy", "list"},
		{"deploy", "list", "moimroom", "--limit", "0"},
		{"deploy", "list", "moimroom", "--limit", "201"},
		{"deploy", "show"},
		{"deploy", "watch"},
		{"deploy", "logs"},
		{"deploy", "moimroom", "--nope"},
	} {
		r := h.run("", args...)
		if last := args[len(args)-1]; last == "app" || last == "branch" { // a bare group prints help
			if r.code != ExitOK || !strings.Contains(r.stdout, "Available Commands") {
				t.Errorf("%v: exit %d, %q", args, r.code, r.stdout)
			}
			continue
		}
		if r.code != ExitUsage || !strings.Contains(r.stderr, "--help' for usage") {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("usage errors sent %d requests", n)
	}
}

func TestPhaseLine(t *testing.T) {
	for _, tc := range []struct {
		status, last apiv1.DeploymentStatus
		want         string
	}{
		{apiv1.DeploymentStatusQueued, "", "[..] queued  [ ] building  [ ] committing  [ ] syncing"},
		{apiv1.DeploymentStatusCommitting, "", "[ok] queued  [ok] building  [..] committing  [ ] syncing"},
		{apiv1.DeploymentStatusHealthy, apiv1.DeploymentStatusSyncing, "[ok] queued  [ok] building  [ok] committing  [ok] syncing  [ok] healthy"},
		{apiv1.DeploymentStatusFailed, apiv1.DeploymentStatusSyncing, "[ok] queued  [ok] building  [ok] committing  [x] syncing  [x] failed"},
		{apiv1.DeploymentStatusFailed, "", "[ ] queued  [ ] building  [ ] committing  [ ] syncing  [x] failed"},
	} {
		if got := phaseLine(tc.status, tc.last); got != tc.want {
			t.Errorf("phaseLine(%s, %q) = %q, want %q", tc.status, tc.last, got, tc.want)
		}
	}
}

func TestDeployFrame(t *testing.T) {
	v := &deployView{}
	v.apply(client.DeploymentMessage{Name: "snapshot", Snapshot: ptr(fixtureDeployment("dep_1", apiv1.DeploymentStatusQueued))})
	ev := func(seq int64, typ apiv1.DeploymentEventType, phase apiv1.DeploymentStatus, env, msg string) {
		e := apiv1.DeploymentEvent{Seq: seq, DeploymentId: "dep_1", Time: tsec(int(seq)), Type: typ, Phase: &phase}
		if env != "" {
			e.Env = &env
		}
		if msg != "" {
			e.Message = &msg
		}
		v.apply(client.DeploymentMessage{Name: string(typ), Seq: seq, Event: &e})
	}
	ev(1, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusBuilding, "", "")
	for seq := int64(2); seq <= 50; seq++ {
		ev(seq, apiv1.DeploymentEventTypeLog, apiv1.DeploymentStatusBuilding, "", fmt.Sprintf("\x1b[1mstep %d\tok", seq))
	}
	ev(51, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusSyncing, "", "")
	ev(52, apiv1.DeploymentEventTypeEnvironment, apiv1.DeploymentStatusSyncing, "eks", "Synced, Progressing")

	now := t0.Add(65 * time.Second)
	out := v.frame(now, 0, 0)
	for _, want := range []string{
		"Deployment dep_1  app moimroom  main (9fceb02)  register by alice",
		"Status [..] syncing  elapsed 1m05s",
		"Phases [ok] queued  [ok] building  [ok] committing  [..] syncing",
		"eks  [ ] pending",
		"Synced, Progressing",
		clock(tsec(50)) + "  building   [1mstep 50 ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\t") {
		t.Errorf("control characters from the log reach the terminal:\n%q", out)
	}
	if got := strings.Count(out, "step "); got != deployRecent-2 {
		t.Errorf("recent log lines %d, want %d", got, deployRecent-2)
	}

	// A short terminal drops old events first.
	small := v.frame(now, 50, 20)
	if n := strings.Count(small, "\n"); n > 20 {
		t.Errorf("frame has %d lines for a 20-line limit", n)
	}
	for _, l := range strings.Split(small, "\n") {
		if len([]rune(l)) > 49 {
			t.Errorf("line longer than the terminal: %q", l)
		}
	}
	if !strings.Contains(small, "Phases") || !strings.Contains(small, "env eks: Synced, Progressing") {
		t.Errorf("small frame:\n%s", small)
	}

	// The end: the final snapshot, and replayed events after it change nothing.
	v.apply(client.DeploymentMessage{Name: "snapshot", Snapshot: ptr(fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy))})
	ev(53, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusSyncing, "", "")
	v.url = "https://moimroom.hopcharlie.dev"
	out = v.frame(now, 0, 0)
	for _, want := range []string{"Status [ok] healthy  elapsed 1m35s", "[ok] healthy", "\n[ok] live at https://moimroom.hopcharlie.dev\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("final frame lacks %q:\n%s", want, out)
		}
	}
	v.url = ""
	if out := v.frame(now, 0, 0); !strings.Contains(out, "live at https://moimroom-gke.hopcharlie.dev, https://moimroom-eks.hopcharlie.dev") {
		t.Errorf("without the app URL the environments' are shown:\n%s", out)
	}
}
