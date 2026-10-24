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
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

// Config is the persisted join state.
type Config struct {
	URL   string `json:"url"`   // hub origin, e.g. https://ccdash.example.net
	Token string `json:"token"` // device token issued by the hub's "Add device"
	// Letter and Name are what the hub calls this machine, written by the
	// collector each time its tunnel connects (the hub sends them on the
	// dial). Letter prefixes session numbers ("A42") so they are unique
	// across machines.
	Letter string `json:"letter,omitempty"`
	Name   string `json:"name,omitempty"`
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

var letterCache struct {
	sync.Mutex
	mtime  time.Time
	letter string
}

// Letter is this machine's hub letter ("" when not joined or not yet told),
// re-read only when hub.json changes — cheap enough for every list read.
func Letter() string {
	p, err := Path()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	letterCache.Lock()
	defer letterCache.Unlock()
	if fi.ModTime().Equal(letterCache.mtime) {
		return letterCache.letter
	}
	cfg, err := Load()
	letterCache.mtime = fi.ModTime()
	letterCache.letter = ""
	if err == nil {
		letterCache.letter = cfg.Letter
	}
	return letterCache.letter
}

// SetIdentity records the hub's letter and name for this machine, if they
// changed (the collector calls it on each connect).
func SetIdentity(letter, name string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	if cfg.Letter == letter && cfg.Name == name {
		return nil
	}
	cfg.Letter, cfg.Name = letter, name
	return Save(cfg)
}

// WithLetter stamps this machine's hub letter on session rows.
func WithLetter(ss []model.Session) []model.Session {
	l := Letter()
	if l == "" {
		return ss
	}
	for i := range ss {
		ss[i].HostLetter = l
	}
	return ss
}
