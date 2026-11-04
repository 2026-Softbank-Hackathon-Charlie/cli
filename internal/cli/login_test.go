package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func (h *harness) configFile() string  { return filepath.Join(h.dir, "charlie", configFile) }
func (h *harness) pendingFile() string { return filepath.Join(h.dir, "charlie", pendingFile) }

// noSecrets fails when the output shows a device code or a token.
func noSecrets(t *testing.T, r result) {
	t.Helper()
	for _, s := range []string{"-secret", "cli-token-for-"} {
		if strings.Contains(r.stdout+r.stderr, s) {
			t.Errorf("%q printed:\nstdout:\n%s\nstderr:\n%s", s, r.stdout, r.stderr)
		}
	}
}

func readFile[T any](t *testing.T, path string) T {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (%v)", path, err)
	}
}

func private(t *testing.T, path string) {
	t.Helper()
	// Windows has no Unix permission bits; os.Stat reports 0666 there.
	if runtime.GOOS == "windows" {
		return
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: mode %v (%v), want 0600", path, fi.Mode().Perm(), err)
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: mode %v, want 0700", filepath.Dir(path), fi.Mode().Perm())
	}
}

// polls returns the device codes polled, in order.
func (f *fakeServer) pollsMade() []string {
	var codes []string
	for _, r := range f.requests() {
		if r.method == "POST" && r.path == "/v1/auth/device/token" {
			var req struct{ DeviceCode string }
			_ = json.Unmarshal(r.body, &req)
			codes = append(codes, req.DeviceCode)
		}
	}
	return codes
}

func (f *fakeServer) startsMade() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func instructions(code string, expires time.Time, in string) string {
	return "Open https://console.hopcharlie.dev/#device, log in as an administrator and enter this code:\n\n    " + code + "\n\n" +
		"The code expires at " + expires.Local().Format("15:04") + " (" + in + "). The administrator picks the role this CLI gets.\n"
}

func TestDeviceLogin(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = pollScript("pending", "pending", "approved")
	h := newHarness(t, nil)

	r := h.run("", "login", "--server", f.URL+"/").ok(t)
	noSecrets(t, r)
	want := instructions("BCDF-GHJK", t0.Add(10*time.Minute), "in 10 minutes") +
		`Waiting for approval... (Ctrl-C stops waiting; "charlie login --server ` + f.URL + `" resumes)` + "\n" +
		"Logged in to " + f.URL + " as alice (agent) until " + stamp(fakeTokenExpiry) + ".\n" +
		"Saved to " + h.configFile() + "\n"
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	start := f.last(t, "POST", "/v1/auth/device")
	host, _ := os.Hostname()
	var body struct{ Client string }
	_ = json.Unmarshal(start.body, &body)
	if body.Client != deviceClient("1.2.3", host) || !strings.HasPrefix(body.Client, "charlie 1.2.3 on ") {
		t.Errorf("client = %q", body.Client)
	}
	for _, req := range f.requests() {
		if a := req.header.Get("Authorization"); a != "" {
			t.Errorf("%s %s sent Authorization %q", req.method, req.path, a)
		}
	}
	if got := f.pollsMade(); !slices.Equal(got, []string{"dev-secret-0", "dev-secret-0", "dev-secret-0"}) {
		t.Errorf("polls = %v", got)
	}
	if !slices.Equal(h.slept, []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}) {
		t.Errorf("slept %v", h.slept)
	}

	cfg := readFile[config](t, h.configFile())
	if cfg.Server != f.URL || cfg.Token != fakeCLIToken("dev-secret-0") || cfg.ExpiresAt == nil || !cfg.ExpiresAt.Equal(fakeTokenExpiry) {
		t.Errorf("config = %+v", cfg)
	}
	private(t, h.configFile())
	gone(t, h.pendingFile())
	if m, _ := filepath.Glob(filepath.Join(h.dir, "charlie", "*-*.json")); len(m) != 0 {
		t.Errorf("temporary files left: %v", m)
	}

	r = h.run("", "whoami").ok(t)
	if want := "alice (operator) on " + f.URL + "\nThe saved login expires at " + stamp(fakeTokenExpiry) + ".\n"; r.stdout != want {
		t.Errorf("whoami = %q, want %q", r.stdout, want)
	}
	if got := f.last(t, "GET", "/v1/me").header.Get("Authorization"); got != "Bearer "+fakeCLIToken("dev-secret-0") {
		t.Errorf("whoami sent %q", got)
	}
	var me map[string]any
	if err := json.Unmarshal([]byte(h.run("", "--json", "whoami").ok(t).stdout), &me); err != nil ||
		me["name"] != "alice" || me["expiresAt"] != fakeTokenExpiry.Format(time.RFC3339) {
		t.Errorf("whoami --json = %v (%v)", me, err)
	}
	// CHARLIE_TOKEN is not the saved login, so its expiry is unknown.
	h.env["CHARLIE_TOKEN"] = "static"
	if r := h.run("", "whoami").ok(t); strings.Contains(r.stdout, "expires") {
		t.Errorf("whoami with CHARLIE_TOKEN = %q", r.stdout)
	}
}

