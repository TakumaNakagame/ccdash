// Package mcpserver is `ccdash mcp`: a Model Context Protocol server on
// stdio that lets Claude see what ccdash sees — projects, sessions, what
// needs the operator, a session's recent conversation — and organize it:
// put sessions into projects, create / rename / merge projects, recolor
// them, retitle, pin, recolor and archive sessions, put them on the grid,
// mark them seen — and start a new claude session in a
// directory (start_session, like the TUI's `n`). It goes through
// store.Store, so it works against the local DB or a remote collector (-r)
// alike. It never stops claude and never answers approvals; --read-only
// drops the organizing tools and start_session.
//
// The protocol subset is small enough to speak by hand: newline-delimited
// JSON-RPC 2.0 with initialize, ping, tools/list and tools/call.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/store"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// protocolVersion is what we answer when the client asks for a version we
// don't know; known ones are echoed back.
const protocolVersion = "2025-06-18"

var knownVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

const instructions = `ccdash watches the Claude Code sessions of one machine (its collector).
Use the read tools to see the operator's projects, which sessions are running, which
need the operator (approvals, questions), and what a session has been doing recently.
Sessions are referred to as "#N" (their short number) or by session id.
The organizing tools (set_project, rename_project, order_projects, set_project_color, set_title, set_later,
set_grid, set_pinned, set_color, mark_seen, set_archived, archive_finished) change what the operator sees in ccdash;
projects are just names, created on first use.
start_session opens a new claude session in a directory (optionally in a project, with a
first prompt); the operator sees and drives it in ccdash.
When this machine is joined to a ccdash hub, each session carries "url": a link that opens
that session's chat in the hub portal (browser or installed app). Put it next to a session
whenever you list sessions for the operator, e.g. the ones that need them.

Status rule — follow it in every session that has these tools: the operator juggles many
sessions and can't tell which ones wait on them. Right before you end a turn (the message
that hands control back to the operator), call set_status once with what you hand back:
  answer      you asked the operator a question or for a decision and can't go on without it
  action      the operator has to do something themselves first (log in, run or approve
              something outside this session, check a device, merge, restart…)
  report      you are done; the final message reports results and nothing is needed
  background  you are waiting on something else (CI, a build, a background task, a timer)
              and will carry on by yourself
with note = one short line in the operator's language saying what you need, or what you
report ("PR #12 のマージ待ち", "テスト全部通過、デプロイ済み"). Declare it every turn, even a short
reply; a new turn forgets the last one. Don't call it mid-turn while you keep working.`

// Server serves one stdio connection.
type Server struct {
	st       store.Store
	version  string
	readOnly bool
	hubURL   string // hub origin for session links; "" = not joined, no links
	now      func() time.Time
	self     func() string // the calling claude's session id ("" = unknown); see selfSession
}

// WithHubURL makes every session row carry a link to its chat in that hub's
// portal (<hub>/s/<session id>; the hub finds the device).
func (s *Server) WithHubURL(u string) *Server {
	s.hubURL = strings.TrimRight(u, "/")
	return s
}

// New returns a server over st; readOnly leaves out the organizing tools.
func New(st store.Store, version string, readOnly bool) *Server {
	return &Server{st: st, version: version, readOnly: readOnly, now: time.Now, self: selfSession}
}

func (s *Server) tools() []map[string]any {
	// set_status touches only the caller's own session, so --read-only
	// keeps it: the status rule holds in every session.
	out := append(append([]map[string]any{}, readTools...), statusTool)
	if s.readOnly {
		return out
	}
	return append(out, writeTools...)
}

