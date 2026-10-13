package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

// The to-do list's tools (docs/decisions/0006-tasks.md). Tasks are named
// by id ("11" / "#11") or key. Edits from a session link that session to
// the task, so "which sessions worked on #11" fills itself in.

var taskRef = map[string]any{"type": "string", "description": `task id ("11" or "#11") or key (Jira key, PR URL …)`}

var planArg = map[string]any{"type": "string", "description": "today / tomorrow (next weekday) / week (this Friday) / nextweek (next Monday) / YYYY-MM-DD / none"}

var taskReadTools = []map[string]any{
	{
		"name": "list_tasks",
		"description": "The operator's to-do list on this machine, bucketed by the day they plan to work on each task " +
			"(today — including carried-over ones —, tomorrow, this week, next week, later, unplanned). Open tasks by default. " +
			`When the operator says "#11" or "11 について", that is task 11 here (unless the context says a PR / Jira number).`,
		"inputSchema": schema(map[string]any{
			"bucket":           map[string]any{"type": "string", "enum": tasks.BucketKeys()},
			"project":          map[string]any{"type": "string"},
			"query":            map[string]any{"type": "string", "description": "matches title, key, project, note and timeline"},
			"status":           map[string]any{"type": "string", "enum": statusKeys()},
			"include_finished": map[string]any{"type": "boolean", "description": "done / cancelled tasks that aren't archived"},
			"include_archived": map[string]any{"type": "boolean"},
			"limit":            map[string]any{"type": "integer", "description": "default 100"},
		}),
		"annotations": readOnlyHint,
	},
	{
		"name":        "get_task",
		"description": "One task in full: note, links, the whole timeline (oldest first) and the sessions that worked on it (with their portal links). Read it before working on a task.",
		"inputSchema": required(map[string]any{"task": taskRef}, "task"),
		"annotations": readOnlyHint,
	},
	{
		"name": "task_context",
		"description": "For an automated sync: the open tasks with the Slack threads, Jira keys and PRs pulled out of their links, " +
			"the refs already in each timeline, and `since` (the last sync) to look for new activity from.",
		"inputSchema": schema(map[string]any{}),
		"annotations": readOnlyHint,
	},
}

