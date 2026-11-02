package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// config is the saved login.
type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath(dir func() (string, error)) (string, error) {
	d, err := dir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w", err)
	}
	return filepath.Join(d, "charlie", "config.json"), nil
}

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

// saveConfig writes the token file 0600 in a 0700 directory. It renames a
// temporary file into place so a crash never leaves a half-written file, and
// a pre-existing file never keeps wider permissions.
func saveConfig(path string, c config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "config-*.json") // created 0600
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

// removeConfig reports whether there was a file to remove.
func removeConfig(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
