// Package hubcfg is the device side of hub mode: which hub this machine's
// collector dials, and the device token it authenticates with. Written by
// `ccdash hub join`, removed by `ccdash hub leave`, read by the collector's
// hub loop on every connect attempt (so join/leave take effect without a
// collector restart).
//
// The file holds a secret (the device token), so it lives in the state dir
// next to the loopback token, mode 0600 — not in ~/.config with the remote
// client config.
package hubcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

// Config is the persisted join state.
type Config struct {
	URL   string `json:"url"`   // hub origin, e.g. https://ccdash.g3.lab-dev.net
	Token string `json:"token"` // device token issued by the hub's "Add device"
}

// ErrNotFound means this machine hasn't joined a hub.
var ErrNotFound = errors.New("not joined to a hub")

// Path is $XDG_STATE_HOME/ccdash/hub.json.
func Path() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.json"), nil
}

// Load reads the join state. A missing file wraps ErrNotFound.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("malformed %s: %w", path, err)
	}
	if cfg.URL == "" || cfg.Token == "" {
		return Config{}, fmt.Errorf("%s is missing url or token — re-run `ccdash hub join`", path)
	}
	return cfg, nil
}

// Save writes the join state 0600, write-then-rename.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove deletes the join state; a missing file is not an error.
func Remove() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