var taskWriteTools = []map[string]any{
	{
		"name": "add_task",
		"description": "Add a task to the to-do list. Give it a key when it has one (Jira key, PR URL, misc:<slug>) so automated syncs recognise it. " +
			"project is a ccdash project (created on first use). The calling session is linked to the task.",
		"inputSchema": required(map[string]any{
			"title":    map[string]any{"type": "string", "description": "what to do, in the operator's language"},
			"key":      map[string]any{"type": "string"},
			"priority": map[string]any{"type": "integer", "enum": []int{1, 2, 3, 4}, "description": "1 now, 2 this week, 3 soon (default), 4 someday"},
			"plan":     planArg,
			"due":      map[string]any{"type": "string", "description": "deadline YYYY-MM-DD"},
			"project":  map[string]any{"type": "string"},
			"note":     map[string]any{"type": "string"},
			"links":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"sessions": sessionList,
		}, "title"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
	},
	{
		"name":        "update_task",
		"description": "Change a task's title, key, priority, due date, project, note or links when the operator asks for it. Status, plan and archiving have their own tools.",
		"inputSchema": required(map[string]any{
			"task":         taskRef,
			"title":        map[string]any{"type": "string"},
			"key":          map[string]any{"type": "string"},
			"priority":     map[string]any{"type": "integer", "enum": []int{1, 2, 3, 4}},
			"due":          map[string]any{"type": "string", "description": `YYYY-MM-DD; "" clears`},
			"project":      map[string]any{"type": "string", "description": `"" takes it out of its project`},
			"note":         map[string]any{"type": "string", "description": "replaces the note"},
			"add_links":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"remove_links": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, "task"),
		"annotations": writeHint,
	},
	{
		"name": "set_task_status",
		"description": "Change a task's status (logged in its timeline with the reason): todo, doing, waiting, done, cancelled. " +
			"archive=true also archives it (only done / cancelled tasks can be archived; there is no delete).",
		"inputSchema": required(map[string]any{
			"task":    taskRef,
			"status":  map[string]any{"type": "string", "enum": statusKeys()},
			"why":     map[string]any{"type": "string"},
			"archive": map[string]any{"type": "boolean"},
		}, "task", "status"),
		"annotations": writeHint,
	},
	{
		"name":        "plan_task",
		"description": "Set the day to work on a task, when the operator decides it. It counts as their decision: automated syncs won't move it today. The task joins the end of its new bucket.",
		"inputSchema": required(map[string]any{"task": taskRef, "plan": planArg, "why": map[string]any{"type": "string"}}, "task", "plan"),
		"annotations": writeHint,
	},
	{
		"name":        "add_task_note",
		"description": "Append progress to a task's timeline (what was done, decided, found). Do this when work on a task moves. ref dedups (e.g. slack:<channel>:<ts>).",
		"inputSchema": required(map[string]any{
			"task":   taskRef,
			"text":   map[string]any{"type": "string"},
			"link":   map[string]any{"type": "string"},
			"source": map[string]any{"type": "string", "enum": []string{"claude", "slack", "jira", "github", "manual"}, "description": "where it came from (default claude)"},
			"at":     map[string]any{"type": "string", "description": "RFC 3339 time of the event (default now)"},
			"ref":    map[string]any{"type": "string"},
		}, "task", "text"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
	},
	{
		"name":        "link_task",
		"description": "Link URLs / paths (a working directory, a doc) or sessions to a task, or unlink them with unlink=true.",
		"inputSchema": required(map[string]any{
			"task":     taskRef,
			"links":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"sessions": sessionList,
			"unlink":   map[string]any{"type": "boolean"},
		}, "task"),
		"annotations": writeHint,
	},
	{
		"name":        "order_tasks",
		"description": "Put tasks in this order at the top of their bucket (pass the ids of one bucket).",
		"inputSchema": required(map[string]any{"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "tasks"),
		"annotations": writeHint,
	},
	{
		"name":        "archive_task",
		"description": "Archive a done or cancelled task (archived=false brings one back).",
		"inputSchema": required(map[string]any{"task": taskRef, "archived": map[string]any{"type": "boolean", "description": "default true"}}, "task"),
		"annotations": writeHint,
	},
	{
		"name": "sync_tasks",
		"description": "For an automated sync (after task_context): merge what you found. Tasks match by key. The operator's decisions win: " +
			"title, priority and note are never overwritten; a plan set by hand today isn't moved; replan needs a why; a status other than done changes " +
			"only when the same call brings new timeline entries; a finished task is never reopened; a new task's project is kept only if it exists.",
		"inputSchema": required(map[string]any{
			"synced_at": map[string]any{"type": "string", "description": "when this run looked (RFC 3339); the next task_context starts there"},
			"tasks": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"},
					"priority": map[string]any{"type": "integer"}, "plan": map[string]any{"type": "string"},
					"due": map[string]any{"type": "string"}, "project": map[string]any{"type": "string"},
					"note": map[string]any{"type": "string"}, "links": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"done": map[string]any{"type": "boolean"}, "status": map[string]any{"type": "string", "enum": statusKeys()},
					"timeline": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
						"at": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"},
						"link": map[string]any{"type": "string"}, "ref": map[string]any{"type": "string"},
					}}},
					"replan": map[string]any{"type": "object", "properties": map[string]any{"plan": map[string]any{"type": "string"}, "why": map[string]any{"type": "string"}}},
				},
				"required": []string{"key", "title"},
			}},
		}, "tasks"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
}

func statusKeys() []string {
	return []string{model.TaskTodo, model.TaskDoing, model.TaskWaiting, model.TaskDone, model.TaskCancelled}
}

func isTaskTool(name string) (known, write bool) {
	for _, t := range taskReadTools {
		if t["name"] == name {
			return true, false
		}
	}
	for _, t := range taskWriteTools {
		if t["name"] == name {
			return true, true
		}
	}
	return false, false
}

type taskArgs struct {
	Task            string               `json:"task"`
	Tasks           []string             `json:"tasks"`
	Bucket          string               `json:"bucket"`
	Project         *string              `json:"project"`
	Query           string               `json:"query"`
	Status          string               `json:"status"`
	IncludeFinished bool                 `json:"include_finished"`
	IncludeArchived bool                 `json:"include_archived"`
	Limit           int                  `json:"limit"`
	Title           *string              `json:"title"`
	Key             *string              `json:"key"`
	Priority        *int                 `json:"priority"`
	Plan            *string              `json:"plan"`
	Due             *string              `json:"due"`
	Note            *string              `json:"note"`
	Links           []string             `json:"links"`
	AddLinks        []string             `json:"add_links"`
	RemoveLinks     []string             `json:"remove_links"`
	Sessions        []string             `json:"sessions"`
	Unlink          bool                 `json:"unlink"`
	Why             string               `json:"why"`
	Archive         bool                 `json:"archive"`
	Archived        *bool                `json:"archived"`
	Text            string               `json:"text"`
	Link            string               `json:"link"`
	Source          string               `json:"source"`
	At              string               `json:"at"`
	Ref             string               `json:"ref"`
	SyncedAt        string               `json:"synced_at"`
	SyncTasks       []tasks.ProposalTask `json:"-"`
}

// callTask runs a task tool; ok is false for other tools.
func (s *Server) callTask(ctx context.Context, name string, raw json.RawMessage) (out any, ok bool, err error) {
	known, write := isTaskTool(name)
	if !known {
		return nil, false, nil
	}
	if write && s.readOnly {
		return nil, true, fmt.Errorf("%s is not available: ccdash mcp runs with --read-only", name)
	}
	var a taskArgs
	if len(raw) > 0 && string(raw) != "null" {
		// sync_tasks' "tasks" are proposal objects, not task refs.
		if name == "sync_tasks" {
			var p tasks.Proposal
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, true, fmt.Errorf("bad arguments: %w", err)
			}
			a.SyncTasks, a.SyncedAt = p.Tasks, p.SyncedAt
		} else if err := json.Unmarshal(raw, &a); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}
	needTask := func() error {
		if strings.TrimSpace(a.Task) == "" {
			return fmt.Errorf("task is required")
		}
		return nil
	}
	switch name {
	case "list_tasks":
		out, err = s.listTasks(ctx, a)
	case "get_task":
		if err = needTask(); err == nil {
			out, err = s.getTask(ctx, a.Task)
		}
	case "task_context":
		out, err = s.st.TaskContext(ctx)
	case "add_task":
		out, err = s.addTask(ctx, a)
	case "update_task":
		if err = needTask(); err == nil {
			out, err = s.patchTask(ctx, a.Task, tasks.Patch{Title: a.Title, Key: a.Key, Priority: a.Priority, Due: a.Due,
				Project: a.Project, Note: a.Note, AddLinks: a.AddLinks, RemoveLinks: a.RemoveLinks})
		}
	case "set_task_status":
		if err = needTask(); err == nil {
			p := tasks.Patch{Status: &a.Status, Why: a.Why}
			if a.Archive {
				yes := true
				p.Archived = &yes
			}
			out, err = s.patchTask(ctx, a.Task, p)
		}
	case "plan_task":
		if err = needTask(); err == nil {
			if a.Plan == nil {
				return nil, true, fmt.Errorf("plan is required")
			}
			out, err = s.patchTask(ctx, a.Task, tasks.Patch{Plan: a.Plan, Why: a.Why})
		}
	case "add_task_note":
		if err = needTask(); err == nil {
			out, err = s.addTaskNote(ctx, a)
		}
	case "link_task":
		if err = needTask(); err == nil {
			out, err = s.linkTask(ctx, a)
		}
	case "order_tasks":
		out, err = s.orderTasks(ctx, a.Tasks)
	case "archive_task":
		if err = needTask(); err == nil {
			v := a.Archived == nil || *a.Archived
			out, err = s.patchTask(ctx, a.Task, tasks.Patch{Archived: &v})
		}
	case "sync_tasks":
		var res tasks.SyncResult
		if res, err = s.st.SyncTasks(ctx, tasks.Proposal{SyncedAt: a.SyncedAt, Tasks: a.SyncTasks}); err == nil {
			out = res
		}
	}
	return out, true, err
}

