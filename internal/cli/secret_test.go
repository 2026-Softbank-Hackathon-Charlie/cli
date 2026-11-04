package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeSecret struct {
	value, by string
	at        time.Time
}

// fakeSecretLimit stands in for the server's 50 secrets per scope.
const fakeSecretLimit = 5

// fixtureSecrets: two app-wide secrets, and moimroom--dev overrides one of
// them and has one of its own.
func fixtureSecrets() map[string]map[string]fakeSecret {
	return map[string]map[string]fakeSecret{
		"": {
			"ADMIN_TOKEN": {value: "adm-s3cret-value", by: "alice", at: t0},
			"HOST_CODE":   {value: "4821-app-value", by: "alice", at: t0.Add(time.Minute)},
		},
		"moimroom--dev": {
			"DEV_ONLY":  {value: "dev-only-value", by: "bob", at: t0.Add(2 * time.Minute)},
			"HOST_CODE": {value: "dev-code-value", by: "bob", at: t0.Add(3 * time.Minute)},
		},
	}
}

// fixtureSecretValues are the values of fixtureSecrets.
var fixtureSecretValues = []string{"adm-s3cret-value", "4821-app-value", "dev-only-value", "dev-code-value"}

// secretRoutes serves the secret routes of api/openapi.yaml for moimroom.
// It knows only the fixture's instances, so an instance added since is
// instance_not_found as if it had been removed meanwhile.
func (f *deployFake) secretRoutes(mux *http.ServeMux) {
	ready := func(w http.ResponseWriter, r *http.Request) bool {
		switch name := r.PathValue("name"); {
		case name == "shop":
			writeError(w, 409, "app_not_registered", "app shop is only in PLATFORM_APPS_FILE; register it from GitHub (POST /v1/apps) to deploy it")
		case name != "moimroom":
			writeError(w, 404, "app_not_found", "app not found")
		case f.secrets == nil:
			writeError(w, 503, "secrets_not_configured", "this server has no secrets key (PLATFORM_SECRETS_KEY), so it cannot store or read app secrets")
		default:
			return true
		}
		return false
	}
	known := func(w http.ResponseWriter, instance string) bool {
		if instance == "" || instance == "moimroom" || instance == "moimroom--dev" {
			return true
		}
		writeError(w, 404, "instance_not_found", "app moimroom has no instance "+instance)
		return false
	}
	mux.HandleFunc("GET /v1/apps/{name}/secrets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if ready(w, r) {
			writeJSON(w, 200, f.secretList())
		}
	})
	mux.HandleFunc("PUT /v1/apps/{name}/secrets", func(w http.ResponseWriter, r *http.Request) {
		var req apiv1.AppSecretsUpdate
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if !ready(w, r) || !known(w, str(req.Instance)) {
			return
		}
		scope := maps.Clone(f.secrets[str(req.Instance)])
		if scope == nil {
			scope = map[string]fakeSecret{}
		}
		if req.Set != nil {
			for n, v := range *req.Set {
				scope[n] = fakeSecret{value: v, by: "alice", at: t0.Add(time.Hour)}
			}
		}
		if req.Delete != nil {
			for _, n := range *req.Delete {
				delete(scope, n)
			}
		}
		if len(scope) > fakeSecretLimit {
			writeError(w, 400, "invalid_request", "too many secrets: at most 50 app-wide and 50 per instance; delete some first")
			return
		}
		f.secrets[str(req.Instance)] = scope
		writeJSON(w, 200, f.secretList())
	})
	mux.HandleFunc("GET /v1/apps/{name}/secrets/{secret}", func(w http.ResponseWriter, r *http.Request) {
		instance, name := r.URL.Query().Get("instance"), r.PathValue("secret")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !ready(w, r) || !known(w, instance) {
			return
		}
		s, ok := f.secrets[instance][name]
		if !ok {
			where := "app-wide"
			if instance != "" {
				where = "in instance " + instance
			}
			writeError(w, 404, "secret_not_found", fmt.Sprintf("app moimroom has no secret %q %s", name, where))
			return
		}
		v := apiv1.AppSecretValue{Name: name, Value: s.value, UpdatedAt: s.at, UpdatedBy: s.by}
		if instance != "" {
			v.Instance = &instance
		}
		writeJSON(w, 200, v)
	})
}

// secretList is f.secrets as the API lists them; f.mu is held.
func (f *deployFake) secretList() apiv1.AppSecrets {
	infos := func(m map[string]fakeSecret) []apiv1.AppSecretInfo {
		out := []apiv1.AppSecretInfo{}
		for _, n := range slices.Sorted(maps.Keys(m)) {
			out = append(out, apiv1.AppSecretInfo{Name: n, UpdatedAt: m[n].at, UpdatedBy: m[n].by})
		}
		return out
	}
	out := apiv1.AppSecrets{App: infos(f.secrets[""]), Instances: []apiv1.InstanceSecrets{}}
	for _, in := range slices.Sorted(maps.Keys(f.secrets)) {
		if in != "" && len(f.secrets[in]) > 0 {
			out.Instances = append(out.Instances, apiv1.InstanceSecrets{Instance: in, Secrets: infos(f.secrets[in])})
		}
	}
	return out
}

// stored is the value the fake keeps for name in instance ("" app-wide).
func (f *deployFake) stored(instance, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[instance][name]
	return s.value, ok
}

// noValues fails when a secret value is in r's output.
func noValues(t *testing.T, what string, r result, values ...string) {
	t.Helper()
	for _, v := range append(values, fixtureSecretValues...) {
		if strings.Contains(r.stdout, v) || strings.Contains(r.stderr, v) {
			t.Errorf("%s: the value %q is in the output\nstdout:\n%s\nstderr:\n%s", what, v, r.stdout, r.stderr)
		}
	}
}

