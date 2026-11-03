package cli

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestAppBranchAdd(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "app", "branch", "add", "moimroom", "feature/login").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/instances").body); body != `{"branch":"feature/login"}` {
		t.Errorf("body %s", body) // autoDeploy is left to the server's default
	}
	want := "Added instance moimroom--feature-login of app moimroom (branch feature/login, auto deploy on).\n" +
		"URL: https://moimroom--feature-login.hopcharlie.dev\n" +
		"Queued deployment dep_4 of moimroom--feature-login at feature/login (4b825dc) to gke, eks.\n" +
		"Watch it with: charlie deploy watch dep_4\n"
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	r = h.run("", "app", "branch", "add", "moimroom", "qa", "--no-auto-deploy").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/instances").body); body != `{"autoDeploy":false,"branch":"qa"}` {
		t.Errorf("body %s", body)
	}
	if !strings.Contains(r.stdout, "Added instance moimroom--qa of app moimroom (branch qa, auto deploy off).") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	r = h.run("", "--json", "app", "branch", "add", "moimroom", "qa").ok(t)
	if c := decodeOut[apiv1.AppInstanceCreated](t, r.stdout); c.Instance.Name != "moimroom--qa" || c.Deployment.Id != "dep_4" {
		t.Errorf("--json: %+v", c)
	}

	// API errors come with their code, as everywhere else.
	for args, want := range map[string]string{
		"moimroom dev": "error: branch_exists: app moimroom already deploys branch dev (HTTP 409)\n",
		"shop dev":     "error: app_not_registered: app shop is only in PLATFORM_APPS_FILE (HTTP 409)\n",
	} {
		r := h.run("", append([]string{"app", "branch", "add"}, strings.Fields(args)...)...)
		if r.code != ExitError || r.stderr != want || r.stdout != "" {
			t.Errorf("%s: exit %d\nstdout %q\nstderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
}

func TestAppBranchAddWatchFollowsTheFirstDeployment(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		s := dsse{w}
		s.snapshot(fixtureInstanceDeployment("dep_4", "feature/login", apiv1.DeploymentStatusQueued))
		s.event(1, apiv1.DeploymentEventTypePhase, apiv1.DeploymentStatusBuilding, "", "")
		s.event(2, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusHealthy, "", "")
		s.snapshot(fixtureInstanceDeployment("dep_4", "feature/login", apiv1.DeploymentStatusHealthy))
	}
	r := f.harness(t).run("", "app", "branch", "add", "moimroom", "feature/login", "--watch").ok(t)
	for line, want := range map[string]int{
		"Added instance moimroom--feature-login": 1,
		"Deployment dep_4  app moimroom  instance moimroom--feature-login (branch feature/login)  feature/login (4b825dc) to gke, eks: [ ] queued": 1,
		clock(tsec(2)) + "  [ok] healthy\n": 1,
		"Watch it with":                     0,
		// The app does not list the new instance yet, so its own URL is
		// unknown; the default instance's URL is not it.
		"https://moimroom.hopcharlie.dev": 0,
		"[ok] live at https://moimroom--feature-login-gke.hopcharlie.dev, https://moimroom--feature-login-eks.hopcharlie.dev\n": 1,
	} {
		if got := strings.Count(r.stdout, line); got != want {
			t.Errorf("%q printed %d times, want %d\n%s", line, got, want, r.stdout)
		}
	}
	if n := f.sent("GET", "/v1/deployments/dep_4/events"); n != 1 {
		t.Errorf("event stream opened %d times", n)
	}
}

func TestAppBranchList(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	out := h.run("", "app", "branch", "list", "moimroom").ok(t).stdout
	if !strings.HasPrefix(out, "INSTANCE ") || strings.Count(out, "\n") != 3 {
		t.Errorf("app branch list:\n%s", out)
	}
	for _, c := range [][3]string{
		{"BRANCH", "moimroom ", "main"},
		{"DEFAULT", "moimroom ", "yes"},
		{"AUTO DEPLOY", "moimroom ", "yes"},
		{"STATUS", "moimroom ", "[ok] gke, [x] eks"},
		{"COMMIT", "moimroom ", "9fceb02"},
		{"URL", "moimroom ", "https://moimroom.hopcharlie.dev"},
		{"BRANCH", "moimroom--dev", "dev"},
		{"DEFAULT", "moimroom--dev", "no"},
		{"AUTO DEPLOY", "moimroom--dev", "no"},
		{"STATUS", "moimroom--dev", "[ok] gke, [ ] eks"},
		{"COMMIT", "moimroom--dev", "4b825dc"},
		{"URL", "moimroom--dev", "https://moimroom--dev.hopcharlie.dev"},
	} {
		aligned(t, out, c[0], c[1], c[2])
	}

	r := h.run("", "--json", "app", "branch", "list", "moimroom").ok(t)
	if l := decodeOut[[]apiv1.AppInstance](t, r.stdout); !reflect.DeepEqual(l, normalize(t, fixtureRegisteredApp().Deploy.Instances)) {
		t.Errorf("--json: %s", r.stdout)
	}

	if r := h.run("", "app", "branch", "list", "shop"); r.code != ExitError || !strings.Contains(r.stderr, "app shop is only in the server's apps file") {
		t.Errorf("file app: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "branch", "list", "nope"); r.code != ExitError || !strings.Contains(r.stderr, "app_not_found") {
		t.Errorf("unknown app: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestAppBranchRm(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	const del = "/v1/apps/moimroom/instances/moimroom--dev"

	// By the instance name, confirmed by typing it.
	r := h.run("moimroom--dev\n", "app", "branch", "rm", "moimroom", "moimroom--dev").ok(t)
	if !strings.Contains(r.stderr, "This deletes instance moimroom--dev (branch dev) of app moimroom from gke, eks with its databases and buckets. The data cannot be recovered.") ||
		!strings.HasSuffix(r.stderr, "Type the instance name to confirm: \n") {
		t.Errorf("prompt %q", r.stderr)
	}
	if r.stdout != "Deleted instance moimroom--dev of app moimroom. Argo CD is removing its workloads and data.\n" || f.sent("DELETE", del) != 1 {
		t.Errorf("stdout %q, %d deletes", r.stdout, f.sent("DELETE", del))
	}

	// By the branch; the answer is still the instance name.
	for _, answer := range []string{"dev\n", "y\n", ""} {
		r := h.run(answer, "app", "branch", "rm", "moimroom", "dev")
		if r.code != ExitError || !strings.Contains(r.stderr, "instance moimroom--dev was not deleted; type its name to confirm, or pass --yes") {
			t.Errorf("answer %q: exit %d, stderr %q", answer, r.code, r.stderr)
		}
	}
	if n := f.sent("DELETE", del); n != 1 {
		t.Errorf("deleted without confirmation: %d deletes", n)
	}
	r = h.run("", "app", "branch", "rm", "moimroom", "dev", "--yes").ok(t)
	if f.sent("DELETE", del) != 2 || strings.Contains(r.stderr, "confirm") {
		t.Errorf("--yes asked or did not delete: stderr %q", r.stderr)
	}
	if r := h.run("", "--json", "app", "branch", "rm", "moimroom", "moimroom--dev", "-y").ok(t); r.stdout != "" {
		t.Errorf("--json printed %q for a response without a body", r.stdout)
	}

	// The default instance is refused here, by name or by branch, even with --yes.
	for _, arg := range []string{"moimroom", "main"} {
		r := h.run("moimroom\n", "app", "branch", "rm", "moimroom", arg, "--yes")
		want := `error: instance moimroom deploys the app's own branch main; the default instance goes only with the app; use "charlie app rm moimroom"` + "\n"
		if r.code != ExitError || r.stderr != want {
			t.Errorf("%s: exit %d, stderr %q", arg, r.code, r.stderr)
		}
	}
	if n := f.sent("DELETE", "/v1/apps/moimroom/instances/moimroom"); n != 0 {
		t.Errorf("sent %d deletes of the default instance", n)
	}

	if r := h.run("", "app", "branch", "rm", "moimroom", "nope", "-y"); r.code != ExitError ||
		!strings.Contains(r.stderr, `app moimroom has no instance or branch "nope"; list them with: charlie app branch list moimroom`) {
		t.Errorf("unknown instance: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "branch", "rm", "shop", "dev", "-y"); r.code != ExitError || !strings.Contains(r.stderr, "app shop is only in the server's apps file") {
		t.Errorf("file app: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.run("", "app", "branch", "rm", "nope", "dev"); r.code != ExitError || !strings.Contains(r.stderr, "app_not_found") {
		t.Errorf("unknown app: exit %d, stderr %q", r.code, r.stderr)
	}
}

// An instance name wins over a branch of the same name.
func TestFindInstance(t *testing.T) {
	ins := []apiv1.AppInstance{
		{Name: "moimroom", Branch: "main", Default: true},
		{Name: "moimroom--moimroom-dev", Branch: "moimroom--dev"},
		{Name: "moimroom--dev", Branch: "dev"},
	}
	for arg, want := range map[string]string{
		"moimroom--dev": "moimroom--dev",
		"dev":           "moimroom--dev",
		"main":          "moimroom",
		"moimroom":      "moimroom",
		"nope":          "",
	} {
		got := ""
		if in := findInstance(ins, arg); in != nil {
			got = in.Name
		}
		if got != want {
			t.Errorf("findInstance(%q) = %q, want %q", arg, got, want)
		}
	}
}

func TestDeployBranch(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "deploy", "moimroom", "--branch", "dev").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/deployments").body); body != `{"branch":"dev"}` {
		t.Errorf("body %s", body)
	}
	if !strings.Contains(r.stdout, "Queued deployment dep_2 of moimroom--dev at dev (4b825dc) to gke, eks.\n") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	h.run("", "deploy", "moimroom", "--branch", "dev", "--ref", "v1.2", "--env", "eks").ok(t)
	if body := string(f.last(t, "POST", "/v1/apps/moimroom/deployments").body); body != `{"branch":"dev","environments":["eks"],"ref":"v1.2"}` {
		t.Errorf("body %s", body)
	}
	if r := h.run("", "deploy", "moimroom", "--branch", "nope"); r.code != ExitError || !strings.Contains(r.stderr, "invalid_request: app moimroom does not deploy branch nope") {
		t.Errorf("unknown branch: exit %d, stderr %q", r.code, r.stderr)
	}

	out := h.run("", "deploy", "list", "moimroom", "--branch", "dev", "--limit", "5").ok(t).stdout
	if q := f.last(t, "GET", "/v1/apps/moimroom/deployments").query; q != "branch=dev&limit=5" {
		t.Errorf("query %q", q)
	}
	aligned(t, out, "BRANCH", "dep_3", "dev")
	aligned(t, out, "REF", "dep_3", "dev")
	aligned(t, out, "COMMIT", "dep_3", "4b825dc")
	if strings.Contains(out, "dep_1") {
		t.Errorf("another branch's deployment:\n%s", out)
	}

	out = h.run("", "deploy", "show", "dep_1").ok(t).stdout
	for _, want := range []string{"Branch:      main", "Instance:    moimroom"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy show lacks %q:\n%s", want, out)
		}
	}
}

// The live URL is the deployed instance's, not the app's.
func TestDeployBranchWatchShowsTheInstanceURL(t *testing.T) {
	f := newDeployFake(t)
	f.events = func(w http.ResponseWriter, r *http.Request, _ int) {
		s := dsse{w}
		s.snapshot(fixtureInstanceDeployment("dep_2", "dev", apiv1.DeploymentStatusBuilding))
		s.event(1, apiv1.DeploymentEventTypeDone, apiv1.DeploymentStatusHealthy, "", "")
		s.snapshot(fixtureInstanceDeployment("dep_2", "dev", apiv1.DeploymentStatusHealthy))
	}
	r := f.harness(t).run("", "deploy", "moimroom", "--branch", "dev", "--watch").ok(t)
	for _, want := range []string{
		"Deployment dep_2  app moimroom  instance moimroom--dev (branch dev)  dev (4b825dc) to gke, eks: [..] building\n",
		"Deployment dep_2  app moimroom  instance moimroom--dev (branch dev)  dev (4b825dc)  register by alice\n",
		"\n[ok] live at https://moimroom--dev.hopcharlie.dev\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestInstanceURL(t *testing.T) {
	app := fixtureRegisteredApp().Deploy
	old := *app // a server from before branch instances
	old.Instances = nil
	oldDep := fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy)
	oldDep.Branch, oldDep.Instance = "", ""
	for _, tc := range []struct {
		name string
		app  *apiv1.AppDeploy
		dep  apiv1.Deployment
		want string
	}{
		{"default instance", app, fixtureDeployment("dep_1", apiv1.DeploymentStatusHealthy), "https://moimroom.hopcharlie.dev"},
		{"added instance", app, fixtureInstanceDeployment("dep_3", "dev", apiv1.DeploymentStatusHealthy), "https://moimroom--dev.hopcharlie.dev"},
		{"instance the app does not list", app, fixtureInstanceDeployment("dep_4", "qa", apiv1.DeploymentStatusHealthy), ""},
		{"old server", &old, oldDep, "https://moimroom.hopcharlie.dev"},
	} {
		if got := instanceURL(tc.app, &tc.dep); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := deployTarget(&oldDep); got != "app moimroom" {
		t.Errorf("old server's deployment target %q", got)
	}
}
