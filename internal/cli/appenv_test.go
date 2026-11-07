package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// envRoutes serves adding and removing environments of moimroom: onprem
// can be added, eks removed (queuing a deployment to gke) and onprem removed
// without a deployment; the other answers are refusals from the contract.
func (f *deployFake) envRoutes(mux *http.ServeMux) {
	envDeployment := func(envs ...string) *apiv1.Deployment {
		d := fixtureDeployment("dep_5", apiv1.DeploymentStatusQueued)
		d.Trigger = apiv1.DeploymentTriggerEnvironment
		d.Environments = nil
		for _, e := range envs {
			d.Environments = append(d.Environments, apiv1.DeploymentEnvironment{Env: e, Status: apiv1.DeploymentEnvironmentStatusPending})
		}
		return &d
	}
	mux.HandleFunc("POST /v1/apps/{name}/environments", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.AppEnvironmentAddRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch name := r.PathValue("name"); {
		case name == "shop":
			writeError(w, 409, "app_not_registered", "app shop is only in PLATFORM_APPS_FILE")
		case name != "moimroom":
			writeError(w, 404, "app_not_found", "app not found")
		case req.Environment == "eks":
			writeError(w, 409, "environment_exists", "app moimroom already runs in eks")
		case req.Environment == "aks":
			writeError(w, 409, "db_pair_unavailable", "app moimroom has no database pair with peer aks")
		case req.Environment != "onprem":
			writeError(w, 404, "env_not_found", "environment "+req.Environment+" not found")
		default:
			a := f.registeredApp()
			a.Deploy.Environments = append(a.Deploy.Environments, "onprem")
			w.Header().Set("Location", "/v1/apps/moimroom")
			writeJSON(w, 202, apiv1.AppEnvironmentAdded{App: a, Deployment: *envDeployment("gke", "eks", "onprem")})
		}
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/environments/{env}", func(w http.ResponseWriter, r *http.Request) {
		switch name, env := r.PathValue("name"), r.PathValue("env"); {
		case name != "moimroom":
			writeError(w, 404, "app_not_found", "app not found")
		case env == "gke":
			writeError(w, 409, "primary_environment", "gke is the first environment of app moimroom")
		case env == "eks":
			a := f.registeredApp()
			a.Deploy.Environments = []string{"gke"}
			writeJSON(w, 202, apiv1.AppEnvironmentRemoved{App: a, Deployment: envDeployment("gke")})
		case env == "onprem":
			writeJSON(w, 202, apiv1.AppEnvironmentRemoved{App: f.registeredApp()})
		default:
			writeError(w, 404, "env_not_found", "app moimroom is not in environment "+env)
		}
	})
}

func TestAppEnvAdd(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "app", "env", "add", "moimroom", "onprem").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/environments").body); body != `{"environment":"onprem"}` {
		t.Errorf("body %s", body)
	}
	want := "Added environment onprem to app moimroom.\n" +
		"Queued deployment dep_5 of moimroom at main (9fceb02) to gke, eks, onprem.\n" +
		"Watch it with: charlie deploy watch dep_5\n"
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	r = h.run("", "--json", "app", "env", "add", "moimroom", "onprem").ok(t)
	if a := decodeOut[apiv1.AppEnvironmentAdded](t, r.stdout); a.Deployment.Id != "dep_5" || a.Deployment.Trigger != apiv1.DeploymentTriggerEnvironment ||
		strings.Join(a.App.Deploy.Environments, ",") != "gke,eks,onprem" {
		t.Errorf("--json: %+v", a)
	}

	// API errors come with their code, and a next step when there is one.
	for env, want := range map[string]string{
		"eks": "error: environment_exists: app moimroom already runs in eks (HTTP 409)\nThe app already runs in eks and its last deployment there is healthy.\n",
		"aks": "error: db_pair_unavailable: app moimroom has no database pair with peer aks (HTTP 409)\n" +
			"The app's database has no pair with aks as the peer, so its database there could never be migrated to; the pair is made when the app is registered.\n",
		"nope": "error: env_not_found: environment nope not found (HTTP 404)\nSee the server's environments with: charlie env list, and the app's with: charlie app show moimroom\n",
	} {
		r := h.run("", "app", "env", "add", "moimroom", env)
		if r.code != ExitError || r.stderr != want || r.stdout != "" {
			t.Errorf("%s: exit %d\nstdout %q\nstderr %q", env, r.code, r.stdout, r.stderr)
		}
	}
	if r := h.run("", "app", "env", "add", "shop", "onprem"); r.code != ExitError ||
		r.stderr != "error: app_not_registered: app shop is only in PLATFORM_APPS_FILE (HTTP 409)\n" {
		t.Errorf("file app: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "env", "add", "moimroom"); r.code != ExitUsage {
		t.Errorf("one argument: exit %d", r.code)
	}
}

func TestAppEnvAddWatchFollowsTheDeployment(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		s := dsse{w}
		s.snapshot(fixtureDeployment("dep_5", apiv1.DeploymentStatusQueued))
		s.event(1, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusBuilding, "", "")
		s.event(2, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusHealthy, "", "")
		s.snapshot(fixtureDeployment("dep_5", apiv1.DeploymentStatusHealthy))
	}
	r := f.harness(t).run("", "app", "env", "add", "moimroom", "onprem", "--watch").ok(t)
	if !strings.Contains(r.stdout, "Added environment onprem to app moimroom.") || strings.Contains(r.stdout, "Watch it with") ||
		!strings.Contains(r.stdout, "[ok] healthy") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if n := f.sent("GET", "/v1/deployments/dep_5/events"); n != 1 {
		t.Errorf("event stream opened %d times", n)
	}
}