var statusTool = map[string]any{
	"name": "set_status",
	"description": "Tell the operator what this session hands back at the end of the turn — call it once, right before your final message. " +
		"answer = you wait for their answer / decision; action = they must do something themselves first; " +
		"report = done, the message just reports; background = you wait on something else (CI, a build, a task) and continue by yourself. " +
		"note: one short line in the operator's language (what you need / what you report). The status shows in ccdash (TUI, portal, app) and answer / action mark the session \"needs you\".",
	"inputSchema": required(map[string]any{
		"status":  map[string]any{"type": "string", "enum": model.WaitKinds},
		"note":    map[string]any{"type": "string", "description": "one short line: what you need from the operator, or what you report"},
		"session": map[string]any{"type": "string", "description": "only if the call fails to find your session: your session id"},
	}, "status", "note"),
	"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from r until EOF and writes responses to w.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}); err != nil {
				return err
			}
			continue
		}
		result, rerr := s.handle(ctx, req)
		if len(req.ID) == 0 { // a notification: no reply
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
		if rerr == nil && result == nil {
			resp.Result = struct{}{}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocolVersion
		if knownVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ccdash", "version": s.version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		out, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			// Tool failures are results the model can read, not protocol errors.
			return map[string]any{"isError": true, "content": []any{text(err.Error())}}, nil
		}
		return map[string]any{"content": []any{text(out)}}, nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func schema(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}

var readOnlyHint = map[string]any{"readOnlyHint": true}

var writeHint = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true}

var sessionList = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `sessions as "#N", N or session id`}

func required(props map[string]any, req ...string) map[string]any {
	m := schema(props)
	m["required"] = req
	return m
}

var writeTools = []map[string]any{
	{
		"name":        "start_session",
		"description": "Start a new claude session in a directory, hosted by the ccdash collector (what `n` does in the TUI). The session shows up in ccdash, live in its right pane, where the operator drives it. Optionally put it into a project and give it a first prompt.",
		"inputSchema": required(map[string]any{
			"cwd":             map[string]any{"type": "string", "description": "absolute path of an existing directory on the collector's machine"},
			"project":         map[string]any{"type": "string", "description": "put the new session into this project (created on first use)"},
			"prompt":          map[string]any{"type": "string", "description": "first message to send claude; empty = just open it"},
			"permission_mode": map[string]any{"type": "string", "enum": []string{"manual", "acceptEdits", "auto", "plan"}, "description": "claude --permission-mode; default = claude's own"},
		}, "cwd"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
	},
	{
		"name":        "set_project",
		"description": "Put sessions into a project (a new name creates the project with its own color), or take them out with an empty project.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"project":  map[string]any{"type": "string", "description": `project name; "" removes the sessions from their project`},
		}, "sessions", "project"),
		"annotations": writeHint,
	},
	{
		"name":        "rename_project",
		"description": "Rename a project. Renaming it to the name of another existing project merges the two (the sessions move over, the target keeps its color).",
		"inputSchema": required(map[string]any{
			"from": map[string]any{"type": "string"},
			"to":   map[string]any{"type": "string"},
		}, "from", "to"),
		"annotations": writeHint,
	},
	{
		"name":        "order_projects",
		"description": "Set the order projects appear in (first = top of the list). Listed projects go first in the given order; the rest follow in their current order.",
		"inputSchema": required(map[string]any{
			"projects": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, "projects"),
		"annotations": writeHint,
	},
	{
		"name":        "set_project_color",
		"description": `Set a project's color: "#rrggbb", or "random" for one that keeps apart from the other projects' colors.`,
		"inputSchema": required(map[string]any{
			"project": map[string]any{"type": "string"},
			"color":   map[string]any{"type": "string"},
		}, "project", "color"),
		"annotations": writeHint,
	},
	{
		"name":        "set_title",
		"description": "Rename a session as shown in ccdash (an empty title goes back to the automatic one).",
		"inputSchema": required(map[string]any{
			"session": map[string]any{"type": "string", "description": `"#N", N or session id`},
			"title":   map[string]any{"type": "string"},
		}, "session", "title"),
		"annotations": writeHint,
	},
	{
		"name":        "set_later",
		"description": "Mark sessions \"watch later\" (gathered at the top of the list until cleared) or clear the mark.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"later":    map[string]any{"type": "boolean"},
		}, "sessions", "later"),
		"annotations": writeHint,
	},
	{
		"name":        "set_grid",
		"description": "Put sessions on the grid (the tiled view shared by the TUI's V and the portal's grid; tiles go in the order they were put there) or take them off.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"grid":     map[string]any{"type": "boolean"},
		}, "sessions", "grid"),
		"annotations": writeHint,
	},
	{
		"name":        "set_pinned",
		"description": "Pin sessions (kept at the top of their list / project block, most recently pinned first; never auto-archived) or unpin them.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"pinned":   map[string]any{"type": "boolean"},
		}, "sessions", "pinned"),
		"annotations": writeHint,
	},
	{
		"name":        "set_color",
		"description": `Set sessions' color bar: "#rrggbb", "random" for one that keeps apart from the running sessions' colors, or "" for the automatic one.`,
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"color":    map[string]any{"type": "string"},
		}, "sessions", "color"),
		"annotations": writeHint,
	},
	{
		"name":        "mark_seen",
		"description": "Mark sessions as seen by the operator: clears NEW and the finished-unseen (done) mark, as opening them in ccdash does. Approvals, questions and declared answer / action waits stay.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
		}, "sessions"),
		"annotations": writeHint,
	},
	{
		"name":        "set_archived",
		"description": "Archive sessions (hide them from the working list) or bring them back.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"archived": map[string]any{"type": "boolean"},
		}, "sessions", "archived"),
		"annotations": writeHint,
	},
	{
		"name": "archive_finished",
		"description": "Tidy up: archive the sessions whose claude has exited (never a running one — it never stops claude). " +
			"Skips pinned and watch-later sessions and ones the operator hasn't looked at yet (NEW / finished unseen / needs you) unless include_unread. " +
			"Narrow it with sessions, project or idle_hours; dry_run lists what it would archive.",
		"inputSchema": schema(map[string]any{
			"sessions":       sessionList,
			"project":        map[string]any{"type": "string", "description": "only sessions in this project"},
			"idle_hours":     map[string]any{"type": "number", "description": "only sessions with no activity for this many hours (default 0 = any)"},
			"include_unread": map[string]any{"type": "boolean", "description": "also archive finished sessions the operator hasn't looked at"},
			"dry_run":        map[string]any{"type": "boolean", "description": "list the sessions, archive nothing"},
		}),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
}

