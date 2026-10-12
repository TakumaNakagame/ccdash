// Package claudecfg touches Claude Code's own config file (~/.claude.json)
// for the one thing ccdash needs there: marking a folder trusted before it
// starts claude in it, so a new folder under an operator-listed trusted
// root doesn't stop at Claude Code's "Do you trust the files in this
// folder?" question (settings: trusted_dirs).
//
// Claude Code walks up from the cwd looking for a trusted folder, but stops
// at a git repository's root, so trusting the parent alone isn't enough —
// each folder gets its own entry, the shape Claude Code writes itself
// ({"hasTrustDialogAccepted": true} under projects[<path>]).
package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path is Claude Code's global config: $CLAUDE_CONFIG_DIR/.claude.json,
// else ~/.claude.json.
func Path() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// Under reports whether dir is one of roots or inside one. roots is the
// trusted_dirs setting: comma- or newline-separated, "~/" allowed.
func Under(dir, roots string) bool {
	dir = filepath.Clean(dir)
	for _, r := range SplitRoots(roots) {
		if dir == r || strings.HasPrefix(dir, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// SplitRoots parses the trusted_dirs setting into clean absolute paths.
func SplitRoots(roots string) []string {
	home, _ := os.UserHomeDir()
	var out []string
	for _, r := range strings.FieldsFunc(roots, func(c rune) bool { return c == ',' || c == '\n' }) {
		r = strings.TrimSpace(r)
		if r == "~" {
			r = home
		} else if strings.HasPrefix(r, "~/") && home != "" {
			r = filepath.Join(home, r[2:])
		}
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		out = append(out, filepath.Clean(r))
	}
	return out
}

// Trust marks dir (and its resolved real path, if different) trusted in
// the config at path. It reports whether the file changed. A missing file
// is left alone (claude hasn't run here yet; nothing to merge into).
// Everything else in the file is kept: numbers stay as written, and the
// write is a same-directory temp file renamed over the original.
func Trust(path, dir string) (bool, error) {
	cfg, err := load(path)
	if err != nil {
		return false, err
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	keys := []string{filepath.Clean(dir)}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != keys[0] {
		keys = append(keys, real)
	}
	changed := false
	for _, k := range keys {
		p, _ := projects[k].(map[string]any)
		if p == nil {
			p = map[string]any{}
			projects[k] = p
		}
		if v, _ := p["hasTrustDialogAccepted"].(bool); !v {
			p["hasTrustDialogAccepted"] = true
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, save(path, cfg)
}

// load reads the config at path as a JSON object, numbers kept as written.
func load(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg == nil {
		return nil, errors.New(path + " is not a JSON object")
	}
	return cfg, nil
}

// save writes cfg over path: a same-directory temp file renamed over the
// original, keeping its mode.
func save(path string, cfg map[string]any) error {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.ccdash-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bytes.TrimRight(out.Bytes(), "\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
