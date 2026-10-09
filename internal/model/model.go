package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type SessionStatus string

const (
	StatusActive  SessionStatus = "active"  // Claude is currently processing
	StatusIdle    SessionStatus = "idle"    // Claude is alive, awaiting input
	StatusRecent  SessionStatus = "recent"  // Claude exited within ~6h — easy to resume
	StatusStopped SessionStatus = "stopped" // Claude exited long ago
)

type Session struct {
	SessionID      string        `json:"session_id"`
	Cwd            string        `json:"cwd"`
	Repo           string        `json:"repo,omitempty"`
	Branch         string        `json:"branch,omitempty"`
	Commit         string        `json:"commit,omitempty"`
	WrapperPID     int           `json:"wrapper_pid,omitempty"`
	ProcPID        int           `json:"proc_pid,omitempty"`
	Pane           string        `json:"pane,omitempty"`
	TmuxPane       string        `json:"tmux_pane,omitempty"`
	TmuxSession    string        `json:"tmux_session,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	Model          string        `json:"model,omitempty"`
	Num            int64         `json:"num,omitempty"`       // short sequential ID shown as "#N"; unique, assigned on insert
	Title          string        `json:"title,omitempty"`     // auto-derived from transcript (first prompt)
	GenTitle       string        `json:"gen_title,omitempty"` // claude -p generated title (ctrl+t); beats Title
	GenTitleAt     time.Time     `json:"gen_title_at,omitempty"`
	TitleStatus    string        `json:"title_status,omitempty"` // "", "running", "done", "error" for title generation
	CustomTitle    string        `json:"custom_title,omitempty"` // operator override; takes precedence
	UserGroup      string        `json:"user_group,omitempty"`   // operator-named group; overrides repo-based grouping. Rendered as a tab in the strip.
	Account        string        `json:"account,omitempty"`      // account name from accounts.json (e.g. "personal", "enterprise")
	Archived       bool          `json:"archived,omitempty"`
	Favorite       bool          `json:"favorite,omitempty"`
	ColorOverride  string        `json:"color,omitempty"`         // operator-picked "#rrggbb"; see Color
	Project        string        `json:"project,omitempty"`       // operator-named project; groups sessions at the top of the list
	ProjectColor   string        `json:"project_color,omitempty"` // the project's "#rrggbb" (projects table)
	FirstSeen      time.Time     `json:"first_seen"`
	LastSeen       time.Time     `json:"last_seen"`
	Status         SessionStatus `json:"status"`
	PendingCount   int           `json:"pending_count,omitempty"`
	// Attention is what the session wants from the operator: "needs_you"
	// (an approval, a question, a menu on screen) or "done" (finished a turn
	// the operator hasn't looked at yet); "" otherwise. See internal/server
	// attention.go for who sets and clears it.
	Attention       string    `json:"attention,omitempty"`
	AttentionReason string    `json:"attention_reason,omitempty"`
	AttentionAt     time.Time `json:"attention_at,omitzero"`
}

// Project is an operator-named set of sessions (projects table).
type Project struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	// Sessions counts the project's sessions in the working set
	// (not archived).
	Sessions int `json:"sessions"`
}

const (
	AttentionNeedsYou = "needs_you"
	AttentionDone     = "done"
)

// DisplayTitle returns the operator-set title when present, then the
// generated one (ctrl+t), then the auto-derived first prompt.
func (s Session) DisplayTitle() string {
	if s.CustomTitle != "" {
		return s.CustomTitle
	}
	if s.GenTitle != "" {
		return s.GenTitle
	}
	return s.Title
}

// Ref is the short handle shown next to the title, e.g. "#42". Empty until
// the row has a number (a TUI placeholder row for a fresh spawn has none).
func (s Session) Ref() string {
	if s.Num <= 0 {
		return ""
	}
	return fmt.Sprintf("#%d", s.Num)
}

// SessionPalette is the set of session colors: distinct hues (plus light
// variants) that read on dark and light backgrounds. The portal mirrors it
// (SESSION_PALETTE in internal/hub/web/app.js) — change both together.
var SessionPalette = []string{
	"#ef4444", "#f97316", "#f59e0b", "#facc15", "#84cc16", "#22c55e",
	"#10b981", "#14b8a6", "#06b6d4", "#0ea5e9", "#3b82f6", "#6366f1",
	"#8b5cf6", "#a855f7", "#d946ef", "#ec4899", "#f43f5e", "#b45309",
	"#94a3b8", "#fca5a5", "#fde68a", "#86efac", "#93c5fd", "#c4b5fd",
}

// ValidColor reports whether c can be stored as a session color: a
// "#rrggbb" hex (any, not only the palette — the palette is what the UIs
// offer).
func ValidColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// colorStride steps the automatic color by #N: coprime with the palette
// size, so 24 consecutive sessions get 24 different colors, and neighbours
// land far apart on the hue wheel.
const colorStride = 7

// Color is the session's accent color (hex): the operator's pick when set,
// else one from SessionPalette by the session's #N (so recent sessions
// differ), or by an FNV-1a hash of the ID for a row without a number yet.
// Empty for a row without an ID. The portal computes the same
// (sessionColorOf in internal/hub/web/app.js).
func (s Session) Color() string {
	if s.ColorOverride != "" {
		return s.ColorOverride
	}
	n := uint32(len(SessionPalette))
	if s.Num > 0 {
		return SessionPalette[uint32(s.Num*colorStride)%n]
	}
	if s.SessionID == "" {
		return ""
	}
	h := uint32(2166136261)
	for i := 0; i < len(s.SessionID); i++ {
		h ^= uint32(s.SessionID[i])
		h *= 16777619
	}
	return SessionPalette[h%n]
}

type EventType string

const (
	EventSessionStart      EventType = "session_start"
	EventSessionEnd        EventType = "session_end"
	EventUserPrompt        EventType = "user_prompt"
	EventPreTool           EventType = "pre_tool"
	EventPostTool          EventType = "post_tool"
	EventPostToolFailure   EventType = "post_tool_failure"
	EventPermissionRequest EventType = "permission_request"
	EventStop              EventType = "stop"
	EventNotification      EventType = "notification"
	EventSubagentStop      EventType = "subagent_stop"
)

type Event struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	Timestamp time.Time       `json:"timestamp"`
	EventType EventType       `json:"event_type"`
	Tool      string          `json:"tool,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalTimeout  ApprovalStatus = "timeout"
	// ApprovalResolved is set once we observe the matching PostToolUse event,
	// indicating the tool actually ran. We can't tell from a hook whether it
	// was approved by user or by an existing allow rule — but either way it
	// no longer needs the operator's attention.
	ApprovalResolved ApprovalStatus = "resolved"
	// ApprovalFailed indicates the tool ran but failed (PostToolUseFailure).
	ApprovalFailed ApprovalStatus = "failed"
)

type Approval struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	Timestamp time.Time       `json:"timestamp"`
	Tool      string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolInput json.RawMessage `json:"tool_input"`
	Status    ApprovalStatus  `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
}

// ProjectColorOf is a project's color: the stored one (projects table) when
// set, else a palette pick by an FNV-1a hash of the name. The portal
// mirrors it (projectColorOf in internal/hub/web/app.js).
func ProjectColorOf(name, stored string) string {
	if stored != "" {
		return stored
	}
	if name == "" {
		return ""
	}
	h := uint32(2166136261)
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return SessionPalette[h%uint32(len(SessionPalette))]
}

// InkOn picks black or white text for a "#rrggbb" background by its
// relative luminance.
func InkOn(bg string) string {
	if !ValidColor(bg) {
		return "#ffffff"
	}
	v, _ := strconv.ParseUint(bg[1:], 16, 32)
	r, g, b := float64(v>>16&0xff), float64(v>>8&0xff), float64(v&0xff)
	if 0.299*r+0.587*g+0.114*b > 150 {
		return "#111111"
	}
	return "#ffffff"
}
