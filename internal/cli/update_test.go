package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/cli/internal/update"
)

// runWithUpdater runs args with an updater that records background starts
// instead of launching them.
func runWithUpdater(t *testing.T, dir string, starts *[]string, args ...string) result {
	t.Helper()
	u := testUpdater(t, dir, starts)
	var out, errb bytes.Buffer
	app := &App{
		Version: "v0.0.0-20261102022616-c4fbbe8ccdac", Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, ConfigDir: func() (string, error) { return t.TempDir(), nil },
		HTTPClient: http.DefaultClient, Updater: &u,
	}
	code := app.Run(context.Background(), args)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// testUpdater is an updater in dir that records background starts.
func testUpdater(t *testing.T, dir string, starts *[]string) update.Config {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "charlie.exe")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return update.Config{
		Dir:        dir,
		Version:    "v0.0.0-20261102022616-c4fbbe8ccdac",
		Getenv:     func(string) string { return "" },
		Now:        time.Now,
		Executable: func() (string, error) { return exe, nil },
		LookPath:   func(string) (string, error) { return "go", nil },
		Run: func(context.Context, string, []string, string, ...string) ([]byte, error) {
			return []byte(`{"Version":"v0.0.0-20261102022616-c4fbbe8ccdac"}`), nil // already the latest
		},
		Start: func(exe string, args []string, _ string) error {
			*starts = append(*starts, strings.Join(args, " "))
			return nil
		},
	}
}

// The updater keeps update.json and update.log in the charlie config
// directory, which CHARLIE_CONFIG_DIR moves.
func TestUpdaterUsesConfigDir(t *testing.T) {
	base, moved := t.TempDir(), filepath.Join(t.TempDir(), "state")
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{nil, filepath.Join(base, "charlie")},
		{map[string]string{"CHARLIE_CONFIG_DIR": moved}, moved},
	} {
		var starts []string
		u := testUpdater(t, "", &starts) // New leaves Dir to Run
		var logPath string
		u.Start = func(_ string, _ []string, p string) error { logPath = p; return nil }
		app := &App{
			Version: "v0.0.0-20261102022616-c4fbbe8ccdac", Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
			Getenv: func(k string) string { return tc.env[k] }, ConfigDir: func() (string, error) { return base, nil },
			HTTPClient: http.DefaultClient, Updater: &u,
		}
		if code := app.Run(context.Background(), []string{"version"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if logPath != filepath.Join(tc.want, "update.log") {
			t.Errorf("env %v: log %s, want it in %s", tc.env, logPath, tc.want)
		}
		if _, err := os.Stat(filepath.Join(tc.want, "update.json")); err != nil {
			t.Errorf("env %v: %v", tc.env, err)
		}
	}
}

func TestCommandStartsBackgroundUpdateAfterwards(t *testing.T) {
	dir := t.TempDir()
	var starts []string
	r := runWithUpdater(t, dir, &starts, "version")
	if r.code != 0 || len(starts) != 1 || starts[0] != "update --background" {
		t.Fatalf("code %d, starts %v", r.code, starts)
	}
	// A failing command still triggers the check; a second one within the interval does not.
	runWithUpdater(t, dir, &starts, "plan", "show")
	if len(starts) != 1 {
		t.Fatalf("starts %v", starts)
	}
}

func TestUpdateCommandDoesNotStartAnotherUpdate(t *testing.T) {
	var starts []string
	runWithUpdater(t, t.TempDir(), &starts, "update", "--background")
	if len(starts) != 0 {
		t.Fatalf("starts %v", starts)
	}
}

func TestNoticeAfterBackgroundUpdate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(map[string]any{"lastCheck": time.Now(), "updatedFrom": "v0.0.0-1-a", "updatedTo": "v0.0.0-2-b"})
	if err := os.WriteFile(filepath.Join(dir, "update.json"), st, 0o600); err != nil {
		t.Fatal(err)
	}
	var starts []string
	r := runWithUpdater(t, dir, &starts, "version")
	if !strings.Contains(r.stderr, "charlie was updated from v0.0.0-1-a to v0.0.0-2-b") || strings.Contains(r.stdout, "updated") {
		t.Fatalf("stdout %q stderr %q", r.stdout, r.stderr)
	}
	r = runWithUpdater(t, dir, &starts, "version")
	if strings.Contains(r.stderr, "updated") {
		t.Fatalf("notice repeated: %q", r.stderr)
	}
}