var readTools = []map[string]any{
	{
		"name":        "overview",
		"description": "What's going on right now: sessions that need the operator (approvals, questions), finished turns nobody has looked at, running sessions grouped by project, and today's API-price usage estimate.",
		"inputSchema": schema(map[string]any{}),
		"annotations": readOnlyHint,
	},
	{
		"name":        "list_projects",
		"description": "The operator's projects (named sets of sessions) in their list order, with session counts, running / needs-you counts and latest activity.",
		"inputSchema": schema(map[string]any{}),
		"annotations": readOnlyHint,
	},
	{
		"name":        "list_sessions",
		"description": "Sessions, newest first. Filter by project, by text (title, path, repo, #N), to running ones, to ones that need attention, or to the grid (in tile order).",
		"inputSchema": schema(map[string]any{
			"project":          map[string]any{"type": "string", "description": "only sessions in this project"},
			"query":            map[string]any{"type": "string", "description": "case-insensitive text to match in title / cwd / repo / branch / group / #N"},
			"running_only":     map[string]any{"type": "boolean", "description": "only sessions whose claude is running (active or idle)"},
			"attention_only":   map[string]any{"type": "boolean", "description": "only sessions that need the operator or finished unseen"},
			"grid_only":        map[string]any{"type": "boolean", "description": "only sessions on the grid, in tile order"},
			"include_archived": map[string]any{"type": "boolean", "description": "also search archived sessions"},
			"limit":            map[string]any{"type": "integer", "description": "max rows (default 30, max 200)"},
		}),
		"annotations": readOnlyHint,
	},
	{
		"name":        "get_session",
		"description": "One session's details plus its recent conversation (user prompts, Claude's replies, tool calls) and any pending approvals.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":  map[string]any{"type": "string", "description": `"#N", N, or the session id`},
				"messages": map[string]any{"type": "integer", "description": "how many recent transcript entries (default 30, max 200)"},
			},
			"required":             []string{"session"},
			"additionalProperties": false,
		},
		"annotations": readOnlyHint,
	},
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var args struct {
		Project         string   `json:"project"`
		Query           string   `json:"query"`
		RunningOnly     bool     `json:"running_only"`
		AttentionOnly   bool     `json:"attention_only"`
		GridOnly        bool     `json:"grid_only"`
		IncludeArchived bool     `json:"include_archived"`
		Limit           int      `json:"limit"`
		Session         string   `json:"session"`
		Messages        int      `json:"messages"`
		Sessions        []string `json:"sessions"`
		From            string   `json:"from"`
		To              string   `json:"to"`
		Color           string   `json:"color"`
		Title           *string  `json:"title"`
		Projects        []string `json:"projects"`
		Archived        *bool    `json:"archived"`
		Later           *bool    `json:"later"`
		Grid            *bool    `json:"grid"`
		Pinned          *bool    `json:"pinned"`
		IdleHours       float64  `json:"idle_hours"`
		IncludeUnread   bool     `json:"include_unread"`
		DryRun          bool     `json:"dry_run"`
		Cwd             string   `json:"cwd"`
		Prompt          string   `json:"prompt"`
		PermissionMode  string   `json:"permission_mode"`
		Status          string   `json:"status"`
		Note            string   `json:"note"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
	}
	var v any
	var err error
	if s.readOnly {
		for _, t := range writeTools {
			if t["name"] == name {
				return "", fmt.Errorf("%s is not available: ccdash mcp runs with --read-only", name)
			}
		}
	}
	switch name {
	case "set_status":
		v, err = s.setStatus(ctx, args.Status, args.Note, args.Session)
	case "start_session":
		v, err = s.startSession(ctx, args.Cwd, args.Project, args.Prompt, args.PermissionMode)
	case "set_project":
		v, err = s.setProject(ctx, args.Sessions, args.Project)
	case "rename_project":
		v, err = s.renameProject(ctx, args.From, args.To)
	case "order_projects":
		if len(args.Projects) == 0 {
			return "", fmt.Errorf("projects is required")
		}
		if err = s.st.SetProjectOrder(ctx, args.Projects); err == nil {
			v, err = s.listProjects(ctx)
		}
	case "set_project_color":
		v, err = s.setProjectColor(ctx, args.Project, args.Color)
	case "set_title":
		if args.Title == nil {
			return "", fmt.Errorf("title is required")
		}
		v, err = s.setTitle(ctx, args.Session, *args.Title)
	case "set_later":
		v, err = s.setFlag(ctx, args.Sessions, "later", "watch_later", args.Later, s.st.SetLater)
	case "set_grid":
		v, err = s.setFlag(ctx, args.Sessions, "grid", "on_grid", args.Grid, s.st.SetGrid)
	case "set_pinned":
		v, err = s.setFlag(ctx, args.Sessions, "pinned", "pinned", args.Pinned, s.st.SetFavorite)
	case "set_color":
		v, err = s.setColor(ctx, args.Sessions, args.Color)
	case "mark_seen":
		var ss []model.Session
		if ss, err = s.resolveAll(ctx, args.Sessions); err == nil {
			for _, x := range ss {
				if err = s.st.MarkSeen(ctx, x.SessionID); err != nil {
					break
				}
			}
			v = map[string]any{"seen": refsOf(ss)}
		}
	case "set_archived":
		if args.Archived == nil {
			return "", fmt.Errorf("archived is required")
		}
		v, err = s.setArchived(ctx, args.Sessions, *args.Archived)
	case "archive_finished":
		v, err = s.archiveFinished(ctx, args.Sessions, args.Project, args.IdleHours, args.IncludeUnread, args.DryRun)
	case "overview":
		v, err = s.overview(ctx)
	case "list_projects":
		v, err = s.listProjects(ctx)
	case "list_sessions":
		v, err = s.listSessions(ctx, args.Project, args.Query, args.RunningOnly, args.AttentionOnly, args.GridOnly, args.IncludeArchived, args.Limit)
	case "get_session":
		v, err = s.getSession(ctx, args.Session, args.Messages)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(v, "", " ")
	return string(b), err
}

// sessionRow is the compact view of a session the tools return.
type sessionRow struct {
	Ref       string `json:"ref,omitempty"`
	Title     string `json:"title"`
	Project   string `json:"project,omitempty"`
	Group     string `json:"group,omitempty"`
	Status    string `json:"status"`
	Attention string `json:"attention,omitempty"`
	Reason    string `json:"attention_reason,omitempty"`
	Pending   int    `json:"pending_approvals,omitempty"`
	Cwd       string `json:"cwd"`
	Branch    string `json:"branch,omitempty"`
	Account   string `json:"account,omitempty"`
	LastSeen  string `json:"last_seen"`
	Ago       string `json:"last_seen_ago"`
	Pinned    bool   `json:"pinned,omitempty"`
	New       bool   `json:"new_since_seen,omitempty"` // progress since the operator last looked
	Later     bool   `json:"watch_later,omitempty"`
	Grid      bool   `json:"on_grid,omitempty"`         // on the grid shared by the TUI (V) and the portal
	Wait      string `json:"declared_status,omitempty"` // what the session said it hands back (set_status)
	WaitNote  string `json:"declared_note,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
	SessionID string `json:"session_id"`
	URL       string `json:"url,omitempty"` // the session's chat in the hub portal
}

