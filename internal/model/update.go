package model

import "time"

// UpdateState is a collector's own ccdash version and its update
// (GET /api/update; internal/server/update.go). The hub carries it per
// device (/api/devices → update) for the portal's and the app's Update
// button.
type UpdateState struct {
	// Current is the running collector's version; Channel the release
	// channel it follows (the update_channel setting).
	Current string `json:"current"`
	Channel string `json:"channel"`
	// Latest is the newest release in Channel as last seen (checked at
	// most every updateCheckEvery); Available when it is newer than
	// Current.
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	CheckErr  string    `json:"check_err,omitempty"`
	// Auto is the auto_update setting: "off", "stable" or "dev".
	Auto string `json:"auto"`
	// Running: an update is downloading now.
	Running bool `json:"running,omitempty"`
	// Installed is a version already written to disk that the running
	// collector hasn't restarted into yet; RestartNeeded when it can't
	// restart itself (a TUI's or `ccdash -k`'s collector) — the operator
	// restarts it from where it runs.
	Installed     string `json:"installed,omitempty"`
	RestartNeeded bool   `json:"restart_needed,omitempty"`
	// Last is how the latest update attempt ended.
	Last *UpdateResult `json:"last,omitempty"`
}

// UpdateResult is one update attempt.
type UpdateResult struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to,omitempty"`
	// By: "portal" (the Update button), "auto" (the nightly update), "cli".
	By string `json:"by"`
	// OK, or Error (redacted, short).
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