// fakeTerminal makes the harness's stdin and stdout terminals or not, and
// typed what is entered at a hidden prompt.
func fakeTerminal(t *testing.T, stdin, stdout bool, typed string) {
	oldTerm, oldRead := isTerminal, readHidden
	t.Cleanup(func() { isTerminal, readHidden = oldTerm, oldRead })
	isTerminal = func(f any) bool {
		switch f.(type) {
		case *strings.Reader:
			return stdin
		case *bytes.Buffer:
			return stdout
		}
		return false
	}
	readHidden = func(context.Context, io.Reader) ([]byte, error) { return []byte(typed), nil }
}

// writeTemp writes content to a file in a new temporary directory.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	redeployDefault = "  charlie deploy moimroom --redeploy\n"
	redeployDev     = "  charlie deploy moimroom --redeploy --branch dev\n"
	redeployHead    = "Running instances keep the old values until they are deployed again; to apply the change now, without a build:\n"
)

func TestAppSecretList(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	r := h.run("", "app", "secret", "list", "moimroom").ok(t)
	want := "App-wide, for every instance that does not override them\n" +
		"NAME         UPDATED              BY\n" +
		"ADMIN_TOKEN  " + stamp(t0) + "  alice\n" +
		"HOST_CODE    " + stamp(t0.Add(time.Minute)) + "  alice\n" +
		"\nOverrides in instance moimroom--dev\n" +
		"NAME       UPDATED              BY\n" +
		"DEV_ONLY   " + stamp(t0.Add(2*time.Minute)) + "  bob\n" +
		"HOST_CODE  " + stamp(t0.Add(3*time.Minute)) + "  bob\n"
	if r.stdout != want || r.stderr != "" {
		t.Errorf("stdout:\n%s\nwant:\n%s\nstderr %q", r.stdout, want, r.stderr)
	}
	noValues(t, "list", r)
	if n := f.sent("GET", "/v1/apps/moimroom"); n != 0 {
		t.Errorf("read the app %d times without --instance", n)
	}

	r = h.run("", "--json", "app", "secret", "list", "moimroom").ok(t)
	f.mu.Lock()
	wantJSON := normalize(t, f.secretList())
	f.mu.Unlock()
	if got := decodeOut[apiv1.AppSecrets](t, r.stdout); !slices.EqualFunc(got.App, wantJSON.App, func(a, b apiv1.AppSecretInfo) bool { return a == b }) ||
		len(got.Instances) != 1 || got.Instances[0].Instance != "moimroom--dev" || len(got.Instances[0].Secrets) != 2 {
		t.Errorf("--json: %s", r.stdout)
	}
	noValues(t, "list --json", r)

	f.mu.Lock()
	f.secrets = map[string]map[string]fakeSecret{}
	f.mu.Unlock()
	if r := h.run("", "app", "secret", "list", "moimroom").ok(t); r.stdout != "No secrets in app moimroom. Set one with: charlie app secret set moimroom NAME\n" {
		t.Errorf("no secrets: %q", r.stdout)
	}
	f.mu.Lock()
	f.secrets = map[string]map[string]fakeSecret{"moimroom--dev": fixtureSecrets()["moimroom--dev"]}
	f.mu.Unlock()
	if r := h.run("", "app", "secret", "list", "moimroom").ok(t); !strings.HasPrefix(r.stdout, "No app-wide secrets.\n\nOverrides in instance moimroom--dev\n") {
		t.Errorf("only overrides:\n%s", r.stdout)
	}
}

