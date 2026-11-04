package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files in the charlie config directory.
const (
	configFile  = "config.json"
	pendingFile = "pending-login.json"
)

// config is the saved login.
type config struct {
	Server    string     `json:"server"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // unknown for older files
}

// pendingLogin is a device login waiting for an administrator's approval. It
// is saved so that a later "charlie login", possibly another process, shows
// the same code and finishes the login.
type pendingLogin struct {
	Server          string    `json:"server"`
	DeviceCode      string    `json:"deviceCode"` // secret: never printed
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationUri"`
	ExpiresAt       time.Time `json:"expiresAt"` // as the server said when it started
	Interval        int       `json:"interval"`  // seconds between polls
}

// configDir is the directory holding config.json, pending-login.json and the
// update files: CHARLIE_CONFIG_DIR, else charlie below the user config
// directory.
func (a *App) configDir() (string, error) {
	if d := a.Getenv("CHARLIE_CONFIG_DIR"); d != "" {
		// Absolute, so the paths charlie prints say where the files are.
		return filepath.Abs(d)
	}
	d, err := a.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w (set CHARLIE_CONFIG_DIR)", err)
	}
	return filepath.Join(d, "charlie"), nil
}

// ownConfigDir reports whether the config directory is charlie's own (the
// default one), which it tightens to 0700. A CHARLIE_CONFIG_DIR may be a
// shared volume owned by someone else; there the 0600 files protect the
// secrets.
func (a *App) ownConfigDir() bool { return a.Getenv("CHARLIE_CONFIG_DIR") == "" }

// loadConfig returns an empty config when the file does not exist.
func loadConfig(path string) (config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config{}, nil
	}
	if err != nil {
		return config{}, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return config{}, fmt.Errorf("read %s: %w", path, err)
	}
	return c, nil
}

// loadPending returns nil when there is no pending login. A damaged file
// counts as none: the next device login replaces it.
func loadPending(path string) (*pendingLogin, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingLogin
	if json.Unmarshal(b, &p) != nil || p.Server == "" || p.DeviceCode == "" || p.UserCode == "" {
		return nil, nil
	}
	return &p, nil
}

func saveConfig(path string, c config, ownDir bool) error { return writePrivate(path, c, ownDir) }

func savePending(path string, p *pendingLogin, ownDir bool) error {
	return writePrivate(path, p, ownDir)
}

// writePrivate writes v as JSON with mode 0600, since both files hold
// secrets. A directory it creates is 0700, and so is an existing one when
// ownDir says it is charlie's own. It renames a temporary file into place so
// a crash never leaves a half-written file, and a pre-existing file never
// keeps wider permissions.
func writePrivate(path string, v any, ownDir bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if ownDir {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	pattern := strings.TrimSuffix(filepath.Base(path), ".json") + "-*.json"
	f, err := os.CreateTemp(dir, pattern) // created 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeFile reports whether there was a file to remove.
func removeFile(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
