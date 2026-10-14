// Package mcpserver is `ccdash mcp`: a Model Context Protocol server on
// stdio that lets Claude see what ccdash sees — projects, sessions, what
// needs the operator, a session's recent conversation — and organize it:
// put sessions into projects, create / rename / merge projects, recolor
// them, retitle, pin, recolor and archive sessions, put them on the grid,
// mark them seen — start a new claude session in a directory
// (start_session, like the TUI's `n`), and stop / resume a session the
// collector hosts (stop_session, resume_session). It goes through
// store.Store, so it works against the local DB or a remote collector (-r)
// alike. It never answers approvals; --read-only drops the organizing tools
// and the session lifecycle ones.
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
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/hubcfg"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/store"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// protocolVersion is what we answer when the client asks for a version we
// don't know; known ones are echoed back.
const protocolVersion = "2025-06-18"

var knownVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

const instructions = `ccdash watches the Claude Code sessions of one machine (its collector) — and,
when the operator has joined several machines to a ccdash hub and allowed it, of the other
machines too ("hosts"; list_hosts names them).
Use the read tools to see the operator's projects, which sessions are running, which
need the operator (approvals, questions), and what a session has been doing recently.
Sessions are referred to by their ref — the host's letter and number, e.g. "A231" (machine A's
session 231, as rows show it in ref) — or by session id. The letter makes a ref unique across
machines; a bare "#N" or N means this machine's session N; pass host for another machine. Read tools without host cover every reachable host (rows carry host);
start_session, schedule_restart and the project tools act on this machine unless host says
otherwise — start a session on another machine with host + a directory that exists there.
The organizing tools (set_project, rename_project, order_projects, set_project_color, set_title, set_later,
set_grid, set_pinned, set_color, mark_seen, set_archived, archive_finished) change what the operator sees in ccdash;
projects are just names, created on first use.
start_session opens a new claude session in a directory (optionally in a project, with a
first prompt); the operator sees and drives it in ccdash.
stop_session ends the claude process of a session ccdash hosts (one at a time; whatever it is
doing is cut off, the transcript stays) and resume_session brings a stopped session back with
claude --resume. Stop a session only when the operator asked for it; never stop your own.
schedule_restart restarts the ccdash collector once no hosted session is busy (each is then
stopped and resumed; nothing mid-turn is cut off); use it only when the operator asked, e.g.
after a ccdash update, and tell them. overview shows a pending one as restart_pending.
When this machine is joined to a ccdash hub (or runs the local portal, ccdash web), each
session carries "url": a link that opens that session's chat in the portal (browser or
installed app). Put it next to a session
whenever you list sessions for the operator, e.g. the ones that need them.

The operator's to-do list lives here too (list_tasks, get_task …). Claude keeps it: add
tasks the operator takes on, record progress with add_task_note when work on a task moves,
change status with set_task_status (with why), and set the day with plan_task when the
operator decides it. Edits from a session link that session to the task. There is no
delete: done / cancelled tasks are archived. Writes to Jira, Slack or GitHub are not done
here — use those services' own tools and keep the links on the task.
Task ids are "#11"; sessions are "A231" (letter + number). A bare "#11" from the operator most
often means a task; if both a task and a session fit and the context doesn't decide, ask.

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
	fleet    *Fleet // the other hosts through the hub; nil = this machine only (hosts.go)
	now      func() time.Time
	self     func() string // the calling claude's session id ("" = unknown); see selfSession
	letter   func() string // this machine's hub letter (hubcfg.Letter); nil with -r
}

// WithHubURL makes every session row carry a link to its chat in that hub's
// portal (<hub>/s/<session id>; the hub finds the device).
func (s *Server) WithHubURL(u string) *Server {
	s.hubURL = strings.TrimRight(u, "/")
	return s
}

// New returns a server over st; readOnly leaves out the organizing tools.
func New(st store.Store, version string, readOnly bool) *Server {
	return &Server{st: st, version: version, readOnly: readOnly, now: time.Now, self: selfSession, letter: hubcfg.Letter}
}

func (s *Server) tools() []map[string]any {
	// set_status touches only the caller's own session, so --read-only
	// keeps it: the status rule holds in every session.
	out := append(append(append([]map[string]any{}, readTools...), taskReadTools...), statusTool)
	if s.readOnly {
		return out
	}
	return append(append(out, writeTools...), taskWriteTools...)
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

var sessionList = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `sessions as refs ("A231"), "#N" / N for this machine, or session ids`}

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
		"name": "stop_session",
		"description": "Stop one session: ends its claude process (SIGTERM, then a kill), which the ccdash collector hosts. " +
			"Side effects: whatever that claude is doing is cut off (a reply mid-turn is lost; tools it started may be left half done), and the operator loses it in their live view until it is resumed. " +
			"The transcript is kept; resume_session brings the session back. Only for sessions started by ccdash (TUI `n`, the portal, start_session or resumed there) — " +
			"a claude started in a terminal can't be stopped from here. Refuses a session that is already stopped, your own session, and (unless force) one that is working mid-turn. Use it only when the operator asked to stop that session.",
		"inputSchema": required(map[string]any{
			"session": map[string]any{"type": "string", "description": `one session: "#N", N or session id`},
			"force":   map[string]any{"type": "boolean", "description": "also stop a session that is working mid-turn (status active)"},
		}, "session"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false},
	},
	{
		"name": "resume_session",
		"description": "Resume a stopped session: starts `claude --resume` on its transcript in its own directory, hosted by the ccdash collector, so it shows up running in ccdash again (live in its right pane; an archived session is unarchived). " +
			"Refuses a session that is still running — under ccdash or in a terminal. Optionally send a first prompt and pick the permission mode.",
		"inputSchema": required(map[string]any{
			"session":         map[string]any{"type": "string", "description": `one session: "#N", N or session id`},
			"prompt":          map[string]any{"type": "string", "description": "message to send once it is back; empty = just open it"},
			"permission_mode": map[string]any{"type": "string", "enum": []string{"manual", "acceptEdits", "auto", "plan"}, "description": "claude --permission-mode; default = claude's own"},
		}, "session"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
	},
	{
		"name": "schedule_restart",
		"description": "Schedule a restart of the ccdash collector (e.g. to load an updated ccdash) for when no session it hosts is busy — working, holding an approval or a question, or waiting on something in the background. " +
			"Then every hosted session is stopped and resumed with claude --resume (conversations are kept; an unregistered fresh claude with no prompt yet is dropped), and ccdash is unreachable for a few seconds. " +
			"Nothing is cut off mid-turn: it keeps waiting while anything is busy, and gives up after timeout_minutes (default 120). The operator sees a warning in ccdash meanwhile and can cancel it. " +
			"Your own session counts as busy until your turn ends, so the restart (and your resume) comes after your final message. cancel: true drops a scheduled restart. Use it only when the operator asked for a restart.",
		"inputSchema": schema(map[string]any{
			"cancel":          map[string]any{"type": "boolean", "description": "cancel the scheduled restart instead"},
			"timeout_minutes": map[string]any{"type": "integer", "description": "give up after this many minutes of waiting (default 120, max 1440)"},
		}),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
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
		"name":        "list_hosts",
		"description": "The machines (ccdash collectors) joined to the operator's ccdash hub that these tools can reach: name, online, which one is this machine. Pass a name as host to the other tools.",
		"inputSchema": schema(map[string]any{}),
		"annotations": readOnlyHint,
	},
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

// Host arguments (see hosts.go). The tools that read lists cover every
// reachable host by default; the ones that act on one host default to this
// machine; the ones that take sessions use host only for a bare "#N".
var (
	hostAll     = map[string]any{"type": "string", "description": `only this host (a name from list_hosts); default = every host this machine can reach`}
	hostHere    = map[string]any{"type": "string", "description": `the host (a name from list_hosts) to act on; default = this machine`}
	hostSession = map[string]any{"type": "string", "description": `the host of a bare "#N" (a name or letter from list_hosts); default = this machine. Refs like "B22" already name their host`}
)

func init() {
	add := func(ts []map[string]any, prop map[string]any, names ...string) {
		for _, t := range ts {
			for _, n := range names {
				if t["name"] == n {
					t["inputSchema"].(map[string]any)["properties"].(map[string]any)["host"] = prop
				}
			}
		}
	}
	add(readTools, hostAll, "overview", "list_projects", "list_sessions")
	add(readTools, hostSession, "get_session")
	add(writeTools, hostHere, "start_session", "schedule_restart", "rename_project", "order_projects", "set_project_color", "archive_finished")
	add(writeTools, hostSession, "stop_session", "resume_session", "set_project", "set_title", "set_later", "set_grid", "set_pinned", "set_color", "mark_seen", "set_archived")
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if v, ok, err := s.callTask(ctx, name, raw); ok {
		if err != nil {
			return "", err
		}
		b, err := json.MarshalIndent(v, "", " ")
		return string(b), err
	}
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
		Force           bool     `json:"force"`
		Cancel          bool     `json:"cancel"`
		Host            string   `json:"host"`
		TimeoutMinutes  int      `json:"timeout_minutes"`
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
		v, err = s.startSession(ctx, args.Host, args.Cwd, args.Project, args.Prompt, args.PermissionMode)
	case "stop_session":
		v, err = s.stopSession(ctx, args.Session, args.Host, args.Force)
	case "resume_session":
		v, err = s.resumeSession(ctx, args.Session, args.Host, args.Prompt, args.PermissionMode)
	case "schedule_restart":
		v, err = s.scheduleRestart(ctx, args.Host, args.Cancel, args.TimeoutMinutes)
	case "set_project":
		v, err = s.setProject(ctx, args.Sessions, args.Host, args.Project)
	case "rename_project":
		v, err = s.renameProject(ctx, args.Host, args.From, args.To)
	case "order_projects":
		v, err = s.orderProjects(ctx, args.Host, args.Projects)
	case "set_project_color":
		v, err = s.setProjectColor(ctx, args.Host, args.Project, args.Color)
	case "set_title":
		if args.Title == nil {
			return "", fmt.Errorf("title is required")
		}
		v, err = s.setTitle(ctx, args.Session, args.Host, *args.Title)
	case "set_later":
		v, err = s.setFlag(ctx, args.Sessions, args.Host, "later", "watch_later", args.Later, func(st store.Store) func(context.Context, string, bool) error { return st.SetLater })
	case "set_grid":
		v, err = s.setFlag(ctx, args.Sessions, args.Host, "grid", "on_grid", args.Grid, func(st store.Store) func(context.Context, string, bool) error { return st.SetGrid })
	case "set_pinned":
		v, err = s.setFlag(ctx, args.Sessions, args.Host, "pinned", "pinned", args.Pinned, func(st store.Store) func(context.Context, string, bool) error { return st.SetFavorite })
	case "set_color":
		v, err = s.setColor(ctx, args.Sessions, args.Host, args.Color)
	case "mark_seen":
		v, err = s.markSeen(ctx, args.Sessions, args.Host)
	case "set_archived":
		if args.Archived == nil {
			return "", fmt.Errorf("archived is required")
		}
		v, err = s.setArchived(ctx, args.Sessions, args.Host, *args.Archived)
	case "archive_finished":
		v, err = s.archiveFinished(ctx, args.Sessions, args.Host, args.Project, args.IdleHours, args.IncludeUnread, args.DryRun)
	case "overview":
		v, err = s.overview(ctx, args.Host)
	case "list_hosts":
		v, err = s.listHosts(ctx)
	case "list_projects":
		v, err = s.listProjects(ctx, args.Host)
	case "list_sessions":
		v, err = s.listSessions(ctx, args.Host, args.Project, args.Query, args.RunningOnly, args.AttentionOnly, args.GridOnly, args.IncludeArchived, args.Limit)
	case "get_session":
		v, err = s.getSession(ctx, args.Session, args.Host, args.Messages)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		if strings.Contains(err.Error(), "not available through the hub") && !strings.Contains(err.Error(), "update ccdash there") {
			err = fmt.Errorf("%w (the other host's ccdash is probably older than this tool — update ccdash there)", err)
		}
		return "", err
	}
	b, err := json.MarshalIndent(v, "", " ")
	return string(b), err
}

// sessionRow is the compact view of a session the tools return.
type sessionRow struct {
	Ref       string  `json:"ref,omitempty"`
	Host      string  `json:"host,omitempty"` // the hub device name of its collector (multi-host)
	HostLetter string `json:"host_letter,omitempty"` // the hub's letter for that host (its sessions are <letter>N)
	Title     string  `json:"title"`
	Project   string  `json:"project,omitempty"`
	Group     string  `json:"group,omitempty"`
	Status    string  `json:"status"`
	Attention string  `json:"attention,omitempty"`
	Reason    string  `json:"attention_reason,omitempty"`
	Pending   int     `json:"pending_approvals,omitempty"`
	Cwd       string  `json:"cwd"`
	Branch    string  `json:"branch,omitempty"`
	Account   string  `json:"account,omitempty"`
	LastSeen  string  `json:"last_seen"`
	Ago       string  `json:"last_seen_ago"`
	Pinned    bool    `json:"pinned,omitempty"`
	New       bool    `json:"new_since_seen,omitempty"` // progress since the operator last looked
	Later     bool    `json:"watch_later,omitempty"`
	Grid      bool    `json:"on_grid,omitempty"`         // on the grid shared by the TUI (V) and the portal
	Wait      string  `json:"declared_status,omitempty"` // what the session said it hands back (set_status)
	WaitNote  string  `json:"declared_note,omitempty"`
	Archived  bool    `json:"archived,omitempty"`
	SessionID string  `json:"session_id"`
	URL       string  `json:"url,omitempty"`   // the session's chat in the hub portal
	Tasks     []int64 `json:"tasks,omitempty"` // open tasks this session worked on
}

func (s *Server) row(l located) sessionRow {
	x := l.x
	r := sessionRow{
		Ref: l.x.Ref(), Host: l.h.name, Title: x.DisplayTitle(), Project: x.Project, Group: groupOf(x),
		Status: string(x.Status), Attention: x.Attention, Reason: x.AttentionReason, Pending: x.PendingCount,
		Cwd: x.Cwd, Branch: x.Branch, Pinned: x.Favorite, New: x.Unseen, Later: x.Later, Grid: x.Grid, Wait: x.Wait, WaitNote: x.WaitNote, Archived: x.Archived, SessionID: x.SessionID,
	}
	if r.Ref != "" {
		r.Ref = l.ref()
	}
	if x.HostLetter == "" && l.h.letter != "" {
		r.HostLetter = l.h.letter
	} else {
		r.HostLetter = x.HostLetter
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

// noteHosts adds what a cross-host read couldn't cover to out.
func noteHosts(out map[string]any, offline []string, errs map[string]string) {
	if len(offline) > 0 {
		out["offline_hosts"] = offline
	}
	if len(errs) > 0 {
		out["unreachable_hosts"] = errs
	}
}

func (s *Server) overview(ctx context.Context, hostArg string) (any, error) {
	hs, offline, err := s.hostsFor(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	ls, errs, err := s.sessionsOn(ctx, hs, false)
	if err != nil {
		return nil, err
	}
	var needs, unseen []sessionRow
	byProject := map[string][]sessionRow{}
	total, run := len(ls), 0
	for _, l := range ls {
		switch l.x.Attention {
		case model.AttentionNeedsYou:
			needs = append(needs, s.row(l))
		case model.AttentionDone:
			unseen = append(unseen, s.row(l))
		}
		if running(l.x) {
			run++
			byProject[l.x.Project] = append(byProject[l.x.Project], s.row(l))
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
	if len(hs) > 1 {
		out["hosts"] = hostNames(hs)
		out["running_by_project_note"] = `the "" key holds running sessions that are in no project; projects belong to one host each (rows carry host)`
	}
	noteHosts(out, offline, errs)
	var waiting []map[string]any
	restarts := map[string]any{}
	usage := map[string]any{}
	for _, h := range hs {
		if ws, err := h.st.WaitingSpawns(ctx); err == nil {
			for _, w := range ws {
				row := map[string]any{"key": w.Key, "cwd": w.Cwd, "prompt": w.Prompt}
				if h.name != "" {
					row["host"] = h.name
				}
				waiting = append(waiting, row)
			}
		}
		if rs, err := h.st.RestartStatus(ctx); err == nil && rs.Pending {
			restarts[hostLabel(h)] = restartView(rs)
		}
		if u, err := h.st.UsageSummary(ctx, 1); err == nil {
			usage[hostLabel(h)] = map[string]any{
				"usd_at_api_list_price": fmt.Sprintf("%.2f", u.Today.Cost),
				"input_tokens":          u.Today.Input,
				"output_tokens":         u.Today.Output,
			}
		}
	}
	if len(waiting) > 0 {
		out["new_sessions_waiting"] = waiting
		out["new_sessions_waiting_note"] = "claudes started by ccdash that stopped at a dialog (e.g. Claude Code's folder-trust question) before they have a session; the operator answers it in ccdash (TUI row / portal 起動中)"
	}
	// One host keeps the single-host shape.
	if len(hs) == 1 {
		for _, v := range restarts {
			out["restart_pending"] = v
		}
		for _, v := range usage {
			out["today_usage_estimate"] = v
		}
	} else {
		if len(restarts) > 0 {
			out["restart_pending"] = restarts
		}
		if len(usage) > 0 {
			out["today_usage_estimate_by_host"] = usage
		}
	}
	return out, nil
}

func hostNames(hs []host) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = hostLabel(h)
		if h.self {
			out[i] += " (this machine)"
		}
	}
	return out
}

type projectRow struct {
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	Color    string `json:"color,omitempty"`
	Sessions int    `json:"sessions"`
	Running  int    `json:"running"`
	NeedsYou int    `json:"needs_you"`
	Latest   string `json:"latest_activity,omitempty"`
	latest   time.Time
}

func (s *Server) listProjects(ctx context.Context, hostArg string) (any, error) {
	hs, offline, err := s.hostsFor(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	var out []projectRow
	errs := map[string]string{}
	for _, h := range hs {
		rows, err := s.projectsOn(ctx, h)
		if err != nil {
			if len(hs) == 1 {
				return nil, h.explain(err)
			}
			errs[hostLabel(h)] = h.explain(err).Error()
			continue
		}
		out = append(out, rows...)
	}
	if out == nil {
		out = []projectRow{}
	}
	res := map[string]any{"projects": out}
	noteHosts(res, offline, errs)
	return res, nil
}

func (s *Server) projectsOn(ctx context.Context, h host) ([]projectRow, error) {
	ps, err := h.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := h.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	rows := map[string]*projectRow{}
	var order []string
	for _, p := range ps {
		rows[p.Name] = &projectRow{Name: p.Name, Host: h.name, Color: model.ProjectColorOf(p.Name, p.Color)}
		order = append(order, p.Name)
	}
	for _, x := range ss {
		if x.Project == "" {
			continue
		}
		r := rows[x.Project]
		if r == nil {
			r = &projectRow{Name: x.Project, Host: h.name, Color: model.ProjectColorOf(x.Project, x.ProjectColor)}
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
	return out, nil
}

func (s *Server) listSessions(ctx context.Context, hostArg, project, query string, runningOnly, attentionOnly, gridOnly, archived bool, limit int) (any, error) {
	hs, offline, err := s.hostsFor(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	ls, errs, err := s.sessionsOn(ctx, hs, archived)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].x.LastSeen.After(ls[j].x.LastSeen) })
	if gridOnly { // tile order, as the TUI and the portal lay them out
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].x.GridAt.Before(ls[j].x.GridAt) })
	}
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 200)
	q := strings.ToLower(strings.TrimSpace(query))
	var out []sessionRow
	matched := 0
	byTask := s.tasksBySession(ctx)
	for _, l := range ls {
		x := l.x
		if project != "" && x.Project != project ||
			runningOnly && !running(x) ||
			attentionOnly && x.Attention == "" ||
			gridOnly && !x.Grid ||
			q != "" && !matches(l, q) {
			continue
		}
		matched++
		if len(out) < limit {
			r := s.row(l)
			if l.h.self {
				r.Tasks = byTask[x.SessionID]
			}
			out = append(out, r)
		}
	}
	res := map[string]any{"matched": matched, "sessions": out}
	if len(hs) > 1 {
		res["hosts"] = hostNames(hs)
	}
	noteHosts(res, offline, errs)
	return res, nil
}

func matches(l located, q string) bool {
	x := l.x
	if ref := l.ref(); ref != "" && (q == strings.ToLower(ref) || model.MatchesRef(q, x.HostLetter, x.Num)) {
		return true
	}
	for _, f := range []string{x.DisplayTitle(), x.Project, x.UserGroup, x.Repo, x.Cwd, x.Branch, x.SessionID, l.h.name} {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// findSession resolves "#N", "N" or a session id (archived ones too) on
// this machine's collector — for the tools that only work here (tasks,
// set_status). The others use locate (hosts.go).
func (s *Server) findSession(ctx context.Context, ref string) (model.Session, error) {
	ref = strings.TrimSpace(ref)
	if l, n, ok := letterRef(ref); ok && strings.EqualFold(l, s.selfLetter()) {
		ref = n
	}
	for _, archived := range []bool{false, true} {
		ss, err := s.st.ListSessions(ctx, archived)
		if err != nil {
			return model.Session{}, err
		}
		if x, ok := findIn(ss, ref); ok {
			return x, nil
		}
	}
	return model.Session{}, fmt.Errorf("no session %q (use #N or a session id; list_sessions shows them)", ref)
}

type entry struct {
	At   string `json:"at,omitempty"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (s *Server) getSession(ctx context.Context, ref, hostArg string, n int) (any, error) {
	if ref == "" {
		return nil, fmt.Errorf("session is required")
	}
	l, err := s.locate(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	x, st := l.x, l.h.st
	if n <= 0 {
		n = 30
	}
	n = min(n, 200)
	row := s.row(l)
	if l.h.self {
		row.Tasks = s.tasksBySession(ctx)[x.SessionID]
	}
	out := map[string]any{"session": row}
	if aps, err := st.ListPendingApprovals(ctx); err == nil {
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
	tail, err := st.TranscriptTail(ctx, x, 512<<10)
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

func (s *Server) setProject(ctx context.Context, refs []string, hostArg, project string) (any, error) {
	project = strings.TrimSpace(project)
	ls, err := s.locateAll(ctx, refs, hostArg)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if err := l.h.st.SetProject(ctx, l.x.SessionID, project); err != nil {
			return nil, err
		}
	}
	if project == "" {
		return map[string]any{"removed_from_project": refsOfLoc(ls)}, nil
	}
	return map[string]any{"project": project, "sessions": refsOfLoc(ls)}, nil
}

func (s *Server) renameProject(ctx context.Context, hostArg, from, to string) (any, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to are required")
	}
	h, err := s.hostNamed(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	ps, err := h.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	found, merge := false, false
	for _, p := range ps {
		found = found || p.Name == from
		merge = merge || p.Name == to && to != from
	}
	if !found {
		return nil, fmt.Errorf("no project %q on %s (list_projects shows them)", from, hostLabel(h))
	}
	if err := h.st.RenameProject(ctx, from, to); err != nil {
		return nil, err
	}
	out := map[string]any{"renamed": from, "to": to}
	if merge {
		out = map[string]any{"merged": from, "into": to}
	}
	if h.name != "" {
		out["host"] = h.name
	}
	return out, nil
}

func (s *Server) setProjectColor(ctx context.Context, hostArg, project, color string) (any, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if color != "random" && !model.ValidColor(color) {
		return nil, fmt.Errorf(`color must be "#rrggbb" or "random"`)
	}
	h, err := s.hostNamed(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	if err := h.st.SetProjectColor(ctx, project, color); err != nil {
		return nil, err
	}
	out := map[string]any{"project": project, "color": color}
	if h.name != "" {
		out["host"] = h.name
	}
	return out, nil
}

func (s *Server) orderProjects(ctx context.Context, hostArg string, names []string) (any, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("projects is required")
	}
	h, err := s.hostNamed(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	if err := h.st.SetProjectOrder(ctx, names); err != nil {
		return nil, err
	}
	rows, err := s.projectsOn(ctx, h)
	if err != nil {
		return nil, err
	}
	return map[string]any{"projects": rows}, nil
}

func (s *Server) setTitle(ctx context.Context, ref, hostArg, title string) (any, error) {
	l, err := s.locate(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if err := l.h.st.SetCustomTitle(ctx, l.x.SessionID, title); err != nil {
		return nil, err
	}
	return map[string]any{"session": l.ref(), "title": title}, nil
}

// setFlag applies one boolean store setter to every session in refs; arg
// is the tool argument's name, key the result's.
func (s *Server) setFlag(ctx context.Context, refs []string, hostArg, arg, key string, v *bool, set func(store.Store) func(context.Context, string, bool) error) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("%s is required", arg)
	}
	ls, err := s.locateAll(ctx, refs, hostArg)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if err := set(l.h.st)(ctx, l.x.SessionID, *v); err != nil {
			return nil, err
		}
	}
	return map[string]any{key: *v, "sessions": refsOfLoc(ls)}, nil
}

func (s *Server) setColor(ctx context.Context, refs []string, hostArg, color string) (any, error) {
	color = strings.TrimSpace(color)
	if color != "" && color != "random" && !model.ValidColor(color) {
		return nil, fmt.Errorf(`color must be "#rrggbb", "random" or ""`)
	}
	ls, err := s.locateAll(ctx, refs, hostArg)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if err := l.h.st.SetColor(ctx, l.x.SessionID, color); err != nil {
			return nil, err
		}
	}
	if color == "" {
		color = "automatic"
	}
	return map[string]any{"color": color, "sessions": refsOfLoc(ls)}, nil
}

