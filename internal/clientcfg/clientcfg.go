// Package clientcfg reads and writes the CLIENT-side ccdash config at
// $XDG_CONFIG_HOME/ccdash/config.json (default ~/.config/ccdash/config.json).
// It holds where a remote collector lives so `ccdash -r` works without
// per-invocation flags. This is deliberately separate from internal/paths's
// state dir ($XDG_STATE_HOME): state (DB, token) belongs to a collector
// host, config belongs to the operator's client machine.
package clientcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound reports that no config file exists yet. Callers treat this as
// "not configured" (a normal state for local-mode users) rather than a hard
// failure — distinct from a malformed file, which always errors loudly.
var ErrNotFound = errors.New("ccdash client config not found")

// Remote is the `"remote"` object in config.json.
type Remote struct {
	// URL is the collector origin, e.g. "http://192.168.20.132:9123".
	URL string `json:"url,omitempty"`
	// TokenFile is the path to a local copy of the collector's token.
	TokenFile string `json:"token_file,omitempty"`
	// SSHTarget is the user@host ssh attach uses; defaults to the URL's
	// hostname when empty.
	SSHTarget string `json:"ssh_target,omitempty"`
}

// Config is the whole config.json document.
type Config struct {
	Remote Remote `json:"remote"`
}

// Path returns $XDG_CONFIG_HOME/ccdash/config.json, defaulting
// XDG_CONFIG_HOME to ~/.config. It does NOT create the directory — Save
// does that; Load treats a missing path as ErrNotFound.
func Path() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "ccdash", "config.json"), nil
}

// Load reads and parses the config file. A missing file comes back as an
// error wrapping ErrNotFound; a malformed file errors with the path so the
// operator knows exactly what to fix.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%w (%s)", ErrNotFound, path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("malformed config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the config with 0600 perms (the file names a token path, not
// a secret itself, but there's no reason to share it), creating the
// directory 0700 on first use. Write-then-rename so a crash can't leave a
// half-written JSON behind.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
