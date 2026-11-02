// Package update keeps an installed charlie current.
//
// When a command ends, the CLI calls MaybeStart. At most once per interval it
// starts a detached "charlie update --background" and returns at once, so the
// user never waits for the network or a build. That process asks the Go
// module proxy (or GitHub directly, for this private module) for the newest
// version of the CLI, builds it with "go install" into a temporary directory
// and swaps it in place of the running executable. Renaming a running
// executable works on Windows too; overwriting it does not, which is why the
// swap goes through a rename.
//
// Only builds installed with "go install" update themselves: their build info
// carries a module version. Local "go build" binaries report "(devel)" and are
// left alone, as are machines without a Go toolchain.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// Module is the module whose @latest is installed.
	Module = "github.com/2026-Softbank-Hackathon-Charlie/cli"
	// Package is the main package of the charlie command.
	Package = Module + "/cmd/charlie"
	// privatePattern must be in GOPRIVATE: the repositories are private.
	privatePattern = "github.com/2026-Softbank-Hackathon-Charlie"

	// DefaultInterval is how often a finished command may trigger a check.
	DefaultInterval = 10 * time.Minute
	// lockStale is when an abandoned lock (a killed updater) stops counting.
	lockStale   = 15 * time.Minute
	maxLogBytes = 64 << 10
)

// Config is everything the updater touches, so tests can replace it.
type Config struct {
	// Dir holds update.json, update.lock and update.log (the charlie config dir).
	Dir string
	// Version is the running build's module version ("(devel)" for local builds).
	Version string
	Getenv  func(string) string
	Now     func() time.Time
	// Executable returns the path of the running binary.
	Executable func() (string, error)
	// LookPath finds the go command.
	LookPath func(string) (string, error)
	// Run runs a command in dir with env and returns its combined output.
	Run func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
	// Start launches exe detached from this process, appending output to logPath.
	Start func(exe string, args []string, logPath string) error
}

// Default returns the real configuration for the charlie config dir.
func Default(dir string) Config {
	return Config{
		Dir:        dir,
		Version:    BuildVersion(),
		Getenv:     os.Getenv,
		Now:        time.Now,
		Executable: os.Executable,
		LookPath:   exec.LookPath,
		Run:        run,
		Start:      startDetached,
	}
}

// BuildVersion is the module version recorded in the running binary.
func BuildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Version
	}
	return ""
}

// Installed reports whether this build came from "go install module@version".
func Installed(version string) bool {
	return semver.IsValid(version)
}

