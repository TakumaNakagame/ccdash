// Package store is the seam between the TUI (and the non-TUI CLI
// subcommands) and where session data actually lives. Local wraps *db.DB
// plus direct file reads plus internal/summarize (titles) — this is ccdash's
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
	"github.com/takumanakagame/ccmanage/internal/usage"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tasks"
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

// StartRequest is a fresh claude session for the collector to spawn in a
// hosted PTY (POST /pty/start) — what the TUI's `n` does.
type StartRequest struct {
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt,omitempty"`
	// Project puts the new session into this project once its session ID
	// is known.
	Project string `json:"project,omitempty"`
	// PermissionMode is "", manual, acceptEdits, auto or plan.
	PermissionMode string `json:"permissionMode,omitempty"`
}

// Input is what POST /pty/{key}/input types into a hosted claude: Keys
// raw, then Text (bracketed paste when multi-line or Paste), then Enter
// when Submit.
type Input struct {
	Keys   string `json:"keys,omitempty"`
	Text   string `json:"text,omitempty"`
	Submit bool   `json:"submit,omitempty"`
	Paste  bool   `json:"paste,omitempty"`
}

// Store is implemented by Local (internal/store/local.go) and Remote
// (internal/store/remote.go). See the package doc for the split.
type Store interface {
	ListSessions(ctx context.Context, archived bool) ([]model.Session, error)
	ListPendingApprovals(ctx context.Context) ([]model.Approval, error)

	SetArchived(ctx context.Context, sessionID string, v bool) error
	SetFavorite(ctx context.Context, sessionID string, v bool) error
	// SetWait records what a session declared it waits for (MCP
	// set_status: model.WaitAnswer …); "" clears it.
	SetWait(ctx context.Context, sessionID, kind, note string) error
	// SetLater marks / clears "watch later".
	SetLater(ctx context.Context, sessionID string, v bool) error
	// SetGrid puts a session on the grid shared with the portal.
	SetGrid(ctx context.Context, sessionID string, v bool) error
	// SetColor sets the session's color ("#rrggbb"; "" = back to automatic).
	SetColor(ctx context.Context, sessionID, color string) error
	// MarkSeen records that the operator looked at the session (clears an
	// unread "done" attention mark).
	MarkSeen(ctx context.Context, sessionID string) error
	// UsageSummary totals tokens / API-price estimate over the last days.
	UsageSummary(ctx context.Context, days int) (usage.Summary, error)
	SetCustomTitle(ctx context.Context, sessionID, title string) error
	SetUserGroup(ctx context.Context, sessionID, group string) error
	// SetProject puts the session into a project ("" takes it out); a new
	// name creates the project with its own color.
	SetProject(ctx context.Context, sessionID, project string) error
	// ListProjects returns every project (picker candidates), including
	// ones whose sessions are all archived.
	ListProjects(ctx context.Context) ([]model.Project, error)
	// RenameProject renames a project, merging it into an existing one of
	// the new name.
	RenameProject(ctx context.Context, from, to string) error
	// MoveProject moves a project delta places in the list order
	// (negative = toward the newest end); SetProjectOrder puts the named
	// projects first in that order.
	MoveProject(ctx context.Context, project string, delta int) error
	SetProjectOrder(ctx context.Context, names []string) error
	// SetProjectColor sets a project's color ("#rrggbb", or "random").
	SetProjectColor(ctx context.Context, project, color string) error

	// DecideApproval sends the operator's allow/deny choice for a pending
	// PermissionRequest. keep=true on an allow asks Claude to remember the
	// rule for the rest of the session. This always goes over HTTP — even
	// Local — because the pending hold lives in the collector process's
	// memory (server.Server.pending), not in SQLite.
	DecideApproval(ctx context.Context, id int64, behavior, reason string, keep bool) error

	// StartSession asks the collector to spawn claude in a hosted PTY and
	// returns its PTY key ("pid-<N>" until the session ID is known). Like
	// DecideApproval this always goes over HTTP: the PTY lives in the
	// collector process.
	StartSession(ctx context.Context, req StartRequest) (string, error)
	// SendInput types into the hosted PTY ptyKey (POST /pty/{key}/input),
	// over HTTP for the same reason as StartSession.
	SendInput(ctx context.Context, ptyKey string, in Input) error
	// WaitingSpawns lists hosted claudes that have no session yet and wait
	// on a dialog (the folder-trust question in a new folder) — from the
	// collector's GET /pty, so also over HTTP.
	WaitingSpawns(ctx context.Context) ([]WaitingSpawn, error)

	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	// AllSettings returns every known setting in one call. settings.Load
	// goes through this instead of per-key GetSetting so remote-mode TUI
	// startup costs one HTTP round trip, not one per key.
	AllSettings(ctx context.Context) (map[string]string, error)

	// GenerateTitles kicks off claude -p title generation for up to
	// summarize.MaxTitleBatch sessions in one batch (ctrl+t). It returns
	// once title_status is "running"; the titles land
	// in the rows (gen_title) and show up on a later ListSessions poll.
	GenerateTitles(ctx context.Context, sessionIDs []string) error

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

	// The to-do list (docs/decisions/0006-tasks.md). Tasks are named by
	// id ("11" / "#11") or key.
	//
	// ListTasks returns the list bucketed and ordered for display
	// (archived tasks only when asked) with the change counter.
	ListTasks(ctx context.Context, includeArchived bool) (model.TaskList, error)
	GetTask(ctx context.Context, ident string) (model.Task, error)
	AddTask(ctx context.Context, in tasks.Input) (model.Task, error)
	UpdateTask(ctx context.Context, ident string, p tasks.Patch) (model.Task, error)
	AddTaskNote(ctx context.Context, ident string, n model.TaskNote) (model.Task, error)
	// OrderTasks places the given tasks first, in that order.
	OrderTasks(ctx context.Context, ids []int64) error
	// SyncTasks merges an automated run's proposal (tasks.Merge rules).
	SyncTasks(ctx context.Context, p tasks.Proposal) (tasks.SyncResult, error)
	// TaskContext is the open tasks and their links for an automated run.
	TaskContext(ctx context.Context) (tasks.Context, error)
	// ImportTasks brings in a work-status tasks.json (dryRun = plan only).
	ImportTasks(ctx context.Context, f tasks.LegacyFile, opt tasks.ImportOptions, dryRun bool) (tasks.ImportPlan, error)
}

// WaitingSpawn is a hosted claude stuck on a dialog before it has a
// session: its PTY key, start directory and the dialog's question.
type WaitingSpawn struct {
	Key    string `json:"pty"`
	Cwd    string `json:"cwd,omitempty"`
	Prompt string `json:"waiting_on"`
}

// ptyRow is one row of the collector's GET /pty.
type ptyRow struct {
	Key    string `json:"key"`
	Alive  bool   `json:"alive"`
	PID    int    `json:"pid"`
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt"`
}

// waitingOf keeps the pid-* spawns with a prompt whose PTY has no session
// key yet.
func waitingOf(rows []ptyRow) []WaitingSpawn {
	named := map[int]bool{}
	for _, r := range rows {
		if !strings.HasPrefix(r.Key, "pid-") {
			named[r.PID] = true
		}
	}
	out := []WaitingSpawn{}
	for _, r := range rows {
		if r.Alive && r.Prompt != "" && strings.HasPrefix(r.Key, "pid-") && !named[r.PID] {
			out = append(out, WaitingSpawn{Key: r.Key, Cwd: r.Cwd, Prompt: r.Prompt})
		}
	}
	return out
}
