package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/TakumaNakagame/ccdash/internal/attach"
	"github.com/TakumaNakagame/ccdash/internal/hubcfg"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/redact"
)

// Orchestrated starts (docs/decisions/0009-orchestrator.md §5, §11): what
// POST /pty/start does on top of a plain spawn when ccdash mcp's
// start_session asks for it with "orchestrated": true. The TUI's n, the
// portal's new session and handoff-mcp never set it, so none of this
// touches them.
//
//  1. cwd: the request's, else the project's directory (projects.dir).
//  2. The operator's notes — the common brief and the project's — go after
//     the caller's prompt under fixed headings, unless "plain" (or the
//     prompt is empty: that just opens claude). Composed here, on the
//     machine that hosts the new session, so a start from another host
//     gets THIS machine's notes and the kept brief is exactly what the
//     session received.
//  3. The session_setup_command setting, when set, runs before claude and
//     may name the directory to start in.

// setupTimeout is how long the setup command may run; a variable so tests
// can shorten it.
var setupTimeout = 60 * time.Second

// setupErrMax clips the stderr tail quoted in a failed start.
const setupErrMax = 600

// prepareStart applies the three steps to req and returns it ready for
// spawnPTY, or an HTTP status and error.
func (s *Server) prepareStart(ctx context.Context, req ptyStartReq) (ptyStartReq, int, error) {
	if s.db == nil {
		return req, 0, nil
	}
	if strings.HasPrefix(req.Prompt, "-") {
		// Refused by spawnPTY too; say so before the setup command runs.
		return req, http.StatusBadRequest, fmt.Errorf("prompt must not start with '-'")
	}
	project := strings.TrimSpace(req.Project)
	dir, pbrief := "", ""
	if project != "" {
		var err error
		if dir, pbrief, err = s.db.ProjectStart(ctx, project); err != nil {
			return req, http.StatusInternalServerError, err
		}
	}
	if req.Cwd == "" {
		req.Cwd = dir
	}
	if !req.Plain {
		common, err := s.db.BriefCommon(ctx)
		if err != nil {
			return req, http.StatusInternalServerError, err
		}
		own := req.Prompt
		req.Prompt, req.notes = model.ComposeBrief(own, common, pbrief)
		if req.notes != nil {
			// ComposeBrief trims the caller's trailing blank space before
			// the first heading.
			req.own = strings.TrimRight(own, "\n\t ")
		}
	}
	if cmd := strings.TrimSpace(s.startDefaults().SessionSetupCommand); cmd != "" {
		cwd, err := s.runSetup(ctx, cmd, req, project)
		if err != nil {
			return req, http.StatusBadGateway, err
		}
		if cwd != "" {
			req.Cwd = cwd
		}
	}
	switch {
	case req.Cwd == "" && project == "":
		return req, http.StatusBadRequest, fmt.Errorf("no directory to start in: pass cwd, or a project that has a working directory")
	case req.Cwd == "":
		return req, http.StatusBadRequest, fmt.Errorf("no directory to start in: project %q has no working directory on this machine (the operator sets it in the portal's project menu or with `ccdash brief project %s --dir <path>`); pass cwd", project, project)
	}
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		return req, http.StatusBadRequest, fmt.Errorf("%s is not a directory on this machine", req.Cwd)
	}
	return req, 0, nil
}

