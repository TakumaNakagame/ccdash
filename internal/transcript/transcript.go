// Package transcript parses the Claude Code session transcript at
// ~/.claude/projects/<encoded-cwd>/<session_id>.jsonl into a slice of
// renderable Message values. We only surface the entries a human cares about
// when reading a session: prompts, assistant replies, tool calls, results.
package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type Kind string

const (
	KindUser      Kind = "user"
	KindAssistant Kind = "assistant"
	KindThinking  Kind = "thinking"
	KindToolUse   Kind = "tool_use"
	KindToolResult Kind = "tool_result"
	KindSystem    Kind = "system"
)

type Message struct {
	Kind      Kind
	Timestamp time.Time
	Text      string // user prompt / assistant text / thinking / system content
	Tool      string // name for KindToolUse
	ToolUseID string // ties tool_use ↔ tool_result
	ToolInput string // pretty-ish single-line for KindToolUse
	IsError   bool   // for KindToolResult
}

// Load reads the entire transcript file and returns parsed messages in
// chronological order.
func Load(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		out = append(out, parseLine(line)...)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	return out, nil
}

func parseLine(line []byte) []Message {
	var head struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		Content   string          `json:"content"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return nil
	}
	ts := parseTime(head.Timestamp)

	switch head.Type {
	case "user":
		return parseUserMessage(head.Message, ts)
	case "assistant":
		return parseAssistantMessage(head.Message, ts)
	case "system":
		// Most system entries are noisy (file-history-snapshot, bridge_status).
		// Surface only the ones that read like real status updates.
		if head.Subtype == "" || head.Subtype == "info" || head.Subtype == "compact" {
			if s := strings.TrimSpace(head.Content); s != "" {
				return []Message{{Kind: KindSystem, Timestamp: ts, Text: s}}
			}
		}
	}
	return nil
}

func parseUserMessage(raw json.RawMessage, ts time.Time) []Message {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if len(m.Content) == 0 {
		return nil
	}
	// Plain string => typed prompt.
	if m.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			s = strings.TrimSpace(s)
			if s == "" || strings.HasPrefix(s, "<command-") {
				return nil
			}
			return []Message{{Kind: KindUser, Timestamp: ts, Text: s}}
		}
		return nil
	}
	// Array => tool_result blocks.
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var out []Message
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		out = append(out, Message{
			Kind:      KindToolResult,
			Timestamp: ts,
			Text:      flattenContent(b.Content),
			ToolUseID: b.ToolUseID,
			IsError:   b.IsError,
		})
	}
	return out
}

func parseAssistantMessage(raw json.RawMessage, ts time.Time) []Message {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var out []Message
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			t := strings.TrimSpace(c.Text)
			if t == "" {
				continue
			}
			out = append(out, Message{Kind: KindAssistant, Timestamp: ts, Text: t})
		case "thinking":
			t := strings.TrimSpace(c.Thinking)
			if t == "" {
				continue
			}
			out = append(out, Message{Kind: KindThinking, Timestamp: ts, Text: t})
		case "tool_use":
			out = append(out, Message{
				Kind:      KindToolUse,
				Timestamp: ts,
				Tool:      c.Name,
				ToolUseID: c.ID,
				ToolInput: summarizeToolInput(c.Name, c.Input),
			})
		}
	}
	return out
}

// flattenContent turns a tool_result content (which can be a string or an
// array of {type: "text", text: ...} blocks) into a single readable string.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for i, blk := range blocks {
			if blk.Type == "text" {
				if i > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func summarizeToolInput(tool string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	switch tool {
	case "Bash":
		return pick("command")
	case "Edit", "Write", "Read":
		return pick("file_path")
	case "Glob", "Grep":
		return pick("pattern", "query")
	case "WebFetch", "WebSearch":
		return pick("url", "query")
	}
	return pick("file_path", "command", "url", "query", "pattern")
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