type state struct {
	LastCheck   time.Time `json:"lastCheck"`
	UpdatedFrom string    `json:"updatedFrom,omitempty"`
	UpdatedTo   string    `json:"updatedTo,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
}

func (c Config) statePath() string { return filepath.Join(c.Dir, "update.json") }
func (c Config) lockPath() string  { return filepath.Join(c.Dir, "update.lock") }

// LogPath is where background updates write their output.
func (c Config) LogPath() string { return filepath.Join(c.Dir, "update.log") }

func (c Config) load() state {
	var s state
	if b, err := os.ReadFile(c.statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (c Config) save(s state) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.statePath())
}

// disabled says why automatic updates are off, or "" when they are on.
func (c Config) disabled() string {
	switch {
	case c.Getenv("CHARLIE_NO_UPDATE") != "":
		return "CHARLIE_NO_UPDATE is set"
	case !Installed(c.Version):
		return "this is a development build (not installed with go install)"
	}
	if _, err := c.LookPath("go"); err != nil {
		return "the go command is not on PATH"
	}
	return ""
}

func (c Config) interval() time.Duration {
	if v := c.Getenv("CHARLIE_UPDATE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return DefaultInterval
}

// MaybeStart starts a background update when one is due. It never fails the
// caller's command: problems are recorded in update.json and the log.
func MaybeStart(c Config) {
	if c.disabled() != "" {
		return
	}
	s := c.load()
	now := c.Now()
	if !s.LastCheck.IsZero() && now.Sub(s.LastCheck) < c.interval() {
		return
	}
	// Record the attempt first, so commands that end together start one updater.
	s.LastCheck = now
	if c.save(s) != nil {
		return
	}
	exe, err := c.Executable()
	if err != nil {
		return
	}
	trimLog(c.LogPath())
	if err := c.Start(exe, []string{"update", "--background"}, c.LogPath()); err != nil {
		s.LastError = "start background update: " + err.Error()
		_ = c.save(s)
	}
}

// TakeNotice returns a one-line message about an update that happened in the
// background since the last command, and forgets it.
func TakeNotice(c Config) string {
	s := c.load()
	if s.UpdatedTo == "" {
		return ""
	}
	msg := fmt.Sprintf("charlie was updated from %s to %s", s.UpdatedFrom, s.UpdatedTo)
	s.UpdatedFrom, s.UpdatedTo = "", ""
	_ = c.save(s)
	return msg
}

// Result is what Update did.
type Result struct {
	From, To string
	Updated  bool
}

// ErrBusy means another update is running.
var ErrBusy = errors.New("another charlie update is running")

// Update installs the newest version if it is newer than the running one.
// Progress lines go to log.
func Update(ctx context.Context, c Config, log io.Writer) (Result, error) {
	res := Result{From: c.Version, To: c.Version}
	// CHARLIE_NO_UPDATE only turns off the automatic check; asking works.
	if !Installed(c.Version) {
		return res, errors.New("cannot update: this is a development build (not installed with go install)")
	}
	goCmd, err := c.LookPath("go")
	if err != nil {
		return res, fmt.Errorf("cannot update: the go command is not on PATH")
	}
	unlock, err := c.lock()
	if err != nil {
		return res, err
	}
	defer unlock()

	err = func() error {
		env := goEnv(os.Environ())
		workDir := os.TempDir() // outside any module, so the current directory cannot interfere
		out, err := c.Run(ctx, workDir, env, goCmd, "list", "-m", "-json", Module+"@latest")
		if err != nil {
			return fmt.Errorf("find the latest version: %v: %s", err, firstLine(out))
		}
		var mod struct{ Version string }
		if err := json.Unmarshal(out, &mod); err != nil || mod.Version == "" {
			return fmt.Errorf("find the latest version: unexpected go list output: %s", firstLine(out))
		}
		res.To = mod.Version
		if semver.Compare(mod.Version, c.Version) <= 0 {
			fmt.Fprintf(log, "charlie %s is up to date\n", c.Version)
			return nil
		}
		fmt.Fprintf(log, "installing charlie %s (running %s)\n", mod.Version, c.Version)

		tmp, err := os.MkdirTemp("", "charlie-update-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		out, err = c.Run(ctx, workDir, append(env, "GOBIN="+tmp), goCmd, "install", Package+"@"+mod.Version)
		if err != nil {
			return fmt.Errorf("go install: %v: %s", err, firstLine(out))
		}
		exe, err := c.Executable()
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		built := filepath.Join(tmp, "charlie"+filepath.Ext(exe))
		if err := swap(exe, built); err != nil {
			return fmt.Errorf("replace %s: %w", exe, err)
		}
		res.Updated = true
		fmt.Fprintf(log, "updated %s to %s\n", exe, mod.Version)
		return nil
	}()

	s := c.load()
	s.LastCheck = c.Now()
	s.LastError = ""
	if err != nil {
		s.LastError = err.Error()
	}
	if res.Updated {
		s.UpdatedFrom, s.UpdatedTo = res.From, res.To
	}
	_ = c.save(s)
	return res, err
}

// CleanOld removes the previous executable left by a swap. On Windows it can
// only go once that old process has exited, so every run tries again.
func CleanOld(exe string) {
	_ = os.Remove(exe + ".old")
}

// swap puts built in place of exe, keeping exe as exe.old until it can be removed.
func swap(exe, built string) error {
	if _, err := os.Stat(built); err != nil {
		return fmt.Errorf("go install produced no binary: %w", err)
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := moveFile(built, exe); err != nil {
		_ = os.Rename(old, exe) // put the working binary back
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(old) // a running binary can be unlinked here
	}
	return nil
}

// moveFile renames, or copies when the temporary directory is on another volume.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

func (c Config) lock() (func(), error) {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, err
	}
	p := c.lockPath()
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, serr := os.Stat(p); serr == nil && c.Now().Sub(fi.ModTime()) > lockStale {
			_ = os.Remove(p) // left behind by a killed updater
			continue
		}
		return nil, ErrBusy
	}
	return nil, ErrBusy
}

// goEnv adds the organisation to GOPRIVATE: its modules are private, so the
// public proxy and checksum database cannot serve them.
func goEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	private := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "GOPRIVATE="); ok {
			private = v
			continue
		}
		out = append(out, kv)
	}
	if !strings.Contains(private, privatePattern) {
		if private != "" {
			private += ","
		}
		private += privatePattern
	}
	return append(out, "GOPRIVATE="+private, "GOFLAGS=-mod=mod")
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	hideWindow(cmd)
	return cmd.CombinedOutput()
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// trimLog keeps the background log from growing without bound.
func trimLog(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogBytes {
		_ = os.Remove(path)
	}
}
