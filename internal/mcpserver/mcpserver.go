// Package mcpserver is `ccdash mcp`: a Model Context Protocol server on
// stdio that lets Claude see what ccdash sees — projects, sessions, what
// needs the operator, a session's recent conversation — and organize it:
// put sessions into projects, create / rename / merge projects, recolor
// them, retitle and archive sessions. It goes through store.Store, so it
// works against the local DB or a remote collector (-r) alike. It never
// starts or stops claude and never answers approvals; --read-only drops
// the organizing tools.
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
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
The organizing tools (set_project, rename_project, order_projects, set_project_color, set_title, set_archived)
change what the operator sees in ccdash; projects are just names, created on first use.`

// Server serves one stdio connection.
type Server struct {
	st       store.Store
	version  string
	readOnly bool
	now      func() time.Time
}

// New returns a server over st; readOnly leaves out the organizing tools.
func New(st store.Store, version string, readOnly bool) *Server {
	return &Server{st: st, version: version, readOnly: readOnly, now: time.Now}
}

func (s *Server) tools() []map[string]any {
	if s.readOnly {
		return readTools
	}
	return append(append([]map[string]any{}, readTools...), writeTools...)
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
		"name":        "set_archived",
		"description": "Archive sessions (hide them from the working list) or bring them back.",
		"inputSchema": required(map[string]any{
			"sessions": sessionList,
			"archived": map[string]any{"type": "boolean"},
		}, "sessions", "archived"),
		"annotations": writeHint,
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
		"description": "Sessions, newest first. Filter by project, by text (title, path, repo, #N), to running ones, or to ones that need attention.",
		"inputSchema": schema(map[string]any{
			"project":          map[string]any{"type": "string", "description": "only sessions in this project"},
			"query":            map[string]any{"type": "string", "description": "case-insensitive text to match in title / cwd / repo / branch / group / #N"},
			"running_only":     map[string]any{"type": "boolean", "description": "only sessions whose claude is running (active or idle)"},
			"attention_only":   map[string]any{"type": "boolean", "description": "only sessions that need the operator or finished unseen"},
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
	case "set_archived":
		if args.Archived == nil {
			return "", fmt.Errorf("archived is required")
		}
		v, err = s.setArchived(ctx, args.Sessions, *args.Archived)
	case "overview":
		v, err = s.overview(ctx)
	case "list_projects":
		v, err = s.listProjects(ctx)
	case "list_sessions":
		v, err = s.listSessions(ctx, args.Project, args.Query, args.RunningOnly, args.AttentionOnly, args.IncludeArchived, args.Limit)
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
	Favorite  bool   `json:"favorite,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
	SessionID string `json:"session_id"`
}

func (s *Server) row(x model.Session) sessionRow {
	r := sessionRow{
		Ref: x.Ref(), Title: x.DisplayTitle(), Project: x.Project, Group: groupOf(x),
		Status: string(x.Status), Attention: x.Attention, Reason: x.AttentionReason, Pending: x.PendingCount,
		Cwd: x.Cwd, Branch: x.Branch, Favorite: x.Favorite, Archived: x.Archived, SessionID: x.SessionID,
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

func (s *Server) listSessions(ctx context.Context, project, query string, runningOnly, attentionOnly, archived bool, limit int) (any, error) {
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