func (s *Server) row(x model.Session) sessionRow {
	r := sessionRow{
		Ref: x.Ref(), Title: x.DisplayTitle(), Project: x.Project, Group: groupOf(x),
		Status: string(x.Status), Attention: x.Attention, Reason: x.AttentionReason, Pending: x.PendingCount,
		Cwd: x.Cwd, Branch: x.Branch, Pinned: x.Favorite, New: x.Unseen, Later: x.Later, Grid: x.Grid, Wait: x.Wait, WaitNote: x.WaitNote, Archived: x.Archived, SessionID: x.SessionID,
	}
	if s.hubURL != "" {
		r.URL = s.hubURL + "/s/" + url.PathEscape(x.SessionID)
	}
	if x.Account != "default" {
		r.Account = x.Account
	}
	if !x.LastSeen.IsZero() {
		r.LastSeen = x.LastSeen.Local().Format(time.RFC3339)
		r.Ago = ago(s.now().Sub(x.LastSeen))
	}
	if r.Title == "" {
		r.Title = "(no prompt yet)"
	}
	return r
}

// groupOf mirrors the TUI's tab grouping.
func groupOf(x model.Session) string {
	if x.UserGroup != "" {
		return x.UserGroup
	}
	if x.Repo != "" {
		return x.Repo
	}
	if x.Cwd == "" {
		return ""
	}
	parts := strings.Split(strings.TrimRight(x.Cwd, "/"), "/")
	return parts[len(parts)-1]
}

