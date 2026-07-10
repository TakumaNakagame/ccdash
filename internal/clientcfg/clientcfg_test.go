package clientcfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := Load()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load with no file = %v, want ErrNotFound", err)
	}
	// The error should name the path so the operator knows where the file
	// is expected.
	if !strings.Contains(err.Error(), "config.json") {
		t.Errorf("error %q should mention the config path", err)
	}
}

func TestLoadMalformed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfgDir := filepath.Join(dir, "ccdash")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("Load with malformed JSON should error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("malformed file must NOT read as not-found — that would silently ignore a broken config")
	}
	if !strings.Contains(err.Error(), "malformed") || !strings.Contains(err.Error(), "config.json") {
		t.Errorf("error %q should say malformed and name the path", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := Config{Remote: Remote{
		URL:       "http://192.168.20.132:9123",
		TokenFile: "/home/op/.config/ccdash/claude-code.token",
		SSHTarget: "claude-code",
	}}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}

	// Permissions: file 0600, dir 0700.
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perms = %o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir perms = %o, want 0700", perm)
	}
}

func TestSaveMergePreservesUnsetFields(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Remote: Remote{URL: "http://a:9123", SSHTarget: "a-host"}}); err != nil {
		t.Fatal(err)
	}
	// A caller doing load-modify-save (like `ccdash remote set --url`) must
	// end up with the untouched fields intact.
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remote.URL = "http://b:9123"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote.URL != "http://b:9123" || got.Remote.SSHTarget != "a-host" {
		t.Fatalf("merge lost fields: %+v", got)
	}
}
