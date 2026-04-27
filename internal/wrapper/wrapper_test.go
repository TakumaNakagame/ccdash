package wrapper

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestCollectMetadata(t *testing.T) {
	m := Collect(context.Background())
	if m.Cwd == "" {
		t.Fatal("expected cwd to be set")
	}
	if m.WrapperPID == 0 {
		t.Fatal("expected wrapper pid")
	}
	// We're inside a git repo (the ccmanage repo itself? actually no — task.md is here but no .git).
	// Just check that branch is either empty or non-empty without panicking.
	t.Logf("repo=%q branch=%q commit=%q", m.Repo, m.Branch, m.Commit)
}

func TestEnvIncludesAllVars(t *testing.T) {
	m := Metadata{WrapperPID: 42, Branch: "main", Commit: "abc"}
	env := m.Env()
	wanted := []string{
		"CCDASH_WRAPPER_PID=42",
		"CCDASH_GIT_BRANCH=main",
		"CCDASH_GIT_COMMIT=abc",
	}
	for _, w := range wanted {
		found := false
		for _, e := range env {
			if e == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing env entry %q in %v", w, env)
		}
	}
}

func TestMergeEnvOverwrites(t *testing.T) {
	base := []string{"FOO=1", "BAR=2"}
	out := mergeEnv(base, []string{"FOO=99", "BAZ=3"})
	if !contains(out, "FOO=99") || !contains(out, "BAR=2") || !contains(out, "BAZ=3") {
		t.Fatalf("merge mismatch: %v", out)
	}
	// FOO appears only once
	count := 0
	for _, kv := range out {
		if strings.HasPrefix(kv, "FOO=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("FOO present %d times, want 1", count)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Compile-time check that Exec isn't broken (we don't actually run it).
var _ = func(args []string) error { return Exec(context.Background(), args) }

// Avoid unused import on tests.
var _ = os.Stdout