func running(x model.Session) bool {
	return x.Status == model.StatusActive || x.Status == model.StatusIdle
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func (s *Server) overview(ctx context.Context) (any, error) {
	ss, err := s.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	var needs, unseen []sessionRow
	byProject := map[string][]sessionRow{}
	total, run := len(ss), 0
	for _, x := range ss {
		switch x.Attention {
		case model.AttentionNeedsYou:
			needs = append(needs, s.row(x))
		case model.AttentionDone:
			unseen = append(unseen, s.row(x))
		}
		if running(x) {
			run++
			byProject[x.Project] = append(byProject[x.Project], s.row(x))
		}
	}
	out := map[string]any{
		"sessions_in_list":        total,
		"running":                 run,
		"needs_you":               needs,
		"finished_unseen":         unseen,
		"running_by_project":      byProject,
		"running_by_project_note": `the "" key holds running sessions that are in no project`,
	}
	if ws, err := s.st.WaitingSpawns(ctx); err == nil && len(ws) > 0 {
		out["new_sessions_waiting"] = ws
		out["new_sessions_waiting_note"] = "claudes started by ccdash that stopped at a dialog (e.g. Claude Code's folder-trust question) before they have a session; the operator answers it in ccdash (TUI row / portal 起動中)"
	}
	if u, err := s.st.UsageSummary(ctx, 1); err == nil {
		out["today_usage_estimate"] = map[string]any{
			"usd_at_api_list_price": fmt.Sprintf("%.2f", u.Today.Cost),
			"input_tokens":          u.Today.Input,
			"output_tokens":         u.Today.Output,
		}
	}
	return out, nil
}

type projectRow struct {
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	Sessions int    `json:"sessions"`
	Running  int    `json:"running"`
	NeedsYou int    `json:"needs_you"`
	Latest   string `json:"latest_activity,omitempty"`
	latest   time.Time
}

func (s *Server) listProjects(ctx context.Context) (any, error) {
	ps, err := s.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := s.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	rows := map[string]*projectRow{}
	var order []string
	for _, p := range ps {
		rows[p.Name] = &projectRow{Name: p.Name, Color: model.ProjectColorOf(p.Name, p.Color)}
		order = append(order, p.Name)
	}
	for _, x := range ss {
		if x.Project == "" {
			continue
		}
		r := rows[x.Project]
		if r == nil {
			r = &projectRow{Name: x.Project, Color: model.ProjectColorOf(x.Project, x.ProjectColor)}
			rows[x.Project] = r
			order = append(order, x.Project)
		}
		r.Sessions++
		if running(x) {
			r.Running++
		}
		if x.Attention == model.AttentionNeedsYou {
			r.NeedsYou++
		}
		if x.LastSeen.After(r.latest) {
			r.latest = x.LastSeen
		}
	}
	out := make([]projectRow, 0, len(order))
	for _, n := range order {
		r := rows[n]
		if !r.latest.IsZero() {
			r.Latest = r.latest.Local().Format(time.RFC3339) + " (" + ago(s.now().Sub(r.latest)) + ")"
		}
		out = append(out, *r)
	}
	return map[string]any{"projects": out}, nil
}

func (s *Server) listSessions(ctx context.Context, project, query string, runningOnly, attentionOnly, gridOnly, archived bool, limit int) (any, error) {
	ss, err := s.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	if archived {
		more, err := s.st.ListSessions(ctx, true)
		if err != nil {
			return nil, err
		}
		ss = append(ss, more...)
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].LastSeen.After(ss[j].LastSeen) })
	if gridOnly { // tile order, as the TUI and the portal lay them out
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].GridAt.Before(ss[j].GridAt) })
	}
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 200)
	q := strings.ToLower(strings.TrimSpace(query))
	var out []sessionRow
	matched := 0
	for _, x := range ss {
		if project != "" && x.Project != project ||
			runningOnly && !running(x) ||
			attentionOnly && x.Attention == "" ||
			gridOnly && !x.Grid ||
			q != "" && !matches(x, q) {
			continue
		}
		matched++
		if len(out) < limit {
			out = append(out, s.row(x))
		}
	}
	return map[string]any{"matched": matched, "sessions": out}, nil
}

