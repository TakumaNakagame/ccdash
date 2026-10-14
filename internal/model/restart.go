package model

import "time"

// RestartState is a collector's scheduled restart (GET /api/restart): a
// restart asked for "when idle" waits until no hosted claude is busy, then
// restarts the collector and resumes every hosted session. The TUI, the
// portal, the Android app and MCP all draw their warning from it; the hub
// carries it per device (/api/devices → restart).
type RestartState struct {
	Pending  bool      `json:"pending"`
	Since    time.Time `json:"since,omitzero"`
	Deadline time.Time `json:"deadline,omitzero"`
	// By says where it was asked from ("portal", "tui", "cli", "mcp").
	By string `json:"by,omitempty"`
	// Waiting lists the hosted sessions the restart waits for.
	Waiting []RestartBlocker `json:"waiting"`
	// Hosted is how many sessions the restart stops and resumes;
	// Unregistered the hosted claudes with no session yet, which it drops.
	Hosted       int `json:"hosted"`
	Unregistered int `json:"unregistered,omitempty"`
	// Last / LastAt: how the previous scheduled restart ended without
	// running ("cancelled", "expired"), kept for a while for the UIs.
	Last       string    `json:"last,omitempty"`
	LastAt     time.Time `json:"last_at,omitzero"`
	CanRestart bool      `json:"can_restart"`
}

// RestartBlocker is one hosted session a scheduled restart waits for.
type RestartBlocker struct {
	SessionID string `json:"session_id"`
	Num       int64  `json:"num,omitempty"`
	Title     string `json:"title,omitempty"`
	// Why: 作業中 / 承認待ち / 質問: … / 確認: … / バックグラウンド待ち: …
	Why string `json:"why"`
}

// RestartWhenIdle / RestartNow are the "when" of POST /api/restart.
const (
	RestartWhenIdle = "idle"
	RestartNow      = "now"
)
