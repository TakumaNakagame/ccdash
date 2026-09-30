// Package discovery scans ~/.claude/projects/*/*.jsonl to enumerate Claude
// Code sessions that we haven't observed via hooks (yet). Each .jsonl file is
// a session transcript; line 2 carries cwd / sessionId / gitBranch metadata,
// and the first user message gives us a human-readable title.
package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/redact"
)

type Discovered struct {
	SessionID      string
	Cwd            string
	GitBranch      string
	Title          string
	TranscriptPath string
	// LastModified is when the session last did something: the newest
	// entry timestamp in the transcript, falling back to the file mtime.
	// Not the mtime itself — `claude --resume` touches the file without
	// adding entries, so merely opening a session would bump it.
	LastModified time.Time
}

// Scan walks ~/.claude/projects looking for transcript files. The base
// argument lets callers override the directory for tests; pass "" for the
// default ~/.claude/projects.
func Scan(ctx context.Context, base string) ([]Discovered, error) {
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".claude", "projects")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Discovered
	for _, e := range entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			d, err := readTranscript(path)
			if err != nil {
				continue
			}
			// Skip our own summarize-spawned sessions; their first user
			// prompt always begins with the marker we inject in
			// internal/summarize.
			if strings.HasPrefix(d.Title, "[ccdash:summary]") {
				continue
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func readTranscript(path string) (Discovered, error) {
	d := Discovered{TranscriptPath: path}
	info, err := os.Stat(path)
	if err != nil {
		return d, err
	}
	d.LastModified = lastEntryTime(path, info)

	f, err := os.Open(path)
	if err != nil {
		return d, err
	}
	defer f.Close()

	// Use a generous buffer because individual JSONL lines can be large
	// (tool results, base64 thinking blocks, etc.).
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Cheap pre-check before json.Unmarshal: every interesting line we read
		// here has a top-level "type" or "sessionId" field.
		applyLine(line, &d)
		if d.SessionID != "" && d.Title != "" {
			break
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return d, fmt.Errorf("scan %s: %w", path, err)
	}
	if d.SessionID == "" {
		// Fall back to filename — Claude names transcripts <session_id>.jsonl.
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		d.SessionID = base
	}
	return d, nil
}

// tailCache memoizes lastEntryTime per transcript: discovery rescans
// every ~10 s and most files haven't changed since the last pass.
var (
	tailMu    sync.Mutex
	tailCache = map[string]tailEntry{}
)

type tailEntry struct {
	size  int64
	mtime time.Time
	ts    time.Time
}

// lastEntryTime returns the newest top-level "timestamp" in the
// transcript, reading only the tail of the file (transcripts can be tens
// of MB). Falls back to the mtime when no timestamp is found.
func lastEntryTime(path string, info os.FileInfo) time.Time {
	tailMu.Lock()
	c, ok := tailCache[path]
	tailMu.Unlock()
	if ok && c.size == info.Size() && c.mtime.Equal(info.ModTime()) {
		return c.ts
	}
	ts := scanTailTimestamp(path, info.Size())
	if ts.IsZero() {
		ts = info.ModTime()
	}
	tailMu.Lock()
	tailCache[path] = tailEntry{size: info.Size(), mtime: info.ModTime(), ts: ts}
	tailMu.Unlock()
	return ts
}

// scanTailTimestamp reads growing windows from the end of the file until
// a line with a timestamp turns up (a trailing tool result can be large).
func scanTailTimestamp(path string, size int64) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	for win := int64(64 << 10); ; win *= 4 {
		off := size - win
		if off < 0 {
			off = 0
		}
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return time.Time{}
		}
		lines := strings.Split(string(buf), "\n")
		if off > 0 {
			lines = lines[1:] // first line is likely cut mid-way
		}
		for i := len(lines) - 1; i >= 0; i-- {
			var e struct {
				Timestamp time.Time `json:"timestamp"`
			}
			if json.Unmarshal([]byte(lines[i]), &e) == nil && !e.Timestamp.IsZero() {
				return e.Timestamp
			}
		}
		if off == 0 || win >= 16<<20 {
			return time.Time{}
		}
	}
}

// applyLine pulls metadata or a title out of a single transcript line.
func applyLine(line []byte, d *Discovered) {
	// Try the metadata shape first (system/bridge_status carries cwd etc.).
	var meta struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionId"`
		Cwd       string          `json:"cwd"`
		GitBranch string          `json:"gitBranch"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &meta); err != nil {
		return
	}
	if meta.SessionID != "" && d.SessionID == "" {
		d.SessionID = meta.SessionID
	}
	if meta.Cwd != "" && d.Cwd == "" {
		d.Cwd = meta.Cwd
	}
	if meta.GitBranch != "" && d.GitBranch == "" {
		d.GitBranch = meta.GitBranch
	}
	if meta.Type == "user" && d.Title == "" && len(meta.Message) > 0 {
		d.Title = extractUserText(meta.Message)
	}
}

// extractUserText pulls the first plain-text user prompt out of a "user"
// message. It deliberately ignores tool_result content entries (which appear
// as arrays) so we get the human-typed prompt rather than command output.
func extractUserText(msg json.RawMessage) string {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	if m.Role != "user" {
		return ""
	}
	// Content can be a string (typed prompt) or an array of blocks
	// (tool_result etc.).
	if len(m.Content) > 0 && m.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return cleanTitle(s)
		}
	}
	return ""
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	// Skip Claude Code's auto-injected slash command wrappers like
	// "<command-name>...</command-name>\n<command-message>...".
	if strings.HasPrefix(s, "<command-") {
		return ""
	}
	// First line, collapsed.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The transcript may contain prompts that pasted in API keys / tokens
	// when the user was debugging an integration; mask before persisting
	// the result as a session title.
	return redact.String(strings.TrimSpace(s))
}
