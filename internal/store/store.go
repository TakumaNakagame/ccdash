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
	"fmt"
	"github.com/TakumaNakagame/ccdash/internal/usage"
	"strings"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/tasks"
	"github.com/TakumaNakagame/ccdash/internal/transcript"
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
	// Origin marks who started it for whom (the --origin of ccdash handoff-mcp, e.g. "grok");
	// the collector stores it with the session (model.Session.Origin).
	Origin string `json:"origin,omitempty"`
	// SessionID + ResumeID (both the session's id) resume an existing
	// session (`claude --resume`) under that key instead of starting a new
	// one; Cwd must then be the session's own directory.
	SessionID string `json:"sessionId,omitempty"`
	ResumeID  string `json:"resumeId,omitempty"`
	// Parent is the session that starts this one (ccdash mcp's
	// start_session: its caller), kept on the new session's row
	// (model.Session.Parent). ParentHost names the parent's machine when
	// the new session runs on another one.
	Parent     string `json:"parent,omitempty"`
	ParentHost string `json:"parentHost,omitempty"`
	// Model / Effort become claude's --model / --effort (an alias such as
	// "sonnet" or a model id; low, medium, high, xhigh or max). Empty = the
	// collector's default_model / default_effort settings for a new
	// session, nothing for a resume.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Orchestrated marks a start by ccdash mcp's start_session — the only
	// caller that sets it. The collector then takes an empty Cwd from the
	// project's directory, appends the operator's notes (the common brief
	// and the project's) to Prompt unless Plain, and runs the
	// session_setup_command setting before claude
	// (internal/server/setup.go).
	Orchestrated bool `json:"orchestrated,omitempty"`
	Plain        bool `json:"plain,omitempty"`
	// Role starts the new session as an orchestrator
	// (model.RoleOrchestrator, or RoleProjectOrchestrator for Project).
	// For people's starts only (the TUI, the CLI, the portal): the
	// collector refuses it together with Orchestrated and from a peer.
	Role string `json:"role,omitempty"`
}

