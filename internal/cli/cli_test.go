package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestSettingsPrecedence(t *testing.T) {
	a, b, c := newFake(t, ""), newFake(t, ""), newFake(t, "")
	otherServer := func(f *fakeServer) string {
		return `the saved login is for ` + a.URL + `, not ` + f.URL + `; run "charlie login --server ` + f.URL + `" or set CHARLIE_TOKEN`
	}
	cases := []struct {
		name      string
		saved     string // server of the saved login; a.URL when empty
		env       map[string]string
		args      []string
		want      *fakeServer // nil: no request at all
		wantToken string
		wantErr   string
	}{
		{"saved login", "", nil, nil, a, "file-token", ""},
		{"CHARLIE_TOKEN over file", "", map[string]string{"CHARLIE_TOKEN": "env-token"}, nil, a, "env-token", ""},
		{"CHARLIE_SERVER and CHARLIE_TOKEN over file", "", map[string]string{"CHARLIE_SERVER": b.URL, "CHARLIE_TOKEN": "env-token"}, nil, b, "env-token", ""},
		{"--server over env and file", "", map[string]string{"CHARLIE_SERVER": b.URL, "CHARLIE_TOKEN": "env-token"}, []string{"--server", c.URL}, c, "env-token", ""},
		{"--server naming the saved server", "", nil, []string{"--server", a.URL + "/"}, a, "file-token", ""},
		{"CHARLIE_SERVER naming the saved server", a.URL + "/", map[string]string{"CHARLIE_SERVER": a.URL}, nil, a, "file-token", ""},
		// The saved token never goes to another server.
		{"CHARLIE_SERVER elsewhere", "", map[string]string{"CHARLIE_SERVER": b.URL}, nil, nil, "", otherServer(b)},
		{"--server elsewhere", "", nil, []string{"--server", c.URL}, nil, "", otherServer(c)},
		{"--server elsewhere over CHARLIE_SERVER", "", map[string]string{"CHARLIE_SERVER": a.URL}, []string{"--server", c.URL}, nil, "", otherServer(c)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.env)
			saved := cmp.Or(tc.saved, a.URL)
			if err := saveConfig(filepath.Join(h.dir, "charlie", "config.json"), config{Server: saved, Token: "file-token"}); err != nil {
				t.Fatal(err)
			}
			before := map[*fakeServer]int{a: len(a.requests()), b: len(b.requests()), c: len(c.requests())}
			r := h.run("", append(tc.args, "whoami")...)
			if tc.wantErr != "" {
				if r.code != ExitError || !strings.Contains(r.stderr, tc.wantErr) {
					t.Errorf("exit %d, stderr %q, want %q", r.code, r.stderr, tc.wantErr)
				}
			} else {
				r.ok(t)
			}
			for _, f := range []*fakeServer{a, b, c} {
				got := len(f.requests()) - before[f]
				if (f == tc.want) != (got == 1) {
					t.Errorf("server %s got %d requests", f.URL, got)
				}
			}
			if tc.want == nil {
				return
			}
			if got := tc.want.last(t, "GET", "/v1/me").header.Get("Authorization"); got != "Bearer "+tc.wantToken {
				t.Errorf("Authorization = %q, want token %q", got, tc.wantToken)
			}
		})
	}

	// Logging in to the other server still works, and replaces the saved login.
	h := newHarness(t, nil)
	if err := saveConfig(filepath.Join(h.dir, "charlie", "config.json"), config{Server: a.URL, Token: "file-token"}); err != nil {
		t.Fatal(err)
	}
	c.devicePoll = pollScript("approved")
	h.run("", "login", "--server", c.URL).ok(t)
	h.run("", "whoami").ok(t)
	if got := c.last(t, "GET", "/v1/me").header.Get("Authorization"); got != "Bearer "+fakeCLIToken(fakeDeviceCode(0)) {
		t.Errorf("after login to c, Authorization = %q", got)
	}
}

