// Package procmap correlates Claude Code sessions with running processes and
// tmux panes. Claude maintains an authoritative map at ~/.claude/sessions/<pid>.json
// so we read those files (one per running claude process) instead of trying
// to scrape /proc.
package procmap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type Entry struct {
	SessionID    string
	PID          int
	Cwd          string
	Kind         string // "interactive", etc.
	ClaudeStatus string // "idle", "busy"
	TTY          string // e.g. "/dev/pts/14"
	Pane         string // e.g. "0:1.0" — tmux pane address
	TmuxSession  string
}

// Snapshot returns a map of session_id → Entry by reading
// ~/.claude/sessions/<pid>.json for every running claude process.
func Snapshot(ctx context.Context) (map[string]Entry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sessDir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]Entry{}, nil
		}
		return nil, err
	}

	tmuxPanes := tmuxPanesByTTY(ctx)
	out := map[string]Entry{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(sessDir, e.Name())
		entry, ok := readEntry(path)
		if !ok {
			continue
		}
		// Filter to processes that are actually still alive. Stale .json files
		// linger after a session exits; we don't want to mark those active.
		if !pidAlive(entry.PID) {
			continue
		}
		entry.TTY = ttyForPID(entry.PID)
		if entry.TTY != "" {
			if pane, ok := tmuxPanes[entry.TTY]; ok {
				entry.Pane = pane.address
				entry.TmuxSession = pane.session
			}
		}
		out[entry.SessionID] = entry
	}
	return out, nil
}

func readEntry(path string) (Entry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, false
	}
	var raw struct {
		PID       int    `json:"pid"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Kind      string `json:"kind"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Entry{}, false
	}
	if raw.SessionID == "" || raw.PID == 0 {
		return Entry{}, false
	}
	return Entry{
		SessionID:    raw.SessionID,
		PID:          raw.PID,
		Cwd:          raw.Cwd,
		Kind:         raw.Kind,
		ClaudeStatus: raw.Status,
	}, true
}

func pidAlive(pid int) bool {
	// Linux fast path: /proc/<pid>/comm exists iff the process is alive.
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err == nil {
		return strings.TrimSpace(string(b)) == "claude"
	}
	// macOS fallback: Signal(0) checks process existence without killing it.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func ttyForPID(pid int) string {
	// Linux: resolve the TTY via /proc/<pid>/fd symlinks.
	for _, fd := range []string{"0", "1", "2"} {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, "/dev/pts/") || strings.HasPrefix(target, "/dev/ttys") {
			return target
		}
	}
	// macOS fallback: ask ps for the controlling TTY.
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" || tty == "??" {
		return ""
	}
	// ps returns the basename (e.g. "s001"); prepend /dev/tty.
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "/dev/tty" + tty
	}
	return tty
}

type paneInfo struct {
	address string
	session string
}

// tmuxPanesByTTY returns a map keyed by /dev/pts/N for each tmux pane.
// Empty map if tmux is not available.
func tmuxPanesByTTY(ctx context.Context) map[string]paneInfo {
	out := map[string]paneInfo{}
	cmd := exec.CommandContext(ctx, "tmux", "list-panes", "-aF",
		"#{pane_tty}|#{session_name}:#{window_index}.#{pane_index}|#{session_name}")
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		out[parts[0]] = paneInfo{address: parts[1], session: parts[2]}
	}
	return out
}