// Started is what the collector answers to a start.
type Started struct {
	// PtyKey is "pid-<N>" until the session ID is known.
	PtyKey string `json:"ptyKey"`
	// Orchestrated echoes StartRequest.Orchestrated from a collector that
	// honours it; false from an older one, which appended nothing. Notes
	// then names what went after the prompt ("common", "project") and Cwd
	// where claude started.
	Orchestrated bool     `json:"orchestrated,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
}

// StartInfo is what a collector can fill in for a start that names no
// directory (GET /api/start-info): the project's working directory, and
// whether a setup command runs first (it may name one).
type StartInfo struct {
	Dir   string `json:"dir"`
	Setup bool   `json:"setup"`
}

// orchestratedTimeout bounds a start that may run the operator's setup
// command first (the collector gives that 60 s).
const orchestratedTimeout = 75 * time.Second

// Input is what POST /pty/{key}/input types into a hosted claude: Keys
// raw, then Text (bracketed paste when multi-line or Paste), then Enter
// when Submit.
type Input struct {
	Keys   string `json:"keys,omitempty"`
	Text   string `json:"text,omitempty"`
	Submit bool   `json:"submit,omitempty"`
	Paste  bool   `json:"paste,omitempty"`
}

// Tell is a message from one session to another — its parent or its child
// (POST /api/sessions/{id}/message). FromSession is the sending session's
// id and FromRef its ref on its own machine; the receiving collector
// checks the two are parent and child and writes the header line that
// names the sender itself (the sending machine is the one the hub
// authenticated, never a field of this).
type Tell struct {
	Text        string `json:"text"`
	FromSession string `json:"from_session"`
	FromRef     string `json:"from_ref,omitempty"`
}

// Told is the receiving collector's answer: State is model.MessageTyped
// ("delivered": typed into the running claude), model.MessageResumed (the
// stopped session was resumed with it) or model.MessageQueued (kept in the
// collector's inbox; Note says what it waits for).
type Told struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// tellTimeout bounds a tell: the collector may ask the sender's machine
// through the hub who the sender is, and types or starts claude before it
// answers.
const tellTimeout = 20 * time.Second

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
	StartSession(ctx context.Context, req StartRequest) (Started, error)
	// StartInfo asks the collector what it can fill in for a start with
	// no directory. Over HTTP even for Local: it is also the check that
	// the running collector is new enough to take such a start (an older
	// one has no such route and would start claude in its own directory).
	StartInfo(ctx context.Context, project string) (StartInfo, error)
	// BriefCommon / SetBriefCommon / SetProjectBrief / SetProjectDir read
	// and write the operator's notes for start_session briefs and a
	// project's working directory. For people's tools only (`ccdash
	// brief`, the portal has its own routes): nothing in ccdash mcp may
	// call the setters — the notes are input to every child session.
	BriefCommon(ctx context.Context) (string, error)
	SetBriefCommon(ctx context.Context, text string) error
	SetProjectBrief(ctx context.Context, project, brief string) error
	SetProjectDir(ctx context.Context, project, dir string) (string, error)
	// SetRole gives a session an orchestrator role, takes it away (""),
	// or — with from — hands the role and the children of that session
	// over to it. SetProjectOrchestrator sets the model / effort a
	// project's orchestrator runs with. For people's tools only (`ccdash
	// role`; the portal has its own routes): nothing in ccdash mcp may
	// call them — a session must not be able to make an orchestrator.
	SetRole(ctx context.Context, sessionID, role, from string) (model.RoleChange, error)
	SetProjectOrchestrator(ctx context.Context, project, model, effort string) error
	// SendInput types into the hosted PTY ptyKey (POST /pty/{key}/input),
	// over HTTP for the same reason as StartSession.
	SendInput(ctx context.Context, ptyKey string, in Input) error
	// WaitingSpawns lists hosted claudes that have no session yet and wait
	// on a dialog (the folder-trust question in a new folder) — from the
	// collector's GET /pty, so also over HTTP.
	WaitingSpawns(ctx context.Context) ([]WaitingSpawn, error)
	// PTYs lists the collector's hosted PTYs (GET /pty), one row per key:
	// a spawn is "pid-<N>" until its session id is known, then also listed
	// under that id (same PID).
	PTYs(ctx context.Context) ([]PTY, error)
	// HostedSessions lists the session IDs whose claude runs in a live PTY
	// of the collector (GET /pty) — the ones StopSession can stop.
	HostedSessions(ctx context.Context) ([]string, error)
	// StopSession ends the hosted claude in PTY key (DELETE /pty/{key}:
	// SIGTERM, then a kill after a grace period). The transcript stays, so
	// StartSession with ResumeID brings it back.
	StopSession(ctx context.Context, key string) error
	// RestartStatus / ScheduleRestart / CancelRestart drive the collector's
	// scheduled restart (GET / POST {"when":"idle"} / DELETE /api/restart):
	// it waits until no hosted claude is busy, then restarts and resumes
	// them. by names the asker ("tui", "cli", "mcp"); timeout 0 = default.
	RestartStatus(ctx context.Context) (model.RestartState, error)
	ScheduleRestart(ctx context.Context, by string, timeout time.Duration) (model.RestartState, error)
	CancelRestart(ctx context.Context) (model.RestartState, error)
	// LoginStatus is the machine's Claude Code login (GET /api/login):
	// whether it stopped working and which sessions stopped on it.
	LoginStatus(ctx context.Context) (model.LoginState, error)

	// SetParent makes parent the session's parent, overwriting the one it
	// had (adopt_session); "" clears it (release_session). parentHost
	// names the parent's machine when it isn't the session's own.
	SetParent(ctx context.Context, sessionID, parent, parentHost string) error
	// SetChildrenSeen stamps when a session last listed its children
	// (my_sessions; model.Session.ChildrenSeenAt).
	SetChildrenSeen(ctx context.Context, sessionID string, at time.Time) error
	// Briefs returns what the session was asked to do when ccdash started
	// it (the kept /pty/start prompts), oldest first.
	Briefs(ctx context.Context, sessionID string) ([]model.Brief, error)
	// TellSession hands the collector a message for one of its sessions
	// from that session's parent or child (ccdash mcp's tell_session). The
	// collector delivers it when the session can take it — never onto an
	// approval or a dialog — and keeps it queued until then. Over HTTP
	// even for Local: the inbox and the PTYs live in the collector.
	TellSession(ctx context.Context, sessionID string, t Tell) (Told, error)
	// Peers maps the session id of each claude running on the collector's
	// machine to the name Claude Code's SendMessage reaches it by. Read
	// from the live ~/.claude/sessions files, never stored.
	Peers(ctx context.Context) (map[string]string, error)

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
	// ImportTasks brings in a tasks.json (dryRun = plan only).
	ImportTasks(ctx context.Context, f tasks.LegacyFile, opt tasks.ImportOptions, dryRun bool) (tasks.ImportPlan, error)
}

// WaitingSpawn is a hosted claude stuck on a dialog before it has a
// session: its PTY key, start directory and the dialog's question.
type WaitingSpawn struct {
	Key    string `json:"pty"`
	Cwd    string `json:"cwd,omitempty"`
	Prompt string `json:"waiting_on"`
}

// PTY is one row of the collector's GET /pty. Prompt is the dialog a spawn
// with no session yet is stuck on; Parent the session that started it.
type PTY struct {
	Key    string `json:"key"`
	Alive  bool   `json:"alive"`
	PID    int    `json:"pid"`
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt"`
	Parent string `json:"parent,omitempty"`
}

type ptyRow = PTY

// restartBody is POST /api/restart's body for a scheduled restart.
func restartBody(by string, timeout time.Duration) map[string]any {
	return map[string]any{"when": model.RestartWhenIdle, "by": by, "timeout_min": int(timeout.Minutes())}
}

// errNoScheduledRestart: the collector predates scheduled restarts (or is
// unreachable), so POST /api/restart would restart it right away.
func errNoScheduledRestart(err error) error {
	return fmt.Errorf("this collector can't schedule a restart (older than v0.9.60, or not running): %w", err)
}

// hostedOf keeps the session-ID keys of the alive PTYs.
func hostedOf(rows []ptyRow) []string {
	out := []string{}
	for _, r := range rows {
		if r.Alive && !strings.HasPrefix(r.Key, "pid-") {
			out = append(out, r.Key)
		}
	}
	return out
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
