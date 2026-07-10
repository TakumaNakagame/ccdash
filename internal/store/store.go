// Package store is the seam between the TUI (and the non-TUI CLI
// subcommands) and where session data actually lives. Local wraps *db.DB
// plus direct file reads plus internal/summarize — this is ccdash's
// original, same-host behavior. Remote talks the same shape of operations
// to a ccdash collector's HTTP API (internal/server) so the TUI can run on a
// different machine than the collector ("remote mode").
//
// Every method takes context.Context first and mirrors an existing *db.DB
// method 1:1 where one exists, so Local is mostly a thin pass-through and
// the diff to internal/tui stays mechanical (m.db.X → m.store.X).
package store

import (
	"context"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// TailResult is the parsed live-tail plus the metadata the TUI needs to
// decide whether to re-render — mirroring what the TUI used to get from an
// os.Stat + transcript.LoadTail pair directly (tui.go, pre-Store).
type TailResult struct {
	Messages []transcript.Message
	Mtime    time.Time
	Size     int64
}

// Store is implemented by Local (internal/store/local.go) and Remote
// (internal/store/remote.go). See the package doc for the split.
type Store interface {
	ListSessions(ctx context.Context, archived bool) ([]model.Session, error)
	ListPendingApprovals(ctx context.Context) ([]model.Approval, error)

	SetArchived(ctx context.Context, sessionID string, v bool) error
	SetFavorite(ctx context.Context, sessionID string, v bool) error
	SetCustomTitle(ctx context.Context, sessionID, title string) error
	SetUserGroup(ctx context.Context, sessionID, group string) error

	// DecideApproval sends the operator's allow/deny choice for a pending
	// PermissionRequest. keep=true on an allow asks Claude to remember the
	// rule for the rest of the session. This always goes over HTTP — even
	// Local — because the pending hold lives in the collector process's
	// memory (server.Server.pending), not in SQLite.
	DecideApproval(ctx context.Context, id int64, behavior, reason string, keep bool) error

	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	// AllSettings returns every known setting in one call. settings.Load
	// goes through this instead of per-key GetSetting so remote-mode TUI
	// startup costs one HTTP round trip, not one per key.
	AllSettings(ctx context.Context) (map[string]string, error)

	// Summarize kicks off the claude -p summary flow for sessionID and
	// returns as soon as the "running" status is recorded — it does not
	// block for the summary itself. The result (summary text / status)
	// lands in the session row and is picked up on the next ListSessions
	// poll, same as every other background update.
	Summarize(ctx context.Context, sessionID string) error

	// TranscriptStat reports the transcript's mtime/size cheaply, without
	// parsing it — the "did anything change since I last tailed this"
	// check the TUI runs on every tick before paying for a re-parse.
	TranscriptStat(ctx context.Context, s model.Session) (mtime time.Time, size int64, err error)
	// TranscriptTail returns the parsed trailing budgetBytes of the
	// transcript, for the always-on right-pane live view.
	TranscriptTail(ctx context.Context, s model.Session, budgetBytes int) (TailResult, error)
	// TranscriptFull returns the whole parsed transcript, for the
	// full-screen modal viewer ('o').
	TranscriptFull(ctx context.Context, s model.Session) ([]transcript.Message, error)
}