// runSetup runs the operator's setup command through the login shell (like
// the claude it precedes, so the same PATH and functions apply) and
// returns the directory it named: the last non-empty line of its stdout,
// when that is an existing absolute directory. No such line keeps the cwd
// (""). A non-zero exit or a timeout is an error carrying the tail of its
// stderr, redacted.
func (s *Server) runSetup(ctx context.Context, command string, req ptyStartReq, project string) (string, error) {
	f, err := os.CreateTemp("", "ccdash-brief-*.md")
	if err != nil {
		return "", fmt.Errorf("setup command: %w", err)
	}
	defer os.Remove(f.Name())
	// CreateTemp makes it 0600; the brief is the operator's text and the
	// caller's, readable by this user only.
	_, werr := f.WriteString(req.Prompt)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", fmt.Errorf("setup command: %w", werr)
	}

	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	shell := os.Getenv("SHELL")
	args := []string{"-i", "-c", command}
	if shell == "" {
		shell, args = "sh", []string{"-c", command}
	}
	c := exec.CommandContext(ctx, shell, args...)
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err == nil && fi.IsDir() {
			c.Dir = req.Cwd
		}
	}
	c.Env = append(attach.SafeEnv(),
		"CCDASH_BRIEF_FILE="+f.Name(),
		"CCDASH_PROJECT="+project,
		"CCDASH_TITLE="+briefTitle(req.Prompt),
		"CCDASH_CWD="+req.Cwd,
		"CCDASH_PARENT="+s.parentRef(ctx, req.Parent),
	)
	stdout, stderr := &tailWriter{max: 1 << 16}, &tailWriter{max: 1 << 16}
	c.Stdout, c.Stderr = stdout, stderr
	// Its own process group, so a timeout takes the whole pipeline down,
	// not just the shell; WaitDelay stops a stray grandchild holding the
	// pipes from keeping Run waiting.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	err = c.Run()
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("the setup command (setting session_setup_command) did not finish within %s; nothing was started%s", setupTimeout, stderrTail(stderr.String()))
	}
	if err != nil {
		return "", fmt.Errorf("the setup command (setting session_setup_command) failed (%v); nothing was started%s", err, stderrTail(stderr.String()))
	}
	return setupDir(stdout.String()), nil
}

// setupDir picks the directory out of the setup command's stdout: its last
// non-empty line, when that is an existing absolute directory.
func setupDir(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if !filepath.IsAbs(l) {
			return ""
		}
		if fi, err := os.Stat(l); err != nil || !fi.IsDir() {
			return ""
		}
		return filepath.Clean(l)
	}
	return ""
}

// stderrTail is the end of the setup command's stderr as an error suffix.
func stderrTail(errOut string) string {
	// An interactive shell without a terminal complains about job control
	// on every run; that is never the reason the command failed.
	var keep []string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.Contains(l, "job control") || strings.Contains(l, "cannot set terminal process group") {
			continue
		}
		keep = append(keep, l)
	}
	t := strings.TrimSpace(redact.String(strings.Join(keep, "\n")))
	if t == "" {
		return ""
	}
	if len(t) > setupErrMax {
		t = t[len(t)-setupErrMax:]
		for !utf8.ValidString(t) && len(t) > 0 { // don't start mid-rune
			t = t[1:]
		}
		t = "…" + t
	}
	return ": " + t
}

// briefTitle is the first non-empty line of a prompt, clipped — what the
// setup command gets as CCDASH_TITLE.
func briefTitle(prompt string) string {
	for _, l := range strings.Split(prompt, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#"))
		if l == "" {
			continue
		}
		if r := []rune(l); len(r) > 80 {
			l = string(r[:80])
		}
		return l
	}
	return ""
}

// parentRef names the starting session for the setup command: its ref
// ("B880") when it is a session of this machine, else the id it came with.
func (s *Server) parentRef(ctx context.Context, parent string) string {
	if parent == "" {
		return ""
	}
	if x, ok, err := s.db.GetSession(ctx, parent); err == nil && ok && x.Num > 0 {
		return hubcfg.WithLetter([]model.Session{x})[0].Ref()
	}
	return parent
}

// tailWriter keeps about the last max bytes written — the end is what
// matters (the directory line, the error) — so a chatty setup command
// can't grow the collector.
type tailWriter struct {
	buf bytes.Buffer
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 2*t.max {
		b := t.buf.Bytes()
		keep := append([]byte(nil), b[len(b)-t.max:]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return t.buf.String() }