// taskRow is a task as list_tasks shows it: everything but the timeline,
// which shrinks to its latest entry.
type taskRow struct {
	ID          int64    `json:"id"`
	Key         string   `json:"key,omitempty"`
	Title       string   `json:"title"`
	Priority    int      `json:"priority"`
	Status      string   `json:"status"`
	Bucket      string   `json:"bucket"`
	Plan        string   `json:"plan,omitempty"`
	CarriedOver bool     `json:"carried_over,omitempty"`
	Due         string   `json:"due,omitempty"`
	Overdue     bool     `json:"overdue,omitempty"`
	Project     string   `json:"project,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
	Latest      string   `json:"latest,omitempty"`
	LatestAt    string   `json:"latest_at,omitempty"`
	Links       int      `json:"links,omitempty"`
	Sessions    []string `json:"sessions,omitempty"`
}

func (s *Server) listTasks(ctx context.Context, a taskArgs) (any, error) {
	l, err := s.st.ListTasks(ctx, a.IncludeArchived)
	if err != nil {
		return nil, err
	}
	now := s.now()
	today := tasks.DateString(tasks.Day(now))
	refs := s.sessionRefs(ctx)
	limit := a.Limit
	if limit <= 0 {
		limit = 100
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	rows := []taskRow{}
	matched := 0
	for _, t := range l.Tasks {
		if t.Finished() && !a.IncludeFinished && !t.Archived ||
			a.Bucket != "" && t.Bucket != a.Bucket ||
			a.Project != nil && t.Project != *a.Project ||
			a.Status != "" && t.Status != a.Status ||
			q != "" && !strings.Contains(taskText(t), q) {
			continue
		}
		matched++
		if len(rows) >= limit {
			continue
		}
		r := taskRow{ID: t.ID, Key: t.Key, Title: t.Title, Priority: t.Priority, Status: t.Status, Bucket: t.Bucket,
			Plan: t.Plan, CarriedOver: tasks.CarriedOver(t, now), Due: t.Due, Overdue: !t.Finished() && t.Due != "" && t.Due < today,
			Project: t.Project, Archived: t.Archived, Links: len(t.Links)}
		if n := len(t.Timeline); n > 0 {
			r.Latest, r.LatestAt = clip(t.Timeline[n-1].Text, 200), t.Timeline[n-1].At
		}
		for _, sid := range t.Sessions {
			r.Sessions = append(r.Sessions, refs.of(sid))
		}
		rows = append(rows, r)
	}
	headings := map[string]string{}
	for _, b := range l.Buckets {
		headings[b.Key] = b.Heading
	}
	return map[string]any{"today": l.Today, "buckets": headings, "matched": matched, "tasks": rows}, nil
}

func taskText(t model.Task) string {
	parts := []string{t.Key, t.Title, t.Project, t.Note}
	for _, e := range t.Timeline {
		parts = append(parts, e.Text)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// sessionRefIndex maps session ids to "#N" (falling back to the id).
type sessionRefIndex map[string]string

func (r sessionRefIndex) of(sid string) string {
	if v := r[sid]; v != "" {
		return v
	}
	return sid
}

func (s *Server) sessionRefs(ctx context.Context) sessionRefIndex {
	out := sessionRefIndex{}
	for _, archived := range []bool{false, true} {
		ss, err := s.st.ListSessions(ctx, archived)
		if err != nil {
			continue
		}
		for _, x := range ss {
			out[x.SessionID] = x.Ref()
		}
	}
	return out
}

func (s *Server) getTask(ctx context.Context, ref string) (any, error) {
	t, err := s.st.GetTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.taskOut(ctx, t), nil
}

// taskOut is a whole task with its sessions spelled out.
func (s *Server) taskOut(ctx context.Context, t model.Task) map[string]any {
	now := s.now()
	out := map[string]any{"task": t, "bucket": tasks.BucketOf(t, now)}
	if tasks.CarriedOver(t, now) {
		out["carried_over"] = true
	}
	if len(t.Sessions) > 0 {
		var ss []map[string]any
		for _, sid := range t.Sessions {
			x, err := s.findSession(ctx, sid)
			if err != nil {
				ss = append(ss, map[string]any{"session_id": sid})
				continue
			}
			row := map[string]any{"ref": x.Ref(), "title": x.DisplayTitle(), "status": string(x.Status), "session_id": x.SessionID}
			if s.hubURL != "" {
				row["url"] = s.hubURL + "/s/" + url.PathEscape(x.SessionID)
			}
			ss = append(ss, row)
		}
		out["sessions"] = ss
	}
	return out
}

// withSelf adds the calling session to the patch, when it can tell.
func (s *Server) withSelf(p *tasks.Patch) {
	if sid := s.self(); sid != "" {
		p.AddSessions = append(p.AddSessions, sid)
	}
}

func (s *Server) patchTask(ctx context.Context, ref string, p tasks.Patch) (any, error) {
	p.Source = tasks.SourceClaude
	s.withSelf(&p)
	t, err := s.st.UpdateTask(ctx, ref, p)
	if err != nil {
		return nil, err
	}
	return s.taskOut(ctx, t), nil
}

func (s *Server) resolveSessionIDs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	for _, r := range refs {
		x, err := s.findSession(ctx, r)
		if err != nil {
			return nil, err
		}
		ids = append(ids, x.SessionID)
	}
	return ids, nil
}

func (s *Server) addTask(ctx context.Context, a taskArgs) (any, error) {
	in := tasks.Input{Links: a.Links, Source: tasks.SourceClaude}
	if a.Title != nil {
		in.Title = *a.Title
	}
	if a.Key != nil {
		in.Key = *a.Key
	}
	if a.Priority != nil {
		in.Priority = *a.Priority
	}
	if a.Plan != nil {
		in.Plan = *a.Plan
	}
	if a.Due != nil {
		in.Due = *a.Due
	}
	if a.Project != nil {
		in.Project = *a.Project
	}
	if a.Note != nil {
		in.Note = *a.Note
	}
	ids, err := s.resolveSessionIDs(ctx, a.Sessions)
	if err != nil {
		return nil, err
	}
	if sid := s.self(); sid != "" {
		ids = append(ids, sid)
	}
	in.Sessions = ids
	t, err := s.st.AddTask(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.taskOut(ctx, t), nil
}

func (s *Server) addTaskNote(ctx context.Context, a taskArgs) (any, error) {
	if strings.TrimSpace(a.Text) == "" {
		return nil, fmt.Errorf("text is required")
	}
	at := a.At
	if at == "" {
		at = tasks.Stamp(s.now())
	} else if _, err := time.Parse(time.RFC3339, at); err != nil {
		return nil, fmt.Errorf("at must be RFC 3339: %w", err)
	}
	src := a.Source
	if src == "" {
		src = tasks.SourceClaude
	}
	t, err := s.st.AddTaskNote(ctx, a.Task, model.TaskNote{At: at, Source: src, Text: a.Text, Link: a.Link, Ref: a.Ref})
	if err != nil {
		return nil, err
	}
	if sid := s.self(); sid != "" {
		if t2, err := s.st.UpdateTask(ctx, strconv.FormatInt(t.ID, 10), tasks.Patch{AddSessions: []string{sid}, Source: tasks.SourceClaude}); err == nil {
			t = t2
		}
	}
	return s.taskOut(ctx, t), nil
}

func (s *Server) linkTask(ctx context.Context, a taskArgs) (any, error) {
	ids, err := s.resolveSessionIDs(ctx, a.Sessions)
	if err != nil {
		return nil, err
	}
	if len(a.Links) == 0 && len(ids) == 0 {
		return nil, fmt.Errorf("links or sessions is required")
	}
	p := tasks.Patch{Source: tasks.SourceClaude}
	if a.Unlink {
		p.RemoveLinks, p.RemoveSessions = a.Links, ids
	} else {
		p.AddLinks, p.AddSessions = a.Links, ids
	}
	t, err := s.st.UpdateTask(ctx, a.Task, p)
	if err != nil {
		return nil, err
	}
	return s.taskOut(ctx, t), nil
}

func (s *Server) orderTasks(ctx context.Context, refs []string) (any, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("tasks is required")
	}
	ids := make([]int64, 0, len(refs))
	for _, r := range refs {
		t, err := s.st.GetTask(ctx, r)
		if err != nil {
			return nil, err
		}
		ids = append(ids, t.ID)
	}
	if err := s.st.OrderTasks(ctx, ids); err != nil {
		return nil, err
	}
	return map[string]any{"ordered": ids}, nil
}

// tasksBySession maps session ids to the open tasks linked to them, for
// the session rows.
func (s *Server) tasksBySession(ctx context.Context) map[string][]int64 {
	l, err := s.st.ListTasks(ctx, false)
	if err != nil {
		return nil
	}
	out := map[string][]int64{}
	for _, t := range l.Tasks {
		if t.Finished() {
			continue
		}
		for _, sid := range t.Sessions {
			out[sid] = append(out[sid], t.ID)
		}
	}
	return out
}