func TestAppEnvRemove(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	const del = "/v1/apps/moimroom/environments/eks"

	r := h.run("eks\n", "app", "env", "remove", "moimroom", "eks").ok(t)
	if !strings.Contains(r.stderr, "This removes environment eks from app moimroom. Argo CD deletes the namespaces of moimroom, moimroom--dev in eks, with their copy of the database and their files. The data there cannot be recovered.") ||
		!strings.HasSuffix(r.stderr, "Type the environment name to confirm: \n") {
		t.Errorf("prompt %q", r.stderr)
	}
	want := "Removed environment eks from app moimroom. Argo CD is deleting its workloads and data there.\n" +
		"Queued deployment dep_5 of moimroom at main (9fceb02) to gke.\n" +
		"Watch it with: charlie deploy watch dep_5\n"
	if r.stdout != want || f.sent("DELETE", del) != 1 {
		t.Errorf("stdout:\n%s\nwant:\n%s\n%d deletes", r.stdout, want, f.sent("DELETE", del))
	}

	// A wrong answer sends nothing.
	for _, answer := range []string{"y\n", "moimroom\n", ""} {
		r := h.run(answer, "app", "env", "remove", "moimroom", "eks")
		if r.code != ExitError || !strings.Contains(r.stderr, "environment eks was not removed from app moimroom; type its name to confirm, or pass --yes") {
			t.Errorf("answer %q: exit %d, stderr %q", answer, r.code, r.stderr)
		}
	}
	if n := f.sent("DELETE", del); n != 1 {
		t.Errorf("%d deletes after wrong answers", n)
	}

	// An environment the app is not in is refused before asking.
	if r := h.run("", "app", "env", "remove", "moimroom", "onprem"); r.code != ExitError ||
		r.stderr != "error: app moimroom is not in environment onprem; its environments are gke, eks\n" {
		t.Errorf("not in app: exit %d, stderr %q", r.code, r.stderr)
	}

	// --yes asks nothing; without a deployment to repeat none is queued.
	r = h.run("", "app", "env", "remove", "moimroom", "onprem", "--yes", "--watch").ok(t)
	if r.stderr != "" || r.stdout != "Removed environment onprem from app moimroom. Argo CD is deleting its workloads and data there.\n"+
		"No deployment was queued: the default instance has no healthy deployment to repeat.\n" {
		t.Errorf("--yes without deployment: stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if n := f.sent("GET", "/v1/deployments/dep_5/events"); n != 0 {
		t.Errorf("watched a deployment that was not queued")
	}

	r = h.run("", "--json", "app", "env", "remove", "moimroom", "eks", "-y").ok(t)
	if rm := decodeOut[apiv1.AppEnvironmentRemoved](t, r.stdout); rm.Deployment == nil || rm.Deployment.Id != "dep_5" || strings.Join(rm.App.Deploy.Environments, ",") != "gke" {
		t.Errorf("--json: %s", r.stdout)
	}

	if r := h.run("", "app", "env", "remove", "moimroom", "gke", "--yes"); r.code != ExitError ||
		r.stderr != "error: primary_environment: gke is the first environment of app moimroom (HTTP 409)\n"+
			"The app's first environment, where its database was born primary, stays; remove the app with: charlie app rm moimroom\n" {
		t.Errorf("primary: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "env", "remove", "shop", "eks"); r.code != ExitError || !strings.Contains(r.stderr, "app shop is only in the server's apps file") {
		t.Errorf("file app: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestAppShowDatabasePair(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	if out := h.run("", "app", "show", "moimroom").ok(t).stdout; strings.Contains(out, "Database pair") {
		t.Errorf("no pair, but:\n%s", out)
	}

	f.topology = &apiv1.AppDBTopology{Primary: "gke", Replica: "onprem"}
	out := h.run("", "app", "show", "moimroom").ok(t).stdout
	for _, want := range []string{
		"\nDatabase pair\n",
		"Replica in app:  no (add it with: charlie app env add moimroom onprem)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Ready to migrate") {
		t.Errorf("readiness without one:\n%s", out)
	}

	f.topology = &apiv1.AppDBTopology{Primary: "gke", Replica: "eks", ReplicaPresent: true, Readiness: &apiv1.AppDBReadiness{
		Ready: false, Reason: "cluster_not_healthy", Message: "the database in eks is not healthy",
		Environments: []apiv1.AppDBReadinessEnvironment{
			{Env: "gke", InApp: true, Deployed: true, GatewayBackend: true, Role: apiv1.AppDBReadinessEnvironmentRolePrimary, Healthy: true, Phase: ptr("Cluster in healthy state")},
			{Env: "eks", InApp: true, Deployed: true, GatewayBackend: true, Role: apiv1.AppDBReadinessEnvironmentRoleReplica},
		}}}
	out = h.run("", "app", "show", "moimroom").ok(t).stdout
	for _, want := range []string{
		"Replica in app:    yes\n",
		"Ready to migrate:  [x] no (cluster_not_healthy)\n",
		"Readiness:         the database in eks is not healthy\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	aligned(t, out, "ROLE", "Cluster in healthy state", "primary")
	aligned(t, out, "HEALTHY", "replica ", "no")
	aligned(t, out, "PHASE", "primary ", "Cluster in healthy state")

	d := decodeOut[appDetail](t, h.run("", "--json", "app", "show", "moimroom").ok(t).stdout)
	if tp := d.App.Deploy.DbTopology; tp == nil || tp.Readiness == nil || tp.Readiness.Reason != "cluster_not_healthy" {
		t.Errorf("--json: %+v", tp)
	}
}