func (s *Server) markSeen(ctx context.Context, refs []string, hostArg string) (any, error) {
	ls, err := s.locateAll(ctx, refs, hostArg)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if err := l.h.st.MarkSeen(ctx, l.x.SessionID); err != nil {
			return nil, err
		}
	}
	return map[string]any{"seen": refsOfLoc(ls)}, nil
}

func (s *Server) setArchived(ctx context.Context, refs []string, hostArg string, archived bool) (any, error) {
	ls, err := s.locateAll(ctx, refs, hostArg)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if err := l.h.st.SetArchived(ctx, l.x.SessionID, archived); err != nil {
			return nil, err
		}
	}
	return map[string]any{"archived": archived, "sessions": refsOfLoc(ls)}, nil
}

// archiveFinished archives exited sessions (see the tool description);
// running ones are reported as skipped, never stopped. Without sessions it
// covers one host: host, else this machine.
func (s *Server) archiveFinished(ctx context.Context, refs []string, hostArg, project string, idleHours float64, unread, dryRun bool) (any, error) {
	var ls []located
	var err error
	if len(refs) > 0 {
		ls, err = s.locateAll(ctx, refs, hostArg)
	} else {
		var h host
		if h, err = s.hostNamed(ctx, hostArg); err == nil {
			ls, _, err = s.sessionsOn(ctx, []host{h}, false)
		}
	}
	if err != nil {
		return nil, err
	}
	project = strings.TrimSpace(project)
	cutoff := s.now().Add(-time.Duration(idleHours * float64(time.Hour)))
	var picked []located
	skipped := map[string][]string{}
	skip := func(why string, l located) { skipped[why] = append(skipped[why], l.ref()) }
	for _, l := range ls {
		x := l.x
		switch {
		case x.Archived, project != "" && x.Project != project:
			continue
		case running(x):
			skip("running", l)
		case x.Favorite:
			skip("pinned", l)
		case x.Later:
			skip("watch_later", l)
		case !unread && (x.Unseen || x.Attention != "" || x.PendingCount > 0):
			skip("unread_or_needs_you", l)
		case idleHours > 0 && x.LastSeen.After(cutoff):
			skip("recent_activity", l)
		default:
			picked = append(picked, l)
		}
	}
	if !dryRun {
		for _, l := range picked {
			if err := l.h.st.SetArchived(ctx, l.x.SessionID, true); err != nil {
				return nil, err
			}
		}
	}
	rows := make([]sessionRow, len(picked))
	for i, l := range picked {
		rows[i] = s.row(l)
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
func (s *Server) waitFor(ctx context.Context, st store.Store, key string) string {
	deadline := s.now().Add(spawnWait)
	for {
		if ws, err := st.WaitingSpawns(ctx); err == nil {
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

func (s *Server) startSession(ctx context.Context, hostArg, cwd, project, prompt, mode string) (any, error) {
	cwd = strings.TrimSpace(cwd)
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("cwd must be an absolute path, got %q", cwd)
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "-") {
		return nil, fmt.Errorf("prompt must not start with '-'")
	}
	h, err := s.hostNamed(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	if err := s.attachOn(ctx, h, "starting sessions"); err != nil {
		return nil, err
	}
	project = strings.TrimSpace(project)
	key, err := h.st.StartSession(ctx, store.StartRequest{Cwd: filepath.Clean(cwd), Prompt: prompt, Project: project, PermissionMode: mode})
	if err != nil {
		return nil, fmt.Errorf("start session on %s (is its ccdash collector running?): %w", hostLabel(h), err)
	}
	out := map[string]any{
		"started": key,
		"cwd":     filepath.Clean(cwd),
		"note":    "the session appears in ccdash once claude starts (a few seconds); open it there to drive it",
	}
	if h.name != "" {
		out["host"] = h.name
	}
	// A new folder can stop claude at the folder-trust question before it
	// registers anything; say so instead of leaving the caller guessing.
	if w := s.waitFor(ctx, h.st, key); w != "" {
		out["waiting_on"] = w
		out["note"] = "claude stopped at a dialog before starting the session; the operator must answer it in ccdash (TUI row / portal 起動中). Folders under ccdash's 'Trusted folders' setting skip the trust question."
	}
	if project != "" {
		out["project"] = project
	}
	return out, nil
}

// attachOn is the switch that keeps the TUI from spawning claude: it gates
// every tool that starts or stops one. Another host's switch is read from
// its /hub/info (it enforces it on /pty itself as well).
func (s *Server) attachOn(ctx context.Context, h host, what string) error {
	if !h.self {
		on, err := s.fleet.attachOn(ctx, h.id)
		if err != nil {
			return err
		}
		if !on {
			return fmt.Errorf("%s is OFF on host %s (its ccdash settings: Attach)", what, h.name)
		}
		return nil
	}
	cfg, err := settings.Load(ctx, s.st)
	if err != nil {
		return err
	}
	if !cfg.AttachEnabled {
		return fmt.Errorf("%s is OFF in ccdash (settings: Attach)", what)
	}
	return nil
}

// hosted reports whether h's collector runs session id's claude in a PTY.
func (s *Server) hosted(ctx context.Context, h host, id string) (bool, error) {
	keys, err := h.st.HostedSessions(ctx)
	if err != nil {
		return false, fmt.Errorf("ask the collector on %s which sessions it hosts (is it running?): %w", hostLabel(h), err)
	}
	for _, k := range keys {
		if k == id {
			return true, nil
		}
	}
	return false, nil
}

// stopSession ends one hosted claude (see the tool description for what it
// refuses).
func (s *Server) stopSession(ctx context.Context, ref, hostArg string, force bool) (any, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("session is required")
	}
	l, err := s.locate(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	if err := s.attachOn(ctx, l.h, "stopping sessions"); err != nil {
		return nil, err
	}
	x, name := l.x, l.ref()
	if self := s.self(); self != "" && self == x.SessionID {
		return nil, fmt.Errorf("%s is your own session; stopping it would cut off this very turn", name)
	}
	on, err := s.hosted(ctx, l.h, x.SessionID)
	if err != nil {
		return nil, err
	}
	if !on {
		if running(x) {
			return nil, fmt.Errorf("%s runs outside ccdash (started in a terminal, status %s); ccdash can't stop it — the operator has to end it where it runs", name, x.Status)
		}
		return nil, fmt.Errorf("%s is already stopped (status %s); resume_session brings it back", name, x.Status)
	}
	if x.Status == model.StatusActive && !force {
		return nil, fmt.Errorf("%s is working mid-turn; stopping it now loses that work — pass force: true only if the operator wants it stopped anyway", name)
	}
	if err := l.h.st.StopSession(ctx, x.SessionID); err != nil {
		return nil, fmt.Errorf("stop %s: %w", name, err)
	}
	return map[string]any{
		"stopped": name,
		"cwd":     x.Cwd,
		"note":    "claude has exited; the transcript is kept and resume_session brings the session back. ccdash shows it as stopped within ~10 s",
	}, nil
}

// resumeSession runs `claude --resume` for a stopped session under its
// host's collector, keyed by its session id (like the portal's resume and
// the restart path).
func (s *Server) resumeSession(ctx context.Context, ref, hostArg, prompt, mode string) (any, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("session is required")
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "-") {
		return nil, fmt.Errorf("prompt must not start with '-'")
	}
	l, err := s.locate(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	if err := s.attachOn(ctx, l.h, "starting sessions"); err != nil {
		return nil, err
	}
	x, name := l.x, l.ref()
	on, err := s.hosted(ctx, l.h, x.SessionID)
	if err != nil {
		return nil, err
	}
	if on {
		return nil, fmt.Errorf("%s is already running under ccdash; nothing to resume", name)
	}
	if running(x) {
		// Two claudes on one transcript would interleave it. Right after a
		// stop the row lags until the next discovery pass.
		return nil, fmt.Errorf("%s is still running outside ccdash (status %s); a second claude on the same transcript would garble it. If it was just stopped, wait ~10 s and retry", name, x.Status)
	}
	if x.Cwd == "" {
		return nil, fmt.Errorf("%s has no known directory; claude --resume needs it", name)
	}
	if x.Archived {
		if err := l.h.st.SetArchived(ctx, x.SessionID, false); err != nil {
			return nil, err
		}
	}
	key, err := l.h.st.StartSession(ctx, store.StartRequest{
		SessionID: x.SessionID, ResumeID: x.SessionID, Cwd: x.Cwd, Prompt: prompt, PermissionMode: mode,
	})
	if err != nil {
		return nil, fmt.Errorf("resume %s (is the ccdash collector running?): %w", name, err)
	}
	out := map[string]any{
		"resumed": name,
		"pty":     key,
		"cwd":     x.Cwd,
		"note":    "claude --resume is starting; the session shows as running in ccdash within a few seconds",
	}
	if x.Archived {
		out["unarchived"] = true
	}
	return out, nil
}

// restartView is a scheduled restart as the tools report it.
func restartView(rs model.RestartState) map[string]any {
	waiting := make([]string, len(rs.Waiting))
	for i, b := range rs.Waiting {
		ref := b.SessionID
		if b.Num > 0 {
			ref = fmt.Sprintf("#%d", b.Num)
		}
		waiting[i] = ref + " " + b.Why
	}
	out := map[string]any{
		"pending":         rs.Pending,
		"waiting_for":     waiting,
		"hosted_sessions": rs.Hosted,
	}
	if rs.Pending {
		out["since"] = rs.Since.Format(time.RFC3339)
		out["gives_up_at"] = rs.Deadline.Format(time.RFC3339)
		out["note"] = "the collector restarts once nothing in waiting_for is busy; every hosted session is then stopped and resumed"
	}
	if rs.Last != "" {
		out["last"] = rs.Last
	}
	return out
}

func (s *Server) scheduleRestart(ctx context.Context, hostArg string, cancel bool, minutes int) (any, error) {
	h, err := s.hostNamed(ctx, hostArg)
	if err != nil {
		return nil, err
	}
	withHost := func(m map[string]any) map[string]any {
		if h.name != "" {
			m["host"] = h.name
		}
		return m
	}
	if cancel {
		rs, err := h.st.CancelRestart(ctx)
		if err != nil {
			return nil, fmt.Errorf("cancel the restart on %s (is the ccdash collector running?): %w", hostLabel(h), err)
		}
		return withHost(restartView(rs)), nil
	}
	if err := s.attachOn(ctx, h, "restarting the collector"); err != nil {
		return nil, err
	}
	if minutes < 0 || minutes > 1440 {
		return nil, fmt.Errorf("timeout_minutes must be 1..1440")
	}
	rs, err := h.st.ScheduleRestart(ctx, "mcp", time.Duration(minutes)*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("schedule the restart on %s (is the ccdash collector running? it must run as `ccdash server` to restart itself): %w", hostLabel(h), err)
	}
	return withHost(restartView(rs)), nil
}