// aligned checks that, in the line containing row, value starts in the
// same column as head does in the header line.
func aligned(t *testing.T, out, head, row, value string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	col := -1
	for _, l := range lines {
		if i := strings.Index(l, head); i >= 0 && strings.ToUpper(l) == l {
			col = i
			break
		}
	}
	for _, l := range lines {
		if strings.Contains(l, row) {
			if col < 0 || len(l) < col || !strings.HasPrefix(l[col:], value) {
				t.Errorf("column %s: want %q in the %q row at %d:\n%s", head, value, row, col, out)
			}
			return
		}
	}
	t.Errorf("no %q row in:\n%s", row, out)
}

func TestTables(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)

	out := h.run("", "env", "list").ok(t).stdout
	for _, col := range [][3]string{{"PROVIDER", "gcp", "onprem"}, {"STATUS", "[ok] reachable", "[x] unreachable"}, {"NODES", "3/3", "-"}, {"DATABASES", "pg 2/2", "-"}} {
		aligned(t, out, col[0], "GCP GKE", col[1])
		aligned(t, out, col[0], "On-prem", col[2])
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape codes in non-terminal output")
	}

	out = h.run("", "env", "show", "gke").ok(t).stdout
	for _, want := range []string{"Display name:", "GCP GKE", "yes (the gateway sends writes here)", "3/3 ready (amd64)", "DATABASE", "pg-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("env show lacks %q:\n%s", want, out)
		}
	}

	out = h.run("", "plan", "list").ok(t).stdout
	aligned(t, out, "ROUTE", "pln_1", "gke -> eks")
	aligned(t, out, "RUN", "pln_1", "run_1")
	if !strings.Contains(out, "[ok] started") || !strings.Contains(out, "[ ] pending_approval") {
		t.Errorf("plan list:\n%s", out)
	}

	out = h.run("", "migrate", "list").ok(t).stdout
	aligned(t, out, "DOWNTIME", "[ok] succeeded", "2.1s")
	aligned(t, out, "DOWNTIME", "[..] running", "-")

	out = h.run("", "audit", "--limit", "2", "--before", "50").ok(t).stdout
	if q := f.last(t, "GET", "/v1/audit").query; q != "before=50&limit=2" {
		t.Errorf("audit query = %q", q)
	}
	aligned(t, out, "ACTION", "planner", "POST /v1/plans/{ref}/approve")
	aligned(t, out, "STATUS", "planner", "403")
	if !strings.Contains(out, "Older entries: charlie audit --before 41") {
		t.Errorf("audit:\n%s", out)
	}

	out = h.run("", "matrix").ok(t).stdout
	if !strings.Contains(out, "[ ] never run") || !strings.Contains(out, "[ok] succeeded") || !strings.Contains(out, "Environments: gke, eks") {
		t.Errorf("matrix:\n%s", out)
	}

	out = h.run("", "traffic").ok(t).stdout
	for _, want := range []string{"Write backend:", "3 reads, 1 writes", "[ok] up", "[x] down", "90%", "3 of 1200 (0.25%)", "eks: connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic lacks %q:\n%s", want, out)
		}
	}
}

func TestJSONOutput(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
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
	envs := fixtureEnvs()
	check([]string{"env", "list"}, &apiv1.EnvironmentList{}, normalize(t, envs))
	check([]string{"plan", "show", "pln_1"}, &apiv1.PlanRecord{}, normalize(t, fixturePlan("pln_1", apiv1.PlanStatusStarted)))
	check([]string{"traffic"}, &apiv1.TrafficState{}, normalize(t, fixtureTraffic()))
	check([]string{"version"}, &map[string]string{}, map[string]string{"version": "1.2.3"})
}

