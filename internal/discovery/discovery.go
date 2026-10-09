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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/redact"
	"github.com/takumanakagame/ccmanage/internal/transcript"
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
	// adding entries, so merely opening a session would bump it. Drives
	// the active/idle status.
	LastModified time.Time
	// LastPrompt is when the operator last typed a prompt (falls back to
	// LastModified). This is the session's time in the list.
	LastPrompt time.Time
	// Prompts counts the operator-typed prompts, capped at PromptCap
	// (enough to decide when a session is worth an automatic title).
	Prompts int
}

// PromptCap is where Discovered.Prompts stops counting.
const PromptCap = 2

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
	d.LastModified, d.LastPrompt = tailTimes(path, info)
	d.Prompts = countPrompts(path, info.Size())

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

// tailCache memoizes the tail scan per transcript: discovery rescans
// every ~10 s and most files haven't changed since the last pass.
var (
	tailMu    sync.Mutex
	tailCache = map[string]tailEntry{}
)

type tailEntry struct {
	size       int64
	mtime      time.Time
	lastEntry  time.Time
	lastPrompt time.Time
}

// tailTimes returns the newest entry timestamp and the newest real user
// prompt timestamp in the transcript, reading only its tail (transcripts
// can be tens of MB). lastEntry falls back to the mtime, lastPrompt to
// lastEntry, when not found.
func tailTimes(path string, info os.FileInfo) (lastEntry, lastPrompt time.Time) {
	tailMu.Lock()
	c, ok := tailCache[path]
	tailMu.Unlock()
	if ok && c.size == info.Size() && c.mtime.Equal(info.ModTime()) {
		return c.lastEntry, c.lastPrompt
	}
	lastEntry, lastPrompt = scanTail(path, info.Size())
	if lastEntry.IsZero() {
		lastEntry = info.ModTime()
	}
	if lastPrompt.IsZero() {
		lastPrompt = lastEntry
	}
	tailMu.Lock()
	tailCache[path] = tailEntry{size: info.Size(), mtime: info.ModTime(), lastEntry: lastEntry, lastPrompt: lastPrompt}
	tailMu.Unlock()
	return lastEntry, lastPrompt
}

// promptCache remembers, per transcript, how far countPrompts has read and
// how many prompts it saw, so each tick only reads what was appended —
// and nothing at all once the count reached PromptCap.
var promptCache = map[string]promptEntry{}

type promptEntry struct {
	offset  int64
	prompts int
}

// countPrompts returns how many operator-typed prompts the transcript
// holds, up to PromptCap, reading only complete lines past the cached
// offset.
func countPrompts(path string, size int64) int {
	tailMu.Lock()
	c := promptCache[path]
	tailMu.Unlock()
	if c.prompts >= PromptCap || size == c.offset {
		return c.prompts
	}
	if size < c.offset { // rewritten
		c = promptEntry{}
	}
	f, err := os.Open(path)
	if err != nil {
		return c.prompts
	}
	defer f.Close()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return c.prompts
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for c.prompts < PromptCap {
		line, err := r.ReadBytes('\n')
		if err != nil { // EOF or a partial last line: read it next time
			break
		}
		c.offset += int64(len(line))
		if _, prompt := lineTimes(line); prompt {
			c.prompts++
		}
	}
	tailMu.Lock()
	promptCache[path] = c
	tailMu.Unlock()
	return c.prompts
}

// scanTail reads growing windows from the end of the file until it has
// seen both an entry timestamp and a real prompt (a long tool run after
// the last prompt can be megabytes). Capped at 16 MiB.
func scanTail(path string, size int64) (lastEntry, lastPrompt time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	for win := int64(64 << 10); ; win *= 4 {
		off := size - win
		if off < 0 {
			off = 0
		}
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return
		}
		lines := strings.Split(string(buf), "\n")
		if off > 0 {
			lines = lines[1:] // first line is likely cut mid-way
		}
		for i := len(lines) - 1; i >= 0 && lastPrompt.IsZero(); i-- {
			ts, prompt := lineTimes([]byte(lines[i]))
			if ts.IsZero() {
				continue
			}
			if lastEntry.IsZero() {
				lastEntry = ts
			}
			if prompt {
				lastPrompt = ts
			}
		}
		if !lastPrompt.IsZero() || off == 0 || win >= 16<<20 {
			return
		}
	}
}

// lineTimes parses one transcript line: its timestamp, and whether it is
// a prompt the operator actually typed — a main-thread user turn with
// text that isn't Claude Code boilerplate (tool results, meta entries,
// local-command output and sidechain turns don't count).
func lineTimes(line []byte) (time.Time, bool) {
	var e struct {
		Type        string    `json:"type"`
		Timestamp   time.Time `json:"timestamp"`
		IsMeta      bool      `json:"isMeta"`
		IsSidechain bool      `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Timestamp.IsZero() {
		return time.Time{}, false
	}
	if e.Type != "user" || e.IsMeta || e.IsSidechain || len(e.Message.Content) == 0 {
		return e.Timestamp, false
	}
	return e.Timestamp, hasTypedText(e.Message.Content)
}

// hasTypedText reports whether a user message content (a string, or an
// array of blocks) carries operator-typed text.
func hasTypedText(content json.RawMessage) bool {
	if content[0] == '"' {
		var s string
		return json.Unmarshal(content, &s) == nil && strings.TrimSpace(s) != "" && !transcript.IsNoise(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" && !transcript.IsNoise(b.Text) {
			return true
		}
	}
	return false
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

var pastedMarker = regexp.MustCompile(`</?pasted_content id="[^"]*">`)

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	// Skip text Claude Code injects as a "user" turn: slash command
	// wrappers ("<command-name>…"), local-command output and its caveat,
	// system reminders. The next real prompt becomes the title instead.
	if transcript.IsNoise(s) {
		return ""
	}
	// Claude Code wraps pasted text in <pasted_content id="…"> …
	// </pasted_content id="…">; the markers are not part of the prompt.
	s = strings.TrimSpace(pastedMarker.ReplaceAllString(s, ""))
	// First line, collapsed.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The transcript may contain prompts that pasted in API keys / tokens
	// when the user was debugging an integration; mask before persisting
	// the result as a session title.
	return redact.String(strings.TrimSpace(s))
}
