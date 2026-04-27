package wrapper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Metadata struct {
	Cwd         string
	Repo        string
	Branch      string
	Commit      string
	TmuxPane    string
	TmuxSession string
	WrapperPID  int
}

func Collect(ctx context.Context) Metadata {
	cwd, _ := os.Getwd()
	m := Metadata{
		Cwd:         cwd,
		TmuxPane:    os.Getenv("TMUX_PANE"),
		TmuxSession: os.Getenv("TMUX"),
		WrapperPID:  os.Getpid(),
	}
	if root, err := gitTopLevel(ctx, cwd); err == nil && root != "" {
		m.Repo = root
		m.Branch = gitOutput(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
		m.Commit = gitOutput(ctx, cwd, "rev-parse", "HEAD")
	}
	return m
}

func (m Metadata) Env() []string {
	out := []string{
		envKV("CCDASH_WRAPPER_PID", strconv.Itoa(m.WrapperPID)),
		envKV("CCDASH_GIT_REPO", filepath.Base(m.Repo)),
		envKV("CCDASH_GIT_BRANCH", m.Branch),
		envKV("CCDASH_GIT_COMMIT", m.Commit),
		envKV("CCDASH_TMUX_PANE", m.TmuxPane),
		envKV("CCDASH_TMUX_SESSION", m.TmuxSession),
	}
	return out
}

// Exec replaces the current process with `claude` plus the given args, after
// injecting wrapper env vars. Uses syscall.Exec so PIDs and signal handling
// behave identically to running `claude` directly.
func Exec(ctx context.Context, args []string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude binary not found in PATH: %w", err)
	}
	meta := Collect(ctx)
	env := mergeEnv(os.Environ(), meta.Env())
	argv := append([]string{bin}, args...)
	return syscall.Exec(bin, argv, env)
}

func envKV(k, v string) string { return k + "=" + v }

func mergeEnv(base, overrides []string) []string {
	idx := map[string]int{}
	for i, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		idx[kv[:eq]] = i
	}
	out := append([]string(nil), base...)
	for _, kv := range overrides {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if i, ok := idx[key]; ok {
			out[i] = kv
		} else {
			out = append(out, kv)
			idx[key] = len(out) - 1
		}
	}
	return out
}

func gitTopLevel(ctx context.Context, cwd string) (string, error) {
	out, err := runGit(ctx, cwd, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(out), err
}

func gitOutput(ctx context.Context, cwd string, args ...string) string {
	out, err := runGit(ctx, cwd, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "git", args...)
	cmd.Dir = cwd
	b, err := cmd.Output()
	return string(b), err
}