func TestLoginNoWaitThenResume(t *testing.T) {
	f := newFake(t, "")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})

	r := h.run("", "login", "--no-wait").ok(t)
	noSecrets(t, r)
	if want := instructions("BCDF-GHJK", t0.Add(10*time.Minute), "in 10 minutes") +
		`After approving, run "charlie login --server ` + f.URL + `" to finish.` + "\n"; r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	if n := len(f.pollsMade()); n != 0 || len(h.slept) != 0 {
		t.Errorf("--no-wait polled %d times, slept %v", n, h.slept)
	}
	p := readFile[pendingLogin](t, h.pendingFile())
	if want := (pendingLogin{Server: f.URL, DeviceCode: "dev-secret-0", UserCode: "BCDF-GHJK",
		VerificationURI: "https://console.hopcharlie.dev/#device", ExpiresAt: t0.Add(10 * time.Minute), Interval: 5}); !reflect.DeepEqual(normalize(t, p), normalize(t, want)) {
		t.Errorf("pending = %+v", p)
	}
	private(t, h.pendingFile())
	gone(t, h.configFile())

	// Still pending a few minutes later: the same code again, then the wait.
	h.now = t0.Add(3 * time.Minute)
	f.devicePoll = pollScript("pending", "pending", "approved")
	r = h.run("", "login").ok(t)
	noSecrets(t, r)
	if !strings.HasPrefix(r.stdout, instructions("BCDF-GHJK", t0.Add(10*time.Minute), "in 7 minutes")) ||
		!strings.Contains(r.stdout, "Logged in to "+f.URL+" as alice (agent)") {
		t.Errorf("resume stdout:\n%s", r.stdout)
	}
	if n := f.startsMade(); n != 1 {
		t.Errorf("%d device logins started", n)
	}
	if got := f.pollsMade(); !slices.Equal(got, []string{"dev-secret-0", "dev-secret-0", "dev-secret-0"}) {
		t.Errorf("polls = %v", got)
	}
	gone(t, h.pendingFile())

	// The bot's way: --no-wait, a person approves, then "charlie login"
	// finishes at once.
	f.devicePoll = pollScript("approved")
	h.run("", "login", "--no-wait").ok(t)
	h.slept = nil
	r = h.run("", "login").ok(t)
	if strings.Contains(r.stdout, "LMNP-QRST") || !strings.HasPrefix(r.stdout, "Logged in to "+f.URL) {
		t.Errorf("finish stdout:\n%s", r.stdout)
	}
	if n := f.startsMade(); n != 2 || len(h.slept) != 0 {
		t.Errorf("%d device logins started, slept %v", n, h.slept)
	}
	if got := f.pollsMade(); got[len(got)-1] != "dev-secret-1" {
		t.Errorf("polls = %v", got)
	}
	if cfg := readFile[config](t, h.configFile()); cfg.Token != fakeCLIToken("dev-secret-1") {
		t.Errorf("config = %+v", cfg)
	}
	gone(t, h.pendingFile())
}

