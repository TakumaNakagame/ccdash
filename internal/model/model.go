package model

import (
	"encoding/json"
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
	Title          string        `json:"title,omitempty"`        // auto-derived from transcript
	CustomTitle    string        `json:"custom_title,omitempty"` // operator override; takes precedence
	Archived       bool          `json:"archived,omitempty"`
	Favorite       bool          `json:"favorite,omitempty"`
	FirstSeen      time.Time     `json:"first_seen"`
	LastSeen       time.Time     `json:"last_seen"`
	Status         SessionStatus `json:"status"`
	PendingCount   int           `json:"pending_count,omitempty"`
}

// DisplayTitle returns the operator-set title when present, otherwise the
// auto-derived one from the transcript.
func (s Session) DisplayTitle() string {
	if s.CustomTitle != "" {
		return s.CustomTitle
	}
	return s.Title
}

type EventType string

const (
	EventSessionStart       EventType = "session_start"
	EventSessionEnd         EventType = "session_end"
	EventUserPrompt         EventType = "user_prompt"
	EventPreTool            EventType = "pre_tool"
	EventPostTool           EventType = "post_tool"
	EventPostToolFailure    EventType = "post_tool_failure"
	EventPermissionRequest  EventType = "permission_request"
	EventStop               EventType = "stop"
	EventNotification       EventType = "notification"
	EventSubagentStop       EventType = "subagent_stop"
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