func TestAppSecretListInstance(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	want := "Secrets of instance moimroom--dev (branch dev)\n" +
		"NAME         SOURCE         UPDATED              BY\n" +
		"ADMIN_TOKEN  app            " + stamp(t0) + "  alice\n" +
		"DEV_ONLY     instance-only  " + stamp(t0.Add(2*time.Minute)) + "  bob\n" +
		"HOST_CODE    override       " + stamp(t0.Add(3*time.Minute)) + "  bob\n"
	for _, arg := range []string{"dev", "moimroom--dev"} {
		r := h.run("", "app", "secret", "list", "moimroom", "--instance", arg).ok(t)
		if r.stdout != want {
			t.Errorf("--instance %s:\n%s\nwant:\n%s", arg, r.stdout, want)
		}
		noValues(t, "list --instance", r)
	}
	// The default instance is named after the app, or by its branch.
	want = "Secrets of instance moimroom (branch main)\n" +
		"NAME         SOURCE  UPDATED              BY\n" +
		"ADMIN_TOKEN  app     " + stamp(t0) + "  alice\n" +
		"HOST_CODE    app     " + stamp(t0.Add(time.Minute)) + "  alice\n"
	for _, arg := range []string{"moimroom", "main"} {
		if r := h.run("", "app", "secret", "list", "moimroom", "--instance", arg).ok(t); r.stdout != want {
			t.Errorf("--instance %s:\n%s\nwant:\n%s", arg, r.stdout, want)
		}
	}

	r := h.run("", "--json", "app", "secret", "list", "moimroom", "--instance", "dev").ok(t)
	e := decodeOut[effectiveSecrets](t, r.stdout)
	var sources []string
	for _, s := range e.Secrets {
		sources = append(sources, s.Name+"="+s.Source+"/"+s.UpdatedBy)
	}
	if e.Instance != "moimroom--dev" || strings.Join(sources, " ") != "ADMIN_TOKEN=app/alice DEV_ONLY=instance-only/bob HOST_CODE=override/bob" {
		t.Errorf("--json: %s", r.stdout)
	}
	noValues(t, "list --instance --json", r)

	f.mu.Lock()
	f.secrets = map[string]map[string]fakeSecret{}
	f.mu.Unlock()
	if r := h.run("", "app", "secret", "list", "moimroom", "--instance", "main").ok(t); r.stdout != "Instance moimroom gets no secrets.\n" {
		t.Errorf("no secrets: %q", r.stdout)
	}
	if r := h.run("", "--json", "app", "secret", "list", "moimroom", "--instance", "main").ok(t); !strings.Contains(r.stdout, `"secrets": []`) {
		t.Errorf("--json without secrets: %s", r.stdout)
	}

	for _, tc := range []struct{ args, want string }{
		{"moimroom --instance nope", `error: app moimroom has no instance or branch "nope"; list them with: charlie app branch list moimroom` + "\n"},
		{"moimroom --instance=", "error: --instance needs an instance name or a branch\n"},
		{"shop --instance dev", "error: app shop is only in the server's apps file, not registered from GitHub; it has no secrets\n"},
	} {
		before := f.sent("GET", "/v1/apps/moimroom/secrets") + f.sent("GET", "/v1/apps/shop/secrets")
		r := h.run("", append([]string{"app", "secret", "list"}, strings.Fields(tc.args)...)...)
		if r.code == 0 || !strings.HasPrefix(r.stderr, tc.want) || r.stdout != "" {
			t.Errorf("%s: exit %d\nstdout %q\nstderr %q", tc.args, r.code, r.stdout, r.stderr)
		}
		if f.sent("GET", "/v1/apps/moimroom/secrets")+f.sent("GET", "/v1/apps/shop/secrets") != before {
			t.Errorf("%s: listed the secrets anyway", tc.args)
		}
	}

	// A branch named like another instance is refused with how to name each.
	f.mu.Lock()
	f.added = append(f.added, apiv1.AppInstance{Name: "moimroom--moimroom-dev", Branch: "moimroom--dev"})
	f.mu.Unlock()
	r = h.run("", "app", "secret", "list", "moimroom", "--instance", "moimroom--dev")
	if want := `error: "moimroom--dev" is instance moimroom--dev (branch dev) and the branch of instance moimroom--moimroom-dev` +
		"; for moimroom--dev, name it by its branch: --instance dev" +
		"; for moimroom--moimroom-dev, name it by its instance name: --instance moimroom--moimroom-dev\n"; r.code != ExitError || r.stderr != want {
		t.Errorf("ambiguous: exit %d\nstderr %q\nwant   %q", r.code, r.stderr, want)
	}
}

// For --instance the default instance is allowed, and an argument that is
// ambiguous both ways is left to the console or the API.
func TestFindInstanceForSecrets(t *testing.T) {
	def := apiv1.AppInstance{Name: "moimroom", Branch: "main", Default: true}
	dev := apiv1.AppInstance{Name: "moimroom--dev", Branch: "dev"}
	for _, tc := range []struct {
		name    string
		ins     []apiv1.AppInstance
		arg     string
		want    string
		wantErr string
	}{
		{"default by name", []apiv1.AppInstance{def, dev}, "moimroom", "moimroom", ""},
		{"default by branch", []apiv1.AppInstance{def, dev}, "main", "moimroom", ""},
		{"added by branch", []apiv1.AppInstance{def, dev}, "dev", "moimroom--dev", ""},
		{"default name, added branch", []apiv1.AppInstance{def, {Name: "moimroom--moimroom", Branch: "moimroom"}}, "moimroom", "",
			`"moimroom" is the default instance moimroom (branch main) and the branch of instance moimroom--moimroom` +
				"; for moimroom, name it by its branch: --instance main" +
				"; for moimroom--moimroom, name it by its instance name: --instance moimroom--moimroom"},
		{"both ways ambiguous", []apiv1.AppInstance{def, {Name: "moimroom--moimroom", Branch: "moimroom"}, {Name: "moimroom--moimroom-moimroom", Branch: "moimroom--moimroom"}}, "moimroom--moimroom", "",
			`"moimroom--moimroom" is instance moimroom--moimroom (branch moimroom) and the branch of instance moimroom--moimroom-moimroom` +
				"; for moimroom--moimroom, use the console or the API with instance moimroom--moimroom" +
				"; for moimroom--moimroom-moimroom, name it by its instance name: --instance moimroom--moimroom-moimroom"},
	} {
		in, err := findInstance("moimroom", tc.ins, tc.arg, false)
		switch {
		case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
			t.Errorf("%s: error %v\nwant %s", tc.name, err, tc.wantErr)
		case tc.wantErr == "" && (err != nil || in.Name != tc.want):
			t.Errorf("%s: %+v, %v; want %s", tc.name, in, err, tc.want)
		}
	}
}