// The hints say "charlie login --server URL" whenever plain "charlie login"
// would go elsewhere: to the saved login's server, else the default.
func TestLoginHintsNameTheServer(t *testing.T) {
	a, b := newFake(t, ""), newFake(t, "")
	h := newHarness(t, nil)
	exp := fakeTokenExpiry
	if err := saveConfig(h.configFile(), config{Server: a.URL, Token: "a-token", ExpiresAt: &exp}, true); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "login", "--no-wait").ok(t); !strings.Contains(r.stdout, `run "charlie login" to finish`) {
		t.Errorf("saved server:\n%s", r.stdout)
	}
	h.env["CHARLIE_SERVER"] = b.URL
	if r := h.run("", "login", "--no-wait").ok(t); !strings.Contains(r.stdout, `run "charlie login --server `+b.URL+`" to finish`) {
		t.Errorf("CHARLIE_SERVER elsewhere:\n%s", r.stdout)
	}

	h2 := newHarness(t, map[string]string{"CHARLIE_SERVER": b.URL})
	if r := h2.run("", "whoami"); r.code != ExitError ||
		!strings.Contains(r.stderr, `not logged in to `+b.URL+`; run "charlie login --server `+b.URL+`" or set CHARLIE_TOKEN`) {
		t.Errorf("whoami: exit %d, %q", r.code, r.stderr)
	}
}

// A code someone already approved: --no-wait saves the token instead of
// printing the code, since the server hands it out only once.
func TestLoginNoWaitSavesApprovedPending(t *testing.T) {
	for _, json := range []bool{false, true} {
		f := newFake(t, "")
		f.devicePoll = pollScript("approved")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		oldPending(t, h, f.URL)
		args := []string{"login", "--no-wait"}
		if json {
			args = append([]string{"--json"}, args...)
		}
		r := h.run("", args...).ok(t)
		noSecrets(t, r)
		if json {
			if res := decodeOut[map[string]any](t, r.stdout); res["name"] != "alice" || res["role"] != "agent" || res["userCode"] != nil {
				t.Errorf("--json = %v", res)
			}
		} else if !strings.HasPrefix(r.stdout, "Logged in to "+f.URL+" as alice (agent)") {
			t.Errorf("stdout:\n%s", r.stdout)
		}
		if cfg := readFile[config](t, h.configFile()); cfg.Token != fakeCLIToken("old-secret") || f.startsMade() != 0 {
			t.Errorf("config %+v, starts %d", cfg, f.startsMade())
		}
		gone(t, h.pendingFile())
	}
}

