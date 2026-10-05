package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Subagents (the Agent tool) write their own transcripts next to the
// session's: <transcript without .jsonl>/subagents/agent-<id>.jsonl, plus
// agent-<id>.meta.json naming the type, description and the parent's
// tool_use id. A running subagent is NOT in the parent transcript yet (the
// Agent call lands there when it returns), so the portal reads this
// directory to show what's running.

var agentIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
	Shape       string `json:"requestShape"` // "background" for async agents
}

type subagentInfo struct {
	AgentID     string    `json:"agentId"`
	AgentType   string    `json:"agentType"`
	Description string    `json:"description"`
	ToolUseID   string    `json:"toolUseId,omitempty"`
	Background  bool      `json:"background"`
	Depth       int       `json:"depth"`
	Status      string    `json:"status"` // running | done | stale
	StartedAt   time.Time `json:"startedAt,omitzero"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Activity    string    `json:"activity,omitempty"` // latest tool call / text
	Tools       int       `json:"tools"`              // tool calls seen in the tail
	Report      string    `json:"report,omitempty"`   // the hand-back, once done
}

// subagentStale is when a subagent that never handed back counts as gone
// (killed, crashed, or from a Claude Code version without SubagentHandback).
const subagentStale = 10 * time.Minute

func (s *Server) subagentDir(r *http.Request, sid string) (string, bool) {
	sess, ok, err := s.db.GetSession(r.Context(), sid)
	if err != nil || !ok || sess.TranscriptPath == "" {
		return "", false
	}
	return filepath.Join(strings.TrimSuffix(sess.TranscriptPath, ".jsonl"), "subagents"), true
}

// handleHubSubagents lists a session's subagents, newest first.
// GET /hub/subagents?session=<id>
func (s *Server) handleHubSubagents(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.subagentDir(r, r.URL.Query().Get("session"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeOK(w, []subagentInfo{})
		return
	}
	var out []subagentInfo
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info := subagentInfo{AgentID: id, UpdatedAt: fi.ModTime().UTC()}
		var meta subagentMeta
		if b, err := os.ReadFile(filepath.Join(dir, "agent-"+id+".meta.json")); err == nil && json.Unmarshal(b, &meta) == nil {
			info.AgentType, info.Description, info.ToolUseID = meta.AgentType, meta.Description, meta.ToolUseID
			info.Background, info.Depth = meta.Shape == "background", meta.SpawnDepth
		}
		summarizeSubagent(filepath.Join(dir, name), &info)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > 30 {
		out = out[:30]
	}
	writeOK(w, out)
}

// summarizeSubagent fills status / activity / report from the head and the
// tail of a subagent transcript.
func summarizeSubagent(path string, info *subagentInfo) {
	if f, err := os.Open(path); err == nil {
		var first struct {
			Timestamp time.Time `json:"timestamp"`
		}
		_ = json.NewDecoder(f).Decode(&first)
		f.Close()
		info.StartedAt = first.Timestamp
	}
	data, _, _, err := transcript.TailBytes(path, 96*1024)
	if err != nil {
		return
	}
	done := false
	for _, line := range strings.Split(string(data), "\n") {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "assistant" {
			continue
		}
		var parts []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(e.Message.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					info.Activity = firstLine(t)
				}
			case "tool_use":
				if p.Name == "SubagentHandback" {
					done = true
					var in map[string]any
					_ = json.Unmarshal(p.Input, &in)
					for _, k := range []string{"report", "result", "summary", "message"} {
						if v, ok := in[k].(string); ok && v != "" {
							info.Report = clip(v, 6000)
							break
						}
					}
					continue
				}
				info.Tools++
				info.Activity = p.Name + toolArgPreview(p.Input)
			}
		}
	}
	switch {
	case done:
		info.Status = "done"
	case time.Since(info.UpdatedAt) > subagentStale:
		info.Status = "stale"
	default:
		info.Status = "running"
	}
}

func toolArgPreview(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"} {
		if v, ok := in[k].(string); ok && v != "" {
			return "(" + clip(firstLine(v), 120) + ")"
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 160)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// handleHubSubagentTranscript returns the tail of one subagent's
// transcript, shaped like GET /api/sessions/{id}/transcript?mode=tail.
// GET /hub/subagents/{session}/{agentId}?bytes=N
func (s *Server) handleHubSubagentTranscript(w http.ResponseWriter, r *http.Request, sid, agentID string) {
	if !agentIDPattern.MatchString(agentID) {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	dir, ok := s.subagentDir(r, sid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	budget := int64(256 * 1024)
	if v := r.URL.Query().Get("bytes"); v != "" {
		if n, err := parsePositive(v); err == nil && n <= 8<<20 {
			budget = n
		}
	}
	data, mtime, size, err := transcript.TailBytes(filepath.Join(dir, "agent-"+agentID+".jsonl"), budget)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeOK(w, transcriptEnvelope{
		Mtime: mtime.UTC().Format(time.RFC3339Nano),
		Size:  size,
		Data:  base64.StdEncoding.EncodeToString(data),
	})
}

func parsePositive(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil && n <= 0 {
		err = strconv.ErrRange
	}
	return n, err
}