// normalize round-trips v through JSON so times compare equal after decoding.
func normalize[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAPIErrorsAreShown(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	r := h.run("", "env", "show", "nope")
	if r.code != ExitError || !strings.Contains(r.stderr, "error: env_not_found: environment not found (HTTP 404)") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	r = h.run("", "plan", "approve", "pln_1")
	if r.code != ExitError || !strings.Contains(r.stderr, "plan_state_conflict") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	h.env["CHARLIE_TOKEN"] = "wrong"
	if r = h.run("", "plan", "list"); r.code != ExitError || !strings.Contains(r.stderr, "unauthorized: missing or invalid bearer token") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "saved login") {
		t.Errorf("CHARLIE_TOKEN got the saved login hint: %q", r.stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	for _, args := range [][]string{
		{"bogus"},
		{"plan", "bogus"},
		{"plan", "show"},
		{"plan", "show", "a", "b"},
		{"--bogus"},
		{"env", "list", "--nope"},
		{"plan", "reject", "pln_1"},
		{"plan", "submit"},
		{"migrate", "watch"},
		{"migrate", "start", "pln_1", "--canary", "10,abc"},
		{"migrate", "start", "pln_1", "--canary", "0"},
		{"migrate", "start", "pln_1", "--canary", "10,101"},
		{"migrate", "start", "pln_1", "--canary-step", "5s"},
		{"migrate", "start", "pln_1", "--max-p95", "fast"},
		{"audit", "--limit", "0"},
		{"traffic", "--interval", "0s"},
		{"--server", "ftp://example.com", "whoami"},
		{"version", "extra"},
	} {
		r := h.run("", args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "--help' for usage") {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("usage errors sent %d requests", n)
	}
	// Help and a bare group are not errors.
	h.run("", "--help").ok(t)
	if r := h.run("", "migrate").ok(t); !strings.Contains(r.stdout, "Available Commands") {
		t.Errorf("bare group = %q", r.stdout)
	}
}

func TestPlanCommands(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	plan := `{"id":"shop-gke-eks","source":{"env":"gke"}}`
	file := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(file, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "plan", "submit", "-f", file).ok(t)
	if got := string(f.last(t, "POST", "/v1/plans").body); got != plan {
		t.Errorf("submitted %q", got)
	}
	if !strings.Contains(r.stdout, "Submitted plan pln_2.") || !strings.Contains(r.stdout, "charlie plan approve pln_2") {
		t.Errorf("submit = %q", r.stdout)
	}
	h.run(plan, "plan", "submit", "-f", "-").ok(t)
	if got := string(f.last(t, "POST", "/v1/plans").body); got != plan {
		t.Errorf("stdin submitted %q", got)
	}

	r = h.run("", "plan", "approve", "pln_2", "--comment", "ok to go").ok(t)
	if got := string(f.last(t, "POST", "/v1/plans/pln_2/approve").body); got != `{"comment":"ok to go","revision":2}` {
		t.Errorf("approve body %s", got)
	}
	if !strings.Contains(r.stdout, "charlie migrate start pln_2") {
		t.Errorf("approve = %q", r.stdout)
	}
	h.run("", "plan", "reject", "pln_2", "--comment", "wrong target").ok(t)
	if got := string(f.last(t, "POST", "/v1/plans/pln_2/reject").body); got != `{"comment":"wrong target"}` {
		t.Errorf("reject body %s", got)
	}

	// Whoever reviews a plan sees where the database passwords would go.
	for _, args := range [][]string{
		{"plan", "show", "pln_1"},
		{"plan", "submit", "-f", file},
		{"plan", "approve", "pln_2"},
		{"plan", "reject", "pln_2", "--comment", "no"},
	} {
		out := h.run("", args...).ok(t).stdout
		for _, want := range []string{"Source DB host:  10.0.1.5:5432", "Target DB host:  pg.eks.example.com:5432"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v lacks %q:\n%s", args, want, out)
			}
		}
	}
}

func TestMigrateStart(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)

	r := h.run("", "migrate", "start", "pln_1").ok(t)
	req := f.last(t, "POST", "/v1/migrations")
	if string(req.body) != `{"planRef":"pln_1"}` {
		t.Errorf("body %s", req.body)
	}
	key := req.header.Get("Idempotency-Key")
	if !strings.HasPrefix(key, "charlie-") || !strings.Contains(r.stdout, "Idempotency key: "+key) {
		t.Errorf("key %q, stdout %q", key, r.stdout)
	}
	if !strings.Contains(r.stdout, "charlie migrate watch run_1") {
		t.Errorf("stdout %q", r.stdout)
	}

	h.run("", "migrate", "start", "pln_1", "--auto-switch", "--canary", "10,50", "--canary-step", "30s",
		"--max-error-rate", "0.02", "--max-p95", "500", "--min-requests", "30", "--idempotency-key", "my-key").ok(t)
	req = f.last(t, "POST", "/v1/migrations")
	var got, want any
	_ = json.Unmarshal(req.body, &got)
	_ = json.Unmarshal([]byte(`{"planRef":"pln_1","autoSwitch":true,"canary":{"weights":[10,50],"stepDuration":"30s","gate":{"maxErrorRate":0.02,"maxP95Millis":500,"minRequests":30}}}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body %s", req.body)
	}
	if k := req.header.Get("Idempotency-Key"); k != "my-key" {
		t.Errorf("Idempotency-Key = %q", k)
	}

	r = h.run("", "--json", "migrate", "start", "pln_1", "--idempotency-key", "k2").ok(t)
	var resp apiv1.StartMigrationResponse
	if err := json.Unmarshal([]byte(r.stdout), &resp); err != nil || resp.RunId != "run_1" {
		t.Errorf("json stdout %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "idempotency key: k2") {
		t.Errorf("json stderr %q", r.stderr)
	}
}

func TestRunActions(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	if r := h.run("", "migrate", "switch", "run_1").ok(t); !strings.Contains(r.stdout, "Cutover approved for run run_1") {
		t.Errorf("switch = %q", r.stdout)
	}
	f.last(t, "POST", "/v1/migrations/run_1/switch")
	h.run("", "migrate", "abort", "run_1", "--reason", "lag too high").ok(t)
	if got := string(f.last(t, "POST", "/v1/migrations/run_1/abort").body); got != `{"reason":"lag too high"}` {
		t.Errorf("abort body %s", got)
	}
	h.run("", "migrate", "resume", "run_1").ok(t)
	f.last(t, "POST", "/v1/migrations/run_1/resume")

	out := h.run("", "migrate", "status", "run_1").ok(t).stdout
	for _, want := range append([]string{"Run run_1  gke -> eks", "[ok] succeeded", "[ ] pending", "Downtime:"}, engineSteps...) {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestTrafficWatchPollsUntilInterrupted(t *testing.T) {
	f := newFake(t, "tok")
	h := loggedIn(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	r := h.runCtx(ctx, "", "--json", "traffic", "--watch", "--interval", "50ms")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("want several polls, got %d:\n%s", len(lines), r.stdout)
	}
	for _, l := range lines {
		var s apiv1.TrafficState
		if err := json.Unmarshal([]byte(l), &s); err != nil || s.WriteBackend != "gke" {
			t.Errorf("line %q: %v", l, err)
		}
	}
}

func TestColorIsAppliedAfterAlignment(t *testing.T) {
	plain := table([][]string{{"A", "B"}, {"[ok] x", "1"}, {"[x] longer", "2"}})
	p := &printer{color: true}
	painted := p.paint(plain)
	if !strings.Contains(painted, "\x1b[32m[ok]\x1b[0m x") || !strings.Contains(painted, "\x1b[31m[x]\x1b[0m") {
		t.Errorf("painted = %q", painted)
	}
	strip := strings.NewReplacer("\x1b[32m", "", "\x1b[31m", "", "\x1b[0m", "")
	if strip.Replace(painted) != plain {
		t.Errorf("painting changed the layout")
	}
	if (&printer{}).paint(plain) != plain {
		t.Errorf("paint without color changed the text")
	}
}

func TestHumanFormats(t *testing.T) {
	for d, want := range map[time.Duration]string{
		850 * time.Millisecond: "850ms", 12300 * time.Millisecond: "12.3s", 245 * time.Second: "4m05s",
		2*time.Hour + 3*time.Minute: "2h03m", 50 * time.Hour: "2d02h", -time.Second: "0ms",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{512: "512 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"} {
		if got := bytesHuman(n); got != want {
			t.Errorf("bytesHuman(%d) = %q, want %q", n, got, want)
		}
	}
}