func TestLoginDamagedFiles(t *testing.T) {
	f := newFake(t, "")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	if err := os.MkdirAll(filepath.Dir(h.pendingFile()), 0o700); err != nil {
		t.Fatal(err)
	}

	// A damaged pending login is replaced by a new one.
	if err := os.WriteFile(h.pendingFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run("", "login", "--no-wait").ok(t)
	if p := readFile[pendingLogin](t, h.pendingFile()); p.DeviceCode != "dev-secret-0" || f.startsMade() != 1 {
		t.Errorf("pending %+v, starts %d", p, f.startsMade())
	}

	// A damaged config.json does not stop the login that replaces it.
	for _, bad := range []string{"{not json", `{"server":"not a url","token":"x"}`} {
		if err := os.WriteFile(h.configFile(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		f.devicePoll = pollScript("approved")
		r := h.run("", "login").ok(t)
		if !strings.Contains(r.stderr, "warning: ignoring the saved login (read "+h.configFile()) || !strings.Contains(r.stdout, "Logged in to") {
			t.Errorf("%s: stdout %q, stderr %q", bad, r.stdout, r.stderr)
		}
		if cfg := readFile[config](t, h.configFile()); cfg.Server != f.URL || cfg.Token == "" {
			t.Errorf("%s: config %+v", bad, cfg)
		}
	}
}

// An answer that is not about the device login, such as a proxy's 403
// page, drops the pending login instead of failing every later login.
func TestLoginResumeDropsPendingOnOtherErrors(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = func(code string, _ int) string {
		if code == "old-secret" {
			return "403"
		}
		return "pending"
	}
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	oldPending(t, h, f.URL)
	r := h.run("", "login", "--no-wait").ok(t)
	if !strings.Contains(r.stderr, "could not check the pending login (HTTP 403: <html><body>Forbidden by proxy</body></html>); starting a new one") || !strings.Contains(r.stdout, "    BCDF-GHJK\n") {
		t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if p := readFile[pendingLogin](t, h.pendingFile()); p.DeviceCode != "dev-secret-0" {
		t.Errorf("pending = %+v", p)
	}
}

func TestLoginRejectsEmptyToken(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = pollScript("notoken")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	r := h.run("", "login")
	if r.code != ExitError || !strings.Contains(r.stderr, f.URL+" approved the login but sent no token") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	gone(t, h.configFile())
	gone(t, h.pendingFile())
}

func TestLoginCapsInterval(t *testing.T) {
	f := newFake(t, "")
	f.interval = 3600
	f.devicePoll = pollScript("pending", "approved")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	h.run("", "login").ok(t)
	if !slices.Equal(h.slept, []time.Duration{time.Minute, time.Minute}) {
		t.Errorf("slept %v", h.slept)
	}
}

// Two "charlie login" in one config directory share the pending login, and
// the server hands out its token once.
func TestLoginConcurrent(t *testing.T) {
	t.Run("the other one gets the token while this one waits", func(t *testing.T) {
		f := newFake(t, "")
		f.devicePoll = pollScript("approved")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		var other result
		h.onSleep = func(n int) {
			if n == 1 {
				other = h.run("", "login", "--no-wait") // a bot's run, after the approval
			}
		}
		r := h.run("", "login").ok(t)
		if !strings.Contains(other.stdout, "Logged in to") {
			t.Errorf("other login: exit %d, %q %q", other.code, other.stdout, other.stderr)
		}
		if !strings.Contains(r.stderr, `Another "charlie login" finished this login.`) ||
			!strings.Contains(r.stdout, "Logged in to "+f.URL+" as alice (operator) until "+stamp(fakeTokenExpiry)+".") {
			t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
		}
		if got := f.last(t, "GET", "/v1/me").header.Get("Authorization"); got != "Bearer "+fakeCLIToken("dev-secret-0") {
			t.Errorf("checked with %q", got)
		}
		if f.startsMade() != 1 {
			t.Errorf("%d device logins started", f.startsMade())
		}
		gone(t, h.pendingFile())
	})
	t.Run("resuming after the other one got the token", func(t *testing.T) {
		f := newFake(t, "")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		h.run("", "login", "--no-wait").ok(t)
		exp := fakeTokenExpiry
		// This login read the pending file; meanwhile the other one exchanged
		// the code, saved the token and removed the file.
		f.devicePoll = func(string, int) string {
			if err := saveConfig(h.configFile(), config{Server: f.URL, Token: "other-token", ExpiresAt: &exp}, true); err != nil {
				t.Error(err)
			}
			_ = os.Remove(h.pendingFile())
			return "expired"
		}
		r := h.run("", "login").ok(t)
		if !strings.Contains(r.stderr, `Another "charlie login" finished this login.`) || !strings.HasPrefix(r.stdout, "Logged in to "+f.URL) {
			t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
		}
		if f.startsMade() != 1 {
			t.Errorf("a new device login was started (%d)", f.startsMade())
		}
		if cfg := readFile[config](t, h.configFile()); cfg.Token != "other-token" {
			t.Errorf("config = %+v", cfg)
		}
	})
	t.Run("a newer pending login is left alone", func(t *testing.T) {
		f := newFake(t, "")
		f.devicePoll = pollScript("denied")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		h.onSleep = func(n int) {
			if n == 1 {
				p := &pendingLogin{Server: f.URL, DeviceCode: "newer-secret", UserCode: "NEWR-CODE",
					VerificationURI: "https://console.hopcharlie.dev/#device", ExpiresAt: t0.Add(15 * time.Minute), Interval: 5}
				if err := savePending(h.pendingFile(), p, true); err != nil {
					t.Error(err)
				}
			}
		}
		if r := h.run("", "login"); r.code != ExitError || !strings.Contains(r.stderr, "denied") {
			t.Errorf("exit %d, stderr %q", r.code, r.stderr)
		}
		if p := readFile[pendingLogin](t, h.pendingFile()); p.DeviceCode != "newer-secret" {
			t.Errorf("pending = %+v", p)
		}
	})
}

// charlie tightens its own config directory to 0700 but leaves a
// CHARLIE_CONFIG_DIR that already exists alone: it may be a shared volume
// owned by someone else.
func TestConfigDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	mode := func(path string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	f := newFake(t, "")
	shared := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil { // whatever the umask
		t.Fatal(err)
	}
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_CONFIG_DIR": shared})
	f.devicePoll = pollScript("approved")
	h.run("", "login").ok(t)
	if m := mode(shared); m != 0o755 {
		t.Errorf("CHARLIE_CONFIG_DIR mode %v, want it left at 0755", m)
	}
	if m := mode(filepath.Join(shared, configFile)); m != 0o600 {
		t.Errorf("config.json mode %v", m)
	}

	// A CHARLIE_CONFIG_DIR that charlie creates is 0700.
	created := filepath.Join(t.TempDir(), "new", "charlie")
	h = newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_CONFIG_DIR": created})
	h.run("", "login", "--no-wait").ok(t)
	if m := mode(created); m != 0o700 {
		t.Errorf("created CHARLIE_CONFIG_DIR mode %v", m)
	}

	// The default directory is charlie's own: an existing one is tightened.
	h = newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	own := filepath.Join(h.dir, "charlie")
	if err := os.Mkdir(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0o755); err != nil {
		t.Fatal(err)
	}
	h.run("", "login", "--no-wait").ok(t)
	if m := mode(own); m != 0o700 {
		t.Errorf("default dir mode %v, want 0700", m)
	}
}

// oldPending is a pending login whose code expired by the local clock.
func oldPending(t *testing.T, h *harness, server string) {
	t.Helper()
	p := &pendingLogin{Server: server, DeviceCode: "old-secret", UserCode: "OLDC-ODEX",
		VerificationURI: "https://console.hopcharlie.dev/#device", ExpiresAt: t0.Add(-5 * time.Minute), Interval: 5}
	if err := savePending(h.pendingFile(), p, true); err != nil {
		t.Fatal(err)
	}
}

func TestLoginResumeAsksTheServerFirst(t *testing.T) {
	t.Run("approved after the local expiry", func(t *testing.T) {
		// Approving extends the exchange window, so the server still has it.
		f := newFake(t, "")
		f.devicePoll = pollScript("approved")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		oldPending(t, h, f.URL)
		r := h.run("", "login").ok(t)
		noSecrets(t, r)
		if got := f.pollsMade(); !slices.Equal(got, []string{"old-secret"}) || f.startsMade() != 0 {
			t.Errorf("polls %v, starts %d", got, f.startsMade())
		}
		if cfg := readFile[config](t, h.configFile()); cfg.Token != fakeCLIToken("old-secret") {
			t.Errorf("config = %+v", cfg)
		}
		gone(t, h.pendingFile())
	})
	for _, noWait := range []bool{false, true} {
		name := "expired starts a new one"
		args := []string{"login"}
		if noWait {
			name, args = name+" with --no-wait", append(args, "--no-wait")
		}
		t.Run(name, func(t *testing.T) {
			f := newFake(t, "")
			f.devicePoll = func(code string, n int) string {
				if code == "old-secret" {
					return "expired"
				}
				return "approved"
			}
			h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
			oldPending(t, h, f.URL)
			r := h.run("", args...).ok(t)
			noSecrets(t, r)
			if f.startsMade() != 1 || !strings.Contains(r.stdout, "    BCDF-GHJK\n") || strings.Contains(r.stdout, "OLDC-ODEX") {
				t.Errorf("starts %d, stdout:\n%s", f.startsMade(), r.stdout)
			}
			if noWait {
				if p := readFile[pendingLogin](t, h.pendingFile()); p.DeviceCode != "dev-secret-0" {
					t.Errorf("pending = %+v", p)
				}
				return
			}
			if cfg := readFile[config](t, h.configFile()); cfg.Token != fakeCLIToken("dev-secret-0") {
				t.Errorf("config = %+v", cfg)
			}
		})
	}
	t.Run("denied", func(t *testing.T) {
		f := newFake(t, "")
		f.devicePoll = pollScript("denied")
		h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
		oldPending(t, h, f.URL)
		r := h.run("", "login", "--no-wait")
		if r.code != ExitError || !strings.Contains(r.stderr, "error: an administrator denied this login in the console") || f.startsMade() != 0 {
			t.Errorf("exit %d, starts %d, stderr %q", r.code, f.startsMade(), r.stderr)
		}
		gone(t, h.pendingFile())
	})
}

func TestLoginDenied(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = pollScript("pending", "denied")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	r := h.run("", "login")
	noSecrets(t, r)
	if r.code != ExitError || !strings.HasSuffix(r.stderr, "error: an administrator denied this login in the console\n") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	gone(t, h.pendingFile())
	gone(t, h.configFile())
}

func TestLoginCodeExpires(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = pollScript("pending", "expired")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	r := h.run("", "login")
	if r.code != ExitError || !strings.Contains(r.stderr, `error: the code BCDF-GHJK expired; "charlie login --server `+f.URL+`" gets a new one`) {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	gone(t, h.pendingFile())
	if r := h.run("", "login", "--no-wait").ok(t); !strings.Contains(r.stdout, "    LMNP-QRST\n") || f.startsMade() != 2 {
		t.Errorf("starts %d, next login:\n%s", f.startsMade(), r.stdout)
	}

	// A server that never answers for good: charlie stops once even an
	// approval at the last moment would have run out.
	f2 := newFake(t, "")
	h2 := newHarness(t, map[string]string{"CHARLIE_SERVER": f2.URL})
	r = h2.run("", "login")
	if r.code != ExitError || !strings.Contains(r.stderr, "expired") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if !h2.now.After(t0.Add(20*time.Minute)) || h2.now.After(t0.Add(20*time.Minute+5*time.Second)) {
		t.Errorf("gave up at %v", h2.now.Sub(t0))
	}
	gone(t, h2.pendingFile())
}

func TestLoginRetriesNetworkErrors(t *testing.T) {
	f := newFake(t, "")
	f.devicePoll = pollScript("drop", "500", "drop", "pending", "approved")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	r := h.run("", "login").ok(t)
	if n := strings.Count(r.stderr, "cannot reach "+f.URL); n != 1 || !strings.Contains(r.stderr, "retrying every 5s") {
		t.Errorf("stderr %q", r.stderr)
	}
	if len(f.pollsMade()) != 5 || !strings.Contains(r.stdout, "Logged in to") {
		t.Errorf("polls %v, stdout %q", f.pollsMade(), r.stdout)
	}
}

func TestLoginCtrlCKeepsPending(t *testing.T) {
	f := newFake(t, "")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.onSleep = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	r := h.runCtx(ctx, "", "login")
	if r.code != ExitInterrupted || !strings.Contains(r.stderr, `"charlie login --server `+f.URL+`" resumes it with the same code`) {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if p := readFile[pendingLogin](t, h.pendingFile()); p.DeviceCode != "dev-secret-0" {
		t.Errorf("pending = %+v", p)
	}
	gone(t, h.configFile())

	h.onSleep = nil
	f.devicePoll = pollScript("approved")
	h.run("", "login").ok(t)
	if got := f.pollsMade(); got[len(got)-1] != "dev-secret-0" || f.startsMade() != 1 {
		t.Errorf("polls %v, starts %d", got, f.startsMade())
	}
}

func TestLoginReplacesPendingForAnotherServer(t *testing.T) {
	f := newFake(t, "")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	oldPending(t, h, "https://other.example")
	r := h.run("", "login", "--no-wait").ok(t)
	if strings.Contains(r.stdout, "OLDC-ODEX") || !strings.Contains(r.stdout, "BCDF-GHJK") || len(f.pollsMade()) != 0 {
		t.Errorf("polls %v, stdout:\n%s", f.pollsMade(), r.stdout)
	}
	if p := readFile[pendingLogin](t, h.pendingFile()); p.Server != f.URL || p.DeviceCode != "dev-secret-0" {
		t.Errorf("pending = %+v", p)
	}
}

// redirect sends every request to target and records the URL it was for.
type redirect struct {
	target *url.URL
	seen   *[]string
}

func (rt redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	*rt.seen = append(*rt.seen, req.URL.String())
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestLoginDefaultServer(t *testing.T) {
	f := newFake(t, "")
	target, _ := url.Parse(f.URL)
	var seen []string
	h := newHarness(t, nil)
	h.http = &http.Client{Transport: redirect{target: target, seen: &seen}}

	r := h.run("", "login", "--no-wait").ok(t)
	if !slices.Equal(seen, []string{"https://api.hopcharlie.dev/v1/auth/device"}) || !strings.Contains(r.stdout, `run "charlie login" to finish`) {
		t.Errorf("requests %v, stdout:\n%s", seen, r.stdout)
	}
	if p := readFile[pendingLogin](t, h.pendingFile()); p.Server != defaultServer {
		t.Errorf("pending = %+v", p)
	}
	f.devicePoll = pollScript("approved")
	h.run("", "login").ok(t)
	if r := h.run("", "whoami").ok(t); !strings.HasPrefix(r.stdout, "alice (operator) on https://api.hopcharlie.dev\n") {
		t.Errorf("whoami = %q", r.stdout)
	}
	if seen[len(seen)-1] != "https://api.hopcharlie.dev/v1/me" {
		t.Errorf("requests %v", seen)
	}
}

func TestLoginJSON(t *testing.T) {
	f := newFake(t, "")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL})
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		slices.Sort(k)
		return k
	}

	r := h.run("", "--json", "login", "--no-wait").ok(t)
	noSecrets(t, r)
	var code map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &code); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if got := keys(code); !slices.Equal(got, []string{"expiresAt", "server", "userCode", "verificationUri"}) ||
		code["userCode"] != "BCDF-GHJK" || code["server"] != f.URL || code["expiresAt"] != t0.Add(10*time.Minute).Format(time.RFC3339) ||
		code["verificationUri"] != "https://console.hopcharlie.dev/#device" {
		t.Errorf("--no-wait --json = %v", code)
	}
	if r.stderr != "" {
		t.Errorf("stderr %q", r.stderr)
	}

	f.devicePoll = pollScript("pending", "approved")
	r = h.run("", "--json", "login").ok(t)
	noSecrets(t, r)
	var res map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if got := keys(res); !slices.Equal(got, []string{"expiresAt", "name", "role"}) || res["name"] != "alice" || res["role"] != "agent" ||
		res["expiresAt"] != fakeTokenExpiry.Format(time.RFC3339) {
		t.Errorf("--json = %v", res)
	}
	if !strings.Contains(r.stderr, "    BCDF-GHJK\n") || !strings.Contains(r.stderr, "Waiting for approval") {
		t.Errorf("stderr %q", r.stderr)
	}
}

func TestLoginFailures(t *testing.T) {
	down := newFake(t, "")
	down.Close()
	h := newHarness(t, nil)
	r := h.run("", "login", "--server", down.URL)
	if r.code != ExitError || !strings.Contains(r.stderr, "start a device login with "+down.URL) {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	gone(t, h.pendingFile())
	if r := h.run("", "login", "extra"); r.code != ExitUsage {
		t.Errorf("argument: exit %d, %q", r.code, r.stderr)
	}
	if r := h.run("", "login", "--server", "ftp://x"); r.code != ExitUsage {
		t.Errorf("bad --server: exit %d, %q", r.code, r.stderr)
	}
}

func TestLogout(t *testing.T) {
	f := newFake(t, "cli-tok")
	h := newHarness(t, nil)
	exp := t0.Add(24 * time.Hour)
	login := func(server, token string) {
		t.Helper()
		if err := saveConfig(h.configFile(), config{Server: server, Token: token, ExpiresAt: &exp}, true); err != nil {
			t.Fatal(err)
		}
	}

	login(f.URL, "cli-tok")
	oldPending(t, h, f.URL)
	r := h.run("", "logout").ok(t)
	if got := f.last(t, "POST", "/v1/auth/logout").header.Get("Authorization"); got != "Bearer cli-tok" {
		t.Errorf("logout sent %q", got)
	}
	if want := "Logged out of " + f.URL + ". The server ended the token.\nRemoved " + h.configFile() + "\nRemoved " + h.pendingFile() + "\n"; r.stdout != want {
		t.Errorf("stdout %q, want %q", r.stdout, want)
	}
	gone(t, h.configFile())
	gone(t, h.pendingFile())
	n := len(f.requests())
	if r := h.run("", "logout").ok(t); r.stdout != "Not logged in.\n" || len(f.requests()) != n {
		t.Errorf("second logout = %q", r.stdout)
	}
	if r := h.run("", "whoami"); r.code != ExitError || !strings.Contains(r.stderr, `no server configured; run "charlie login"`) {
		t.Errorf("whoami after logout: exit %d, %q", r.code, r.stderr)
	}

	login(f.URL, "revoked")
	if r := h.run("", "logout").ok(t); !strings.Contains(r.stdout, "The token had already expired or been revoked.") {
		t.Errorf("revoked: %q", r.stdout)
	}
	gone(t, h.configFile())

	down := newFake(t, "")
	down.Close()
	login(down.URL, "cli-tok")
	r = h.run("", "logout").ok(t)
	if !strings.Contains(r.stdout, "Could not reach the server") || !strings.Contains(r.stdout, "the token stays valid until "+stamp(exp)+".") {
		t.Errorf("server down: %q", r.stdout)
	}
	gone(t, h.configFile())

	// A static token saved by the stdin login of older versions: no expiry,
	// and the server cannot end it.
	if err := saveConfig(h.configFile(), config{Server: f.URL, Token: "cli-tok"}, true); err != nil {
		t.Fatal(err)
	}
	n = len(f.requests())
	r = h.run("", "logout").ok(t)
	if !strings.Contains(r.stdout, "on this machine only") || !strings.Contains(r.stdout, "stays valid on the server") || len(f.requests()) != n {
		t.Errorf("static token: %q, %d requests", r.stdout, len(f.requests())-n)
	}
	gone(t, h.configFile())

	// Only a pending login.
	oldPending(t, h, f.URL)
	if r := h.run("", "logout").ok(t); r.stdout != "Not logged in.\nRemoved "+h.pendingFile()+"\n" {
		t.Errorf("pending only: %q", r.stdout)
	}
}

func TestConfigDirEnv(t *testing.T) {
	f := newFake(t, "")
	dir := filepath.Join(t.TempDir(), "state")
	h := newHarness(t, map[string]string{"CHARLIE_SERVER": f.URL, "CHARLIE_CONFIG_DIR": dir})

	h.run("", "login", "--no-wait").ok(t)
	readFile[pendingLogin](t, filepath.Join(dir, pendingFile))
	f.devicePoll = pollScript("approved")
	if r := h.run("", "login").ok(t); !strings.Contains(r.stdout, "Saved to "+filepath.Join(dir, configFile)+"\n") {
		t.Errorf("login stdout %q", r.stdout)
	}
	gone(t, filepath.Join(dir, pendingFile))
	if cfg := readFile[config](t, filepath.Join(dir, configFile)); cfg.Token != fakeCLIToken("dev-secret-0") {
		t.Errorf("config = %+v", cfg)
	}
	if r := h.run("", "whoami").ok(t); !strings.Contains(r.stdout, "The saved login expires at") {
		t.Errorf("whoami = %q", r.stdout)
	}
	gone(t, filepath.Join(h.dir, "charlie"))

	if r := h.run("", "logout").ok(t); !strings.Contains(r.stdout, "Removed "+filepath.Join(dir, configFile)) {
		t.Errorf("logout = %q", r.stdout)
	}
	gone(t, filepath.Join(dir, configFile))
	if got := f.last(t, "POST", "/v1/auth/logout").header.Get("Authorization"); got != "Bearer "+fakeCLIToken("dev-secret-0") {
		t.Errorf("logout sent %q", got)
	}
}

func TestSavedLoginRejectedHint(t *testing.T) {
	const hint = `The saved login expired or was revoked; run "charlie login".`
	f := newFake(t, "fresh")
	h := newHarness(t, nil)
	if err := saveConfig(h.configFile(), config{Server: f.URL, Token: "stale"}, true); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "env", "list")
	if r.code != ExitError || !strings.HasSuffix(r.stderr, "(HTTP 401)\n"+hint+"\n") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if err := saveConfig(h.configFile(), config{Server: f.URL, Token: "fresh"}, true); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "env", "show", "nope"); r.code != ExitError || strings.Contains(r.stderr, hint) {
		t.Errorf("404: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestDeviceClient(t *testing.T) {
	for _, tc := range []struct{ version, host, want string }{
		{"v0.5.0", "rin-sbx-12", "charlie v0.5.0 on rin-sbx-12"},
		{"1.2.3", "", "charlie 1.2.3 on unknown"},
		{"1.2.3", "\x00\a\n", "charlie 1.2.3 on unknown"},
		{"1.2.3", "evil\x1b[31m‮host\n", "charlie 1.2.3 on evil[31mhost"},
	} {
		if got := deviceClient(tc.version, tc.host); got != tc.want {
			t.Errorf("deviceClient(%q, %q) = %q, want %q", tc.version, tc.host, got, tc.want)
		}
	}
	long := deviceClient("1.2.3", strings.Repeat("h", 300))
	if utf8.RuneCountInString(long) != 200 || !strings.HasPrefix(long, "charlie 1.2.3 on hhh") {
		t.Errorf("long host: %d runes, %q", utf8.RuneCountInString(long), long)
	}
}
