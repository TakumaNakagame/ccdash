package gitinfo

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Info struct {
	Repo   string // basename of repo top-level
	Branch string
	Commit string
}

// Lookup runs git in cwd to derive repo/branch/commit. Returns zero values on
// any error (e.g. cwd is not a git repo, git not on PATH).
func Lookup(ctx context.Context, cwd string) Info {
	if cwd == "" {
		return Info{}
	}
	top := run(ctx, cwd, "rev-parse", "--show-toplevel")
	if top == "" {
		return Info{}
	}
	return Info{
		Repo:   filepath.Base(top),
		Branch: run(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD"),
		Commit: run(ctx, cwd, "rev-parse", "HEAD"),
	}
}

func run(ctx context.Context, cwd string, args ...string) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