func matches(x model.Session, q string) bool {
	if ref := x.Ref(); ref != "" && (q == strings.ToLower(ref) || q == ref[1:]) {
		return true
	}
	for _, f := range []string{x.DisplayTitle(), x.Project, x.UserGroup, x.Repo, x.Cwd, x.Branch, x.SessionID} {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// findSession resolves "#N", "N" or a session id (archived ones too).
func (s *Server) findSession(ctx context.Context, ref string) (model.Session, error) {
	ref = strings.TrimSpace(ref)
	num, numErr := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
	for _, archived := range []bool{false, true} {
		ss, err := s.st.ListSessions(ctx, archived)
		if err != nil {
			return model.Session{}, err
		}
		for _, x := range ss {
			if x.SessionID == ref || numErr == nil && x.Num == num {
				return x, nil
			}
		}
	}
	return model.Session{}, fmt.Errorf("no session %q (use #N or a session id; list_sessions shows them)", ref)
}

type entry struct {
	At   string `json:"at,omitempty"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (s *Server) getSession(ctx context.Context, ref string, n int) (any, error) {
	if ref == "" {
		return nil, fmt.Errorf("session is required")
	}
	x, err := s.findSession(ctx, ref)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 30
	}
	n = min(n, 200)
	out := map[string]any{"session": s.row(x)}
	if aps, err := s.st.ListPendingApprovals(ctx); err == nil {
		var mine []map[string]any
		for _, a := range aps {
			if a.SessionID == x.SessionID {
				mine = append(mine, map[string]any{"tool": a.Tool, "input": clip(string(a.ToolInput), 400), "since": a.Timestamp.Local().Format(time.RFC3339)})
			}
		}
		if len(mine) > 0 {
			out["pending_approvals"] = mine
		}
	}
	if x.TranscriptPath == "" {
		out["transcript"] = "no transcript recorded yet"
		return out, nil
	}
	tail, err := s.st.TranscriptTail(ctx, x, 512<<10)
	if err != nil {
		out["transcript_error"] = err.Error()
		return out, nil
	}
	var es []entry
	for _, m := range tail.Messages {
		e := entry{Kind: string(m.Kind)}
		if !m.Timestamp.IsZero() {
			e.At = m.Timestamp.Local().Format("01-02 15:04")
		}
		switch m.Kind {
		case transcript.KindUser, transcript.KindAssistant:
			e.Text = clip(m.Text, 2000)
		case transcript.KindToolUse:
			e.Text = m.Tool + " " + clip(m.ToolInput, 300)
		case transcript.KindToolResult:
			if !m.IsError {
				continue // results are bulky; the call line says what ran
			}
			e.Kind = "tool_error"
			e.Text = clip(m.Text, 300)
		default:
			continue // thinking / system
		}
		if strings.TrimSpace(e.Text) == "" {
			continue
		}
		es = append(es, e)
	}
	if len(es) > n {
		es = es[len(es)-n:]
	}
	out["recent"] = es
	return out, nil
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// resolveAll turns refs into sessions, failing on the first unknown one so
// nothing is half-applied.
func (s *Server) resolveAll(ctx context.Context, refs []string) ([]model.Session, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("sessions is required")
	}
	out := make([]model.Session, 0, len(refs))
	for _, r := range refs {
		x, err := s.findSession(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func refsOf(ss []model.Session) []string {
	out := make([]string, len(ss))
	for i, x := range ss {
		out[i] = x.Ref()
		if out[i] == "" {
			out[i] = x.SessionID
		}
	}
	return out
}

func (s *Server) setProject(ctx context.Context, refs []string, project string) (any, error) {
	project = strings.TrimSpace(project)
	ss, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, x := range ss {
		if err := s.st.SetProject(ctx, x.SessionID, project); err != nil {
			return nil, err
		}
	}
	if project == "" {
		return map[string]any{"removed_from_project": refsOf(ss)}, nil
	}
	return map[string]any{"project": project, "sessions": refsOf(ss)}, nil
}

func (s *Server) renameProject(ctx context.Context, from, to string) (any, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to are required")
	}
	ps, err := s.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	found, merge := false, false
	for _, p := range ps {
		found = found || p.Name == from
		merge = merge || p.Name == to && to != from
	}
	if !found {
		return nil, fmt.Errorf("no project %q (list_projects shows them)", from)
	}
	if err := s.st.RenameProject(ctx, from, to); err != nil {
		return nil, err
	}
	if merge {
		return map[string]any{"merged": from, "into": to}, nil
	}
	return map[string]any{"renamed": from, "to": to}, nil
}

func (s *Server) setProjectColor(ctx context.Context, project, color string) (any, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if color != "random" && !model.ValidColor(color) {
		return nil, fmt.Errorf(`color must be "#rrggbb" or "random"`)
	}
	if err := s.st.SetProjectColor(ctx, project, color); err != nil {
		return nil, err
	}
	return map[string]any{"project": project, "color": color}, nil
}

func (s *Server) setTitle(ctx context.Context, ref, title string) (any, error) {
	x, err := s.findSession(ctx, ref)
	if err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if err := s.st.SetCustomTitle(ctx, x.SessionID, title); err != nil {
		return nil, err
	}
	return map[string]any{"session": refsOf([]model.Session{x})[0], "title": title}, nil
}

// setFlag applies one boolean store setter to every session in refs; arg
// is the tool argument's name, key the result's.
func (s *Server) setFlag(ctx context.Context, refs []string, arg, key string, v *bool, set func(context.Context, string, bool) error) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("%s is required", arg)
	}
	ss, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, x := range ss {
		if err := set(ctx, x.SessionID, *v); err != nil {
			return nil, err
		}
	}
	return map[string]any{key: *v, "sessions": refsOf(ss)}, nil
}

func (s *Server) setColor(ctx context.Context, refs []string, color string) (any, error) {
	color = strings.TrimSpace(color)
	if color != "" && color != "random" && !model.ValidColor(color) {
		return nil, fmt.Errorf(`color must be "#rrggbb", "random" or ""`)
	}
	ss, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, x := range ss {
		if err := s.st.SetColor(ctx, x.SessionID, color); err != nil {
			return nil, err
		}
	}
	if color == "" {
		color = "automatic"
	}
	return map[string]any{"color": color, "sessions": refsOf(ss)}, nil
}

func (s *Server) setArchived(ctx context.Context, refs []string, archived bool) (any, error) {
	ss, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, x := range ss {
		if err := s.st.SetArchived(ctx, x.SessionID, archived); err != nil {
			return nil, err
		}
	}
	return map[string]any{"archived": archived, "sessions": refsOf(ss)}, nil
}

// archiveFinished archives exited sessions (see the tool description);
// running ones are reported as skipped, never stopped.
func (s *Server) archiveFinished(ctx context.Context, refs []string, project string, idleHours float64, unread, dryRun bool) (any, error) {
	var ss []model.Session
	var err error
	if len(refs) > 0 {
		ss, err = s.resolveAll(ctx, refs)
	} else {
		ss, err = s.st.ListSessions(ctx, false)
	}
	if err != nil {
		return nil, err
	}
	project = strings.TrimSpace(project)
	cutoff := s.now().Add(-time.Duration(idleHours * float64(time.Hour)))
	var picked []model.Session
	skipped := map[string][]string{}
	skip := func(why string, x model.Session) { skipped[why] = append(skipped[why], refsOf([]model.Session{x})[0]) }
	for _, x := range ss {
		switch {
		case x.Archived, project != "" && x.Project != project:
			continue
		case running(x):
			skip("running", x)
		case x.Favorite:
			skip("pinned", x)
		case x.Later:
			skip("watch_later", x)
		case !unread && (x.Unseen || x.Attention != "" || x.PendingCount > 0):
			skip("unread_or_needs_you", x)
		case idleHours > 0 && x.LastSeen.After(cutoff):
			skip("recent_activity", x)
		default:
			picked = append(picked, x)
		}
	}
	if !dryRun {
		for _, x := range picked {
			if err := s.st.SetArchived(ctx, x.SessionID, true); err != nil {
				return nil, err
			}
		}
	}
	rows := make([]sessionRow, len(picked))
	for i, x := range picked {
		rows[i] = s.row(x)
	}
	out := map[string]any{"dry_run": dryRun, "archived": len(picked), "sessions": rows}
	if dryRun {
		out["archived_note"] = "nothing archived (dry_run); these would be"
	}
	if len(skipped) > 0 {
		out["skipped"] = skipped
	}
	return out, nil
}

// spawnWait is how long start_session watches a new spawn for a dialog.
var spawnWait = 6 * time.Second

// waitFor returns the dialog the spawn key waits on, watching it for up to
// spawnWait; "" if none showed up.
func (s *Server) waitFor(ctx context.Context, key string) string {
	deadline := s.now().Add(spawnWait)
	for {
		if ws, err := s.st.WaitingSpawns(ctx); err == nil {
			for _, w := range ws {
				if w.Key == key {
					return w.Prompt
				}
			}
		}
		if !s.now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) startSession(ctx context.Context, cwd, project, prompt, mode string) (any, error) {
	cwd = strings.TrimSpace(cwd)
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("cwd must be an absolute path, got %q", cwd)
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "-") {
		return nil, fmt.Errorf("prompt must not start with '-'")
	}
	// The same switch that keeps the TUI from spawning claude.
	cfg, err := settings.Load(ctx, s.st)
	if err != nil {
		return nil, err
	}
	if !cfg.AttachEnabled {
		return nil, fmt.Errorf("starting sessions is OFF in ccdash (settings: Attach)")
	}
	project = strings.TrimSpace(project)
	key, err := s.st.StartSession(ctx, store.StartRequest{Cwd: filepath.Clean(cwd), Prompt: prompt, Project: project, PermissionMode: mode})
	if err != nil {
		return nil, fmt.Errorf("start session (is the ccdash collector running?): %w", err)
	}
	out := map[string]any{
		"started": key,
		"cwd":     filepath.Clean(cwd),
		"note":    "the session appears in ccdash once claude starts (a few seconds); open it there to drive it",
	}
	// A new folder can stop claude at the folder-trust question before it
	// registers anything; say so instead of leaving the caller guessing.
	if w := s.waitFor(ctx, key); w != "" {
		out["waiting_on"] = w
		out["note"] = "claude stopped at a dialog before starting the session; the operator must answer it in ccdash (TUI row / portal 起動中). Folders under ccdash's 'Trusted folders' setting skip the trust question."
	}
	if project != "" {
		out["project"] = project
	}
	return out, nil
}
