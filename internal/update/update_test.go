package update

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	current = "v0.0.0-20261102022616-c4fbbe8ccdac"
	newer   = "v0.0.0-20261103090000-0123456789ab"
	older   = "v0.0.0-20261101000000-aaaaaaaaaaaa"
)

// fakeGo plays the go command: "go list" answers latest, "go install"
// writes a binary into GOBIN.
type fakeGo struct {
	mu         sync.Mutex
	latest     string
	listErr    error
	installErr error
	calls      []string
}

func (f *fakeGo) run(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if !strings.Contains(strings.Join(env, "\n"), "GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie") {
		return []byte("GOPRIVATE not set"), errors.New("exit status 1")
	}
	switch args[0] {
	case "list":
		if f.listErr != nil {
			return []byte("go: module lookup disabled"), f.listErr
		}
		return []byte(`{"Path":"` + Module + `","Version":"` + f.latest + `"}`), nil
	case "install":
		if f.installErr != nil {
			return []byte("build failed"), f.installErr
		}
		var gobin string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GOBIN="); ok {
				gobin = v
			}
		}
		name := "charlie"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return nil, os.WriteFile(filepath.Join(gobin, name), []byte("new binary "+strings.TrimPrefix(args[1], Package+"@")), 0o755)
	}
	return nil, errors.New("unexpected go " + args[0])
}

type harness struct {
	cfg    Config
	exe    string
	env    map[string]string
	now    time.Time
	go_    *fakeGo
	starts []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{env: map[string]string{}, now: time.Now(), go_: &fakeGo{latest: newer}}
	bin := t.TempDir()
	h.exe = filepath.Join(bin, "charlie")
	if runtime.GOOS == "windows" {
		h.exe += ".exe"
	}
	if err := os.WriteFile(h.exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg = Config{
		Dir:        filepath.Join(t.TempDir(), "charlie"),
		Version:    current,
		Getenv:     func(k string) string { return h.env[k] },
		Now:        func() time.Time { return h.now },
		Executable: func() (string, error) { return h.exe, nil },
		LookPath:   func(string) (string, error) { return "/usr/bin/go", nil },
		Run:        h.go_.run,
		Start: func(exe string, args []string, logPath string) error {
			h.starts = append(h.starts, exe+" "+strings.Join(args, " "))
			return nil
		},
	}
	return h
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMaybeStartAtMostOncePerInterval(t *testing.T) {
	h := newHarness(t)
	MaybeStart(h.cfg)
	MaybeStart(h.cfg)
	if len(h.starts) != 1 || !strings.HasSuffix(h.starts[0], "update --background") {
		t.Fatalf("starts = %v", h.starts)
	}
	h.now = h.now.Add(DefaultInterval)
	MaybeStart(h.cfg)
	if len(h.starts) != 2 {
		t.Fatalf("after the interval: starts = %v", h.starts)
	}
	h.env["CHARLIE_UPDATE_INTERVAL"] = "0"
	MaybeStart(h.cfg)
	if len(h.starts) != 3 {
		t.Fatalf("interval 0 checks every time: starts = %v", h.starts)
	}
}

func TestMaybeStartSkips(t *testing.T) {
	cases := map[string]func(h *harness){
		"opted out":         func(h *harness) { h.env["CHARLIE_NO_UPDATE"] = "1" },
		"development build": func(h *harness) { h.cfg.Version = "(devel)" },
		"no version":        func(h *harness) { h.cfg.Version = "" },
		"no go command": func(h *harness) {
			h.cfg.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			MaybeStart(h.cfg)
			if len(h.starts) != 0 {
				t.Fatalf("started: %v", h.starts)
			}
		})
	}
}

func TestUpdateSwapsBinaryAndLeavesNotice(t *testing.T) {
	h := newHarness(t)
	res, err := Update(context.Background(), h.cfg, io.Discard)
	if err != nil || !res.Updated || res.From != current || res.To != newer {
		t.Fatalf("update = %+v, %v", res, err)
	}
	if got := read(t, h.exe); got != "new binary "+newer {
		t.Fatalf("executable = %q", got)
	}
	old := h.exe + ".old"
	if runtime.GOOS == "windows" {
		if read(t, old) != "old binary" {
			t.Fatal("the previous binary must stay until it can be removed")
		}
		CleanOld(h.exe)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old binary still there: %v", err)
	}
	if msg := TakeNotice(h.cfg); msg != "charlie was updated from "+current+" to "+newer {
		t.Fatalf("notice = %q", msg)
	}
	if msg := TakeNotice(h.cfg); msg != "" {
		t.Fatalf("notice shown twice: %q", msg)
	}
	if _, err := os.Stat(h.cfg.lockPath()); !os.IsNotExist(err) {
		t.Fatal("lock not released")
	}
}

func TestUpdateDoesNothingWhenNotNewer(t *testing.T) {
	for _, latest := range []string{current, older} {
		h := newHarness(t)
		h.go_.latest = latest
		res, err := Update(context.Background(), h.cfg, io.Discard)
		if err != nil || res.Updated {
			t.Fatalf("latest %s: %+v, %v", latest, res, err)
		}
		if len(h.go_.calls) != 1 || read(t, h.exe) != "old binary" {
			t.Fatalf("latest %s: calls %v", latest, h.go_.calls)
		}
	}
}

func TestFailedInstallKeepsTheWorkingBinary(t *testing.T) {
	h := newHarness(t)
	h.go_.installErr = errors.New("exit status 1")
	if _, err := Update(context.Background(), h.cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("err = %v", err)
	}
	if read(t, h.exe) != "old binary" {
		t.Fatal("binary changed after a failed install")
	}
	if s := h.cfg.load(); !strings.Contains(s.LastError, "go install") || s.UpdatedTo != "" {
		t.Fatalf("state = %+v", s)
	}
}

func TestUpdateRefusesDevelopmentBuilds(t *testing.T) {
	h := newHarness(t)
	h.cfg.Version = "(devel)"
	h.env["CHARLIE_NO_UPDATE"] = "1"
	if _, err := Update(context.Background(), h.cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("err = %v", err)
	}
	if len(h.go_.calls) != 0 {
		t.Fatalf("calls = %v", h.go_.calls)
	}
}

func TestLock(t *testing.T) {
	h := newHarness(t)
	unlock, err := h.cfg.lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), h.cfg, io.Discard); !errors.Is(err, ErrBusy) {
		t.Fatalf("second updater: %v", err)
	}
	unlock()

	// A lock left by a killed updater stops counting.
	if err := os.WriteFile(h.cfg.lockPath(), []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := h.now.Add(-lockStale - time.Minute)
	if err := os.Chtimes(h.cfg.lockPath(), stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), h.cfg, io.Discard); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
}

func TestGoEnvAddsPrivatePattern(t *testing.T) {
	env := goEnv([]string{"PATH=/bin", "GOPRIVATE=example.com/x"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GOPRIVATE=example.com/x,github.com/2026-Softbank-Hackathon-Charlie") || strings.Count(joined, "GOPRIVATE=") != 1 {
		t.Fatalf("env = %v", env)
	}
	env = goEnv([]string{"GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie"})
	if strings.Count(strings.Join(env, "\n"), "2026-Softbank-Hackathon-Charlie") != 1 {
		t.Fatalf("env = %v", env)
	}
}