func TestAppSecretGet(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	// Only the value, so that it can be captured.
	r := h.run("", "app", "secret", "get", "moimroom", "HOST_CODE").ok(t)
	if r.stdout != "4821-app-value\n" || r.stderr != "" {
		t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if q := f.last(t, "GET", "/v1/apps/moimroom/secrets/HOST_CODE").query; q != "" {
		t.Errorf("query %q", q)
	}
	r = h.run("", "app", "secret", "get", "moimroom", "HOST_CODE", "--instance", "dev").ok(t)
	if r.stdout != "dev-code-value\n" || r.stderr != "" {
		t.Errorf("--instance: stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if q := f.last(t, "GET", "/v1/apps/moimroom/secrets/HOST_CODE").query; q != "instance=moimroom--dev" {
		t.Errorf("--instance: query %q", q)
	}

	r = h.run("", "--json", "app", "secret", "get", "moimroom", "HOST_CODE", "--instance", "moimroom--dev").ok(t)
	if v := decodeOut[apiv1.AppSecretValue](t, r.stdout); v.Name != "HOST_CODE" || v.Value != "dev-code-value" || str(v.Instance) != "moimroom--dev" || v.UpdatedBy != "bob" || r.stderr != "" {
		t.Errorf("--json: %s, stderr %q", r.stdout, r.stderr)
	}

	// A value is printed as it is, also when it looks like a status marker.
	f.mu.Lock()
	f.secrets[""]["MARKED"] = fakeSecret{value: "[ok] two  spaces\tand tab", by: "alice", at: t0}
	f.mu.Unlock()
	if r := h.run("", "--no-color", "app", "secret", "get", "moimroom", "MARKED").ok(t); r.stdout != "[ok] two  spaces\tand tab\n" {
		t.Errorf("value changed: %q", r.stdout)
	}

	// On a terminal, one warning line on stderr; stdout stays the value.
	fakeTerminal(t, false, true, "")
	r = h.run("", "app", "secret", "get", "moimroom", "HOST_CODE").ok(t)
	if r.stdout != "4821-app-value\n" || r.stderr != "warning: the value of HOST_CODE is shown on the screen\n" {
		t.Errorf("terminal: stdout %q, stderr %q", r.stdout, r.stderr)
	}
}

func TestAppSecretSetStdin(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	// moimroom--dev overrides HOST_CODE, so only the default instance gets
	// the new app-wide value.
	r := h.run("new-s3cret-1\n", "app", "secret", "set", "moimroom", "HOST_CODE", "--stdin").ok(t)
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"set":{"HOST_CODE":"new-s3cret-1"}}` {
		t.Errorf("body %s", body)
	}
	if want := "Set HOST_CODE app-wide in app moimroom.\n" + redeployHead + redeployDefault; r.stdout != want || r.stderr != "" {
		t.Errorf("stdout:\n%s\nwant:\n%s\nstderr %q", r.stdout, want, r.stderr)
	}
	noValues(t, "set --stdin", r, "new-s3cret-1")

	// A new app-wide name reaches every instance.
	r = h.run("new-s3cret-2", "app", "secret", "set", "moimroom", "NEW_KEY", "--stdin").ok(t)
	if want := "Set NEW_KEY app-wide in app moimroom.\n" + redeployHead + redeployDefault + redeployDev; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	// An override reaches its instance only.
	r = h.run("dev-s3cret-3\r\n", "app", "secret", "set", "moimroom", "HOST_CODE", "--instance", "dev", "--stdin").ok(t)
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"instance":"moimroom--dev","set":{"HOST_CODE":"dev-s3cret-3"}}` {
		t.Errorf("body %s", body)
	}
	if want := "Set HOST_CODE for instance moimroom--dev of app moimroom.\n" + redeployHead + redeployDev; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	noValues(t, "set --instance", r, "dev-s3cret-3")

	// Only the line break at the end goes.
	h.run("  spaced\t value  \n", "app", "secret", "set", "moimroom", "SPACED", "--stdin").ok(t)
	if v, _ := f.stored("", "SPACED"); v != "  spaced\t value  " {
		t.Errorf("stored %q", v)
	}
	h.run(strings.Repeat("x", 4096)+"\n", "app", "secret", "set", "moimroom", "LONGEST", "--stdin").ok(t)

	puts := f.sent("PUT", "/v1/apps/moimroom/secrets")
	for _, tc := range []struct{ stdin, want string }{
		{"line one\nline two\n", "error: --stdin reads one line, and stdin has more; use --from-file for a value of several lines\n"},
		{"", "error: the value of HOST_CODE is empty; nothing was set\n"},
		{"\n", "error: the value of HOST_CODE is empty; nothing was set\n"},
		{strings.Repeat("x", 4097), "error: the value of HOST_CODE is longer than 4096 bytes; nothing was set\n"},
		{strings.Repeat("x", 100000), "error: the value of HOST_CODE is longer than 4096 bytes; nothing was set\n"},
		{"bad\xff\xfe", "error: the value of HOST_CODE is not UTF-8 text; nothing was set\n"},
	} {
		r := h.run(tc.stdin, "app", "secret", "set", "moimroom", "HOST_CODE", "--stdin")
		if r.code != ExitError || r.stderr != tc.want || r.stdout != "" {
			t.Errorf("stdin %.20q: exit %d, stderr %q", tc.stdin, r.code, r.stderr)
		}
		noValues(t, "bad stdin", r, "line one", "line two")
	}
	if n := f.sent("PUT", "/v1/apps/moimroom/secrets") - puts; n != 0 {
		t.Errorf("sent %d PUTs for bad values", n)
	}

	// --json prints the names after the change; the rest goes to stderr.
	r = h.run("new-s3cret-4\n", "--json", "app", "secret", "set", "moimroom", "HOST_CODE", "--stdin").ok(t)
	if s := decodeOut[apiv1.AppSecrets](t, r.stdout); len(s.App) == 0 || r.stderr != redeployHead+redeployDefault {
		t.Errorf("--json: stdout %s\nstderr %q", r.stdout, r.stderr)
	}
	noValues(t, "set --json", r, "new-s3cret-4")
}

func TestAppSecretSetPrompt(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	// Without a terminal and without --stdin or --from-file, nothing is
	// asked or sent.
	r := h.run("piped-value\n", "app", "secret", "set", "moimroom", "HOST_CODE")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "stdin is not a terminal, so charlie cannot ask for the value; pass --stdin") || len(f.requests()) != 0 {
		t.Errorf("no terminal: exit %d, stderr %q, %d requests", r.code, r.stderr, len(f.requests()))
	}

	fakeTerminal(t, true, false, "typed-s3cret")
	r = h.run("", "app", "secret", "set", "moimroom", "HOST_CODE").ok(t)
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"set":{"HOST_CODE":"typed-s3cret"}}` {
		t.Errorf("body %s", body)
	}
	if r.stderr != "Value of HOST_CODE (not shown): \n" {
		t.Errorf("prompt %q", r.stderr)
	}
	noValues(t, "prompt", r, "typed-s3cret")

	fakeTerminal(t, true, false, "")
	if r := h.run("", "app", "secret", "set", "moimroom", "HOST_CODE"); r.code != ExitError || !strings.HasSuffix(r.stderr, "error: the value of HOST_CODE is empty; nothing was set\n") {
		t.Errorf("empty: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestAppSecretSetFromFile(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	// The file as it is, without one line break at its end.
	for content, want := range map[string]string{
		"line1\nline2\n":   "line1\nline2",
		"pem\r\n":          "pem",
		"two breaks\n\n":   "two breaks\n",
		"no break":         "no break",
		" keep spaces \t ": " keep spaces \t ",
	} {
		r := h.run("", "app", "secret", "set", "moimroom", "CERT", "--from-file", writeTemp(t, "cert.pem", content)).ok(t)
		if v, _ := f.stored("", "CERT"); v != want {
			t.Errorf("%q: stored %q, want %q", content, v, want)
		}
		noValues(t, "--from-file", r, "line1", "line2", "keep spaces")
	}
	h.run("", "app", "secret", "set", "moimroom", "CERT", "--from-file", writeTemp(t, "max", strings.Repeat("a", 4096)+"\n")).ok(t)

	puts := f.sent("PUT", "/v1/apps/moimroom/secrets")
	big := writeTemp(t, "big", strings.Repeat("a", 4097))
	for file, want := range map[string]string{
		big:                                 "error: " + big + " holds more than 4096 bytes, the most a secret value can have\n",
		writeTemp(t, "empty", ""):           "error: the value of CERT is empty; nothing was set\n",
		writeTemp(t, "bin", "\x00\xff\x01"): "error: the value of CERT is not UTF-8 text; nothing was set\n",
	} {
		if r := h.run("", "app", "secret", "set", "moimroom", "CERT", "--from-file", file); r.code != ExitError || r.stderr != want {
			t.Errorf("%s: exit %d, stderr %q", file, r.code, r.stderr)
		}
	}
	if r := h.run("", "app", "secret", "set", "moimroom", "CERT", "--from-file", filepath.Join(t.TempDir(), "missing")); r.code != ExitError || !strings.HasPrefix(r.stderr, "error: open ") {
		t.Errorf("missing file: exit %d, stderr %q", r.code, r.stderr)
	}
	if n := f.sent("PUT", "/v1/apps/moimroom/secrets") - puts; n != 0 {
		t.Errorf("sent %d PUTs for bad files", n)
	}
}

func TestAppSecretSetEnvFile(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	file := writeTemp(t, "secrets.env", "# moimroom\nHOST_CODE=env-value-1\n\n  # indented comment\r\nADMIN_TOKEN=a=b \"quoted\"  \r\n")
	r := h.run("", "app", "secret", "set", "moimroom", "--env-file", file).ok(t)
	if n := f.sent("PUT", "/v1/apps/moimroom/secrets"); n != 1 {
		t.Errorf("%d PUTs, want 1", n)
	}
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"set":{"ADMIN_TOKEN":"a=b \"quoted\"  ","HOST_CODE":"env-value-1"}}` {
		t.Errorf("body %s", body)
	}
	if want := "Set ADMIN_TOKEN, HOST_CODE app-wide in app moimroom.\n" + redeployHead + redeployDefault + redeployDev; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	noValues(t, "--env-file", r, "env-value-1", "quoted")

	h.run("", "app", "secret", "set", "moimroom", "--env-file", writeTemp(t, "dev.env", "DEV_ONLY=dev-env-2\n"), "--instance", "dev").ok(t)
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"instance":"moimroom--dev","set":{"DEV_ONLY":"dev-env-2"}}` {
		t.Errorf("--instance body %s", body)
	}

	// Every bad line, by number, and no value.
	puts := f.sent("PUT", "/v1/apps/moimroom/secrets")
	bad := writeTemp(t, "bad.env", "GOOD=fine-value\njust-a-secret-line\nlower=bad-name-value\nPORT=8080\nGOOD=again-value\nEMPTY=\nS3_BUCKET=bucket-value\nSPACE =space-value\n")
	r = h.run("", "app", "secret", "set", "moimroom", "--env-file", bad)
	want := "error: " + bad + ": nothing was set:\n" +
		"  line 2: want NAME=value\n" +
		`  line 3: secret name "lower" must match ^[A-Z_][A-Z0-9_]*$` + "\n" +
		"  line 4: secret name PORT is set by the platform (PORT, DATABASE_URL and S3_* are reserved)\n" +
		"  line 5: GOOD is already set on line 1\n" +
		"  line 6: the value of EMPTY is empty\n" +
		"  line 7: secret name S3_BUCKET is set by the platform (PORT, DATABASE_URL and S3_* are reserved)\n" +
		`  line 8: secret name "SPACE " must match ^[A-Z_][A-Z0-9_]*$` + "\n"
	if r.code != ExitError || r.stderr != want || r.stdout != "" {
		t.Errorf("bad lines: exit %d\nstderr:\n%s\nwant:\n%s", r.code, r.stderr, want)
	}
	noValues(t, "bad env file", r, "fine-value", "just-a-secret-line", "bad-name-value", "8080", "again-value", "bucket-value", "space-value")

	var many strings.Builder
	for i := range 51 {
		fmt.Fprintf(&many, "KEY_%d=v\n", i)
	}
	for file, want := range map[string]string{
		writeTemp(t, "comments.env", "# only\n\n"): "has no NAME=value lines; nothing was set\n",
		writeTemp(t, "many.env", many.String()):    "sets 51 secrets; one change sets at most 50\n",
	} {
		if r := h.run("", "app", "secret", "set", "moimroom", "--env-file", file); r.code != ExitError || !strings.HasSuffix(r.stderr, want) {
			t.Errorf("%s: exit %d, stderr %q", file, r.code, r.stderr)
		}
	}
	if n := f.sent("PUT", "/v1/apps/moimroom/secrets") - puts; n != 0 {
		t.Errorf("sent %d PUTs for bad env files", n)
	}

	// The server's own 400 still comes through.
	r = h.run("", "app", "secret", "set", "moimroom", "--env-file", writeTemp(t, "more.env", "K1=v1-value\nK2=v2-value\nK3=v3-value\nK4=v4-value\n"))
	if r.code != ExitError || r.stderr != "error: invalid_request: too many secrets: at most 50 app-wide and 50 per instance; delete some first (HTTP 400)\n" {
		t.Errorf("server 400: exit %d, stderr %q", r.code, r.stderr)
	}
	noValues(t, "server 400", r, "v1-value", "v4-value")
}

func TestAppSecretRm(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	// Without a terminal it does not ask; --yes is required.
	r := h.run("yes\n", "app", "secret", "rm", "moimroom", "HOST_CODE")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "stdin is not a terminal, so charlie cannot ask; pass --yes to delete without asking") || len(f.requests()) != 0 {
		t.Errorf("no terminal: exit %d, stderr %q, %d requests", r.code, r.stderr, len(f.requests()))
	}

	r = h.run("", "app", "secret", "rm", "moimroom", "ADMIN_TOKEN", "HOST_CODE", "--yes").ok(t)
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"delete":["ADMIN_TOKEN","HOST_CODE"]}` {
		t.Errorf("body %s", body)
	}
	// moimroom--dev overrides HOST_CODE but not ADMIN_TOKEN.
	if want := "Deleted ADMIN_TOKEN, HOST_CODE app-wide in app moimroom.\n" + redeployHead + redeployDefault + redeployDev; r.stdout != want || r.stderr != "" {
		t.Errorf("stdout:\n%s\nwant:\n%s\nstderr %q", r.stdout, want, r.stderr)
	}
	if _, ok := f.stored("", "HOST_CODE"); ok {
		t.Error("HOST_CODE is still set")
	}

	// On a terminal it asks for yes.
	fakeTerminal(t, true, false, "")
	puts := f.sent("PUT", "/v1/apps/moimroom/secrets")
	for _, answer := range []string{"no\n", "y\n", ""} {
		r := h.run(answer, "app", "secret", "rm", "moimroom", "HOST_CODE", "--instance", "dev")
		if r.code != ExitError || !strings.HasSuffix(r.stderr, "nothing was deleted; type yes to confirm, or pass --yes\n") {
			t.Errorf("answer %q: exit %d, stderr %q", answer, r.code, r.stderr)
		}
	}
	if n := f.sent("PUT", "/v1/apps/moimroom/secrets") - puts; n != 0 {
		t.Errorf("deleted without confirmation: %d PUTs", n)
	}
	r = h.run("yes\n", "app", "secret", "rm", "moimroom", "HOST_CODE", "--instance", "dev").ok(t)
	if want := "This deletes HOST_CODE for instance moimroom--dev of app moimroom. The values cannot be recovered.\nType yes to confirm: \n"; r.stderr != want {
		t.Errorf("prompt %q\nwant   %q", r.stderr, want)
	}
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"delete":["HOST_CODE"],"instance":"moimroom--dev"}` {
		t.Errorf("body %s", body)
	}
	if want := "Deleted HOST_CODE for instance moimroom--dev of app moimroom.\n" + redeployHead + redeployDev; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	// Names that are not set are skipped with a note.
	r = h.run("", "app", "secret", "rm", "moimroom", "NOPE", "DEV_ONLY", "--instance", "moimroom--dev", "-y").ok(t)
	if r.stderr != "note: NOPE is not set for instance moimroom--dev of app moimroom; skipping it\n" {
		t.Errorf("stderr %q", r.stderr)
	}
	if body := string(f.last(t, "PUT", "/v1/apps/moimroom/secrets").body); body != `{"delete":["DEV_ONLY"],"instance":"moimroom--dev"}` {
		t.Errorf("body %s", body)
	}
	puts = f.sent("PUT", "/v1/apps/moimroom/secrets")
	r = h.run("", "app", "secret", "rm", "moimroom", "NOPE", "--yes")
	if r.code != ExitError || !strings.HasSuffix(r.stderr, "error: nothing to delete; see the secrets with: charlie app secret list moimroom\n") || f.sent("PUT", "/v1/apps/moimroom/secrets") != puts {
		t.Errorf("nothing to delete: exit %d, stderr %q", r.code, r.stderr)
	}

	// Every instance overrides the name: nobody needs a redeploy.
	f.mu.Lock()
	f.secrets = map[string]map[string]fakeSecret{
		"":              {"K": {value: "k-app-value"}},
		"moimroom":      {"K": {value: "k-default-value"}},
		"moimroom--dev": {"K": {value: "k-dev-value"}},
	}
	f.mu.Unlock()
	r = h.run("", "app", "secret", "rm", "moimroom", "K", "--yes").ok(t)
	if want := "Deleted K app-wide in app moimroom.\nEvery instance overrides these secrets, so none of them gets the app-wide values.\n"; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	noValues(t, "rm", r, "k-app-value", "k-default-value", "k-dev-value")
}

// Names are checked as the server would, before anything is sent.
func TestAppSecretValidation(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	names := make([]string, 51)
	for i := range names {
		names[i] = fmt.Sprintf("K_%d", i)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "moimroom", "host_code"}, `secret name "host_code" must match ^[A-Z_][A-Z0-9_]*$`},
		{[]string{"get", "moimroom", "1ABC"}, `secret name "1ABC" must match ^[A-Z_][A-Z0-9_]*$`},
		{[]string{"get", "moimroom", strings.Repeat("A", 65)}, `secret name "AAAAAAAAAAAAAAAAAAAA"... is longer than 64 characters`},
		{[]string{"set", "moimroom", "PORT", "--stdin"}, "secret name PORT is set by the platform (PORT, DATABASE_URL and S3_* are reserved)"},
		{[]string{"set", "moimroom", "DATABASE_URL", "--stdin"}, "secret name DATABASE_URL is set by the platform"},
		{[]string{"rm", "moimroom", "S3_ACCESS_KEY", "--yes"}, "secret name S3_ACCESS_KEY is set by the platform"},
		{[]string{"rm", "moimroom", "A", "B", "A", "--yes"}, "A is given twice"},
		{append([]string{"rm", "moimroom", "--yes"}, names...), "51 names; one change deletes at most 50"},
		{[]string{"rm", "moimroom"}, "charlie app secret rm needs APP NAME..."},
		{[]string{"set", "moimroom"}, "charlie app secret set needs NAME, or --env-file FILE"},
		{[]string{"set"}, "charlie app secret set needs APP NAME, or APP --env-file FILE"},
		{[]string{"set", "moimroom", "A", "--stdin", "--from-file", "x"}, "use only one of --stdin, --from-file and --env-file"},
		{[]string{"set", "moimroom", "A", "--env-file", "x"}, "--env-file takes the names from the file; leave NAME out"},
		{[]string{"set", "moimroom", "A", "--from-file="}, "--from-file needs a file"},
		{[]string{"get", "moimroom", "A", "--instance="}, "--instance needs an instance name or a branch"},
		// The value never goes on the command line, and is not printed back.
		{[]string{"set", "moimroom", "HOST_CODE", "hunter2-value"}, "charlie app secret set takes APP NAME only: the value is never taken from the command line, where it would stay in your shell history"},
		{[]string{"set", "moimroom", "HOST_CODE=hunter2-value", "--stdin"}, `NAME has no "=": the value is never taken from the command line`},
		{[]string{"get", "moimroom", "HOST_CODE=hunter2-value"}, `a secret name has no "="`},
	} {
		r := h.run("", append([]string{"app", "secret"}, tc.args...)...)
		if r.code != ExitUsage || !strings.HasPrefix(r.stderr, "error: "+tc.want) || r.stdout != "" {
			t.Errorf("%v: exit %d\nstderr %q\nwant   %q", tc.args, r.code, r.stderr, tc.want)
		}
		noValues(t, strings.Join(tc.args, " "), r, "hunter2-value")
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("sent %d requests for bad command lines: %v", len(reqs), reqs)
	}
}

func TestAppSecretErrors(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)

	for _, tc := range []struct {
		stdin string
		args  string
		want  string
	}{
		{"", "get moimroom DEV_ONLY", "error: secret_not_found: app moimroom has no secret \"DEV_ONLY\" app-wide (HTTP 404)\n" +
			"See the secrets with: charlie app secret list moimroom\n"},
		{"", "get moimroom ADMIN_TOKEN --instance dev", "error: secret_not_found: app moimroom has no secret \"ADMIN_TOKEN\" in instance moimroom--dev (HTTP 404)\n" +
			"Without --instance, get reads the app-wide value; see what instance moimroom--dev gets with: charlie app secret list moimroom --instance moimroom--dev\n"},
		{"", "get nope HOST_CODE", "error: app_not_found: app not found (HTTP 404)\n"},
		{"", "list shop", "error: app_not_registered: app shop is only in PLATFORM_APPS_FILE; register it from GitHub (POST /v1/apps) to deploy it (HTTP 409)\n"},
		{"", "get shop HOST_CODE", "error: app_not_registered: app shop is only in PLATFORM_APPS_FILE; register it from GitHub (POST /v1/apps) to deploy it (HTTP 409)\n"},
		{"v-value\n", "set shop HOST_CODE --stdin", "error: app shop is only in the server's apps file, not registered from GitHub; it has no secrets\n"},
		{"", "rm nope HOST_CODE --yes", "error: app_not_found: app not found (HTTP 404)\n"},
	} {
		r := h.run(tc.stdin, append([]string{"app", "secret"}, strings.Fields(tc.args)...)...)
		if r.code != ExitError || r.stderr != tc.want || r.stdout != "" {
			t.Errorf("%s: exit %d\nstderr %q\nwant   %q", tc.args, r.code, r.stderr, tc.want)
		}
	}
	if n := f.sent("PUT", "/v1/apps/shop/secrets"); n != 0 {
		t.Errorf("set a file app's secret: %d PUTs", n)
	}

	// An instance removed after charlie found it.
	f.mu.Lock()
	f.added = append(f.added, apiv1.AppInstance{Name: "moimroom--qa", Branch: "qa"})
	f.mu.Unlock()
	r := h.run("qa-value\n", "app", "secret", "set", "moimroom", "HOST_CODE", "--instance", "qa", "--stdin")
	if want := "error: instance_not_found: app moimroom has no instance moimroom--qa (HTTP 404)\nSee the instances with: charlie app branch list moimroom\n"; r.code != ExitError || r.stderr != want {
		t.Errorf("instance_not_found: exit %d\nstderr %q\nwant   %q", r.code, r.stderr, want)
	}

	// A server without a secrets key.
	f.mu.Lock()
	f.secrets = nil
	f.mu.Unlock()
	const off = "error: secrets_not_configured: this server has no secrets key (PLATFORM_SECRETS_KEY), so it cannot store or read app secrets (HTTP 503)\n" +
		"App secrets are off on this server until an administrator gives it a secrets key (PLATFORM_SECRETS_KEY).\n"
	for _, args := range []string{"list moimroom", "list moimroom --instance dev", "get moimroom HOST_CODE", "set moimroom HOST_CODE --stdin", "rm moimroom HOST_CODE --yes"} {
		r := h.run("off-value\n", append([]string{"app", "secret"}, strings.Fields(args)...)...)
		if r.code != ExitError || r.stderr != off || r.stdout != "" {
			t.Errorf("%s: exit %d\nstderr %q", args, r.code, r.stderr)
		}
		noValues(t, args, r, "off-value")
	}
}

func TestDeployRedeploy(t *testing.T) {
	f := newDeployFake(t)
	h := f.harness(t)
	const post = "/v1/apps/moimroom/deployments"

	r := h.run("", "deploy", "moimroom", "--redeploy").ok(t)
	if body := string(f.last(t, "POST", post).body); body != `{"redeploy":true}` {
		t.Errorf("body %s", body)
	}
	if want := "Queued redeploy dep_2 of moimroom at main (9fceb02) to gke, eks.\nWatch it with: charlie deploy watch dep_2\n"; r.stdout != want {
		t.Errorf("stdout %q\nwant   %q", r.stdout, want)
	}
	r = h.run("", "deploy", "moimroom", "--redeploy", "--branch", "dev", "--env", "eks").ok(t)
	if body := string(f.last(t, "POST", post).body); body != `{"branch":"dev","environments":["eks"],"redeploy":true}` {
		t.Errorf("body %s", body)
	}
	if !strings.HasPrefix(r.stdout, "Queued redeploy dep_2 of moimroom--dev at dev (4b825dc) to eks.\n") {
		t.Errorf("stdout %q", r.stdout)
	}
	r = h.run("", "--json", "deploy", "moimroom", "--redeploy").ok(t)
	if d := decodeOut[apiv1.Deployment](t, r.stdout); d.Trigger != apiv1.DeploymentTriggerRedeploy {
		t.Errorf("--json trigger %q", d.Trigger)
	}

	posts := f.sent("POST", post)
	for _, ref := range []string{"main", ""} {
		r := h.run("", "deploy", "moimroom", "--redeploy", "--ref", ref)
		if r.code != ExitUsage || !strings.HasPrefix(r.stderr, "error: --ref cannot be used with --redeploy") {
			t.Errorf("--ref %q: exit %d, stderr %q", ref, r.code, r.stderr)
		}
	}
	if n := f.sent("POST", post) - posts; n != 0 {
		t.Errorf("sent %d deployments with --ref and --redeploy", n)
	}

	for _, tc := range []struct{ code, args, want string }{
		{"nothing_to_redeploy", "--branch dev", "error: nothing_to_redeploy: instance moimroom--dev has no healthy deployment to deploy again (HTTP 409)\n" +
			"A redeploy repeats the instance's last healthy deployment, and it has none; deploy it with a build: charlie deploy moimroom --branch dev\n"},
		{"nothing_to_redeploy", "", "error: nothing_to_redeploy: instance moimroom has no healthy deployment to deploy again (HTTP 409)\n" +
			"A redeploy repeats the instance's last healthy deployment, and it has none; deploy it with a build: charlie deploy moimroom\n"},
		{"deployment_in_progress", "", "error: deployment_in_progress: instance moimroom has a deployment that has not finished (HTTP 409)\n" +
			"Wait until that deployment ends, then redeploy; see it with: charlie deploy list moimroom\n"},
		{"deployment_in_progress", "--branch dev --watch", "error: deployment_in_progress: instance moimroom--dev has a deployment that has not finished (HTTP 409)\n" +
			"Wait until that deployment ends, then redeploy; see it with: charlie deploy list moimroom --branch dev\n"},
	} {
		f.mu.Lock()
		f.redeployCode = tc.code
		f.mu.Unlock()
		r := h.run("", append([]string{"deploy", "moimroom", "--redeploy"}, strings.Fields(tc.args)...)...)
		if r.code != ExitError || r.stderr != tc.want || r.stdout != "" {
			t.Errorf("%s %s: exit %d\nstderr %q\nwant   %q", tc.code, tc.args, r.code, r.stderr, tc.want)
		}
	}
}

func TestRedeployTrigger(t *testing.T) {
	d := fixtureDeployment("dep_5", apiv1.DeploymentStatusHealthy)
	d.Trigger = apiv1.DeploymentTriggerRedeploy
	aligned(t, renderDeployments([]apiv1.Deployment{d}, t0), "TRIGGER", "dep_5", "redeploy")
	if out := renderDeployment(&d, t0); !strings.Contains(out, "Trigger:     redeploy by alice\n") {
		t.Errorf("deploy show:\n%s", out)
	}
}
