package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

// The to-do list's tools (docs/decisions/0006-tasks.md). Tasks are named
// by ref ("B#11", see taskhosts.go), id ("11" / "#11") or key. Edits from a
// session link that session to the task, so "which sessions worked on #11"
// fills itself in — across hosts too.

var taskRef = map[string]any{"type": "string", "description": `task ref as list_tasks shows it ("B#11" = host B's task 11), "#11" / "11" (this machine's; if it has none, the one other host that does), or key (Jira key, PR URL …)`}

var (
	hostTasks   = map[string]any{"type": "string", "description": `only this host's list (a name or letter from list_hosts); default = every host this machine can reach`}
	hostTaskRef = map[string]any{"type": "string", "description": `the host of a bare "#11" or key (a name or letter from list_hosts); refs like "B#11" already name their host`}
	hostAddTask = map[string]any{"type": "string", "description": `whose list to add it to (a name or letter from list_hosts); default = this machine`}
)

var planArg = map[string]any{"type": "string", "description": "today / tomorrow (next weekday) / week (this Friday) / nextweek (next Monday) / YYYY-MM-DD / none"}

var taskReadTools = []map[string]any{
	{
		"name": "list_tasks",
		"description": "The operator's to-do list — every reachable host's, merged (each machine keeps its own; rows carry host and ref, e.g. \"B#11\") — " +
			"bucketed by the day they plan to work on each task (today — including carried-over ones —, tomorrow, this week, next week, later, unplanned). Open tasks by default. " +
			`When the operator says "#11" or "11 について", that is task 11 here — or, if this machine has none, on the one host that has it; "B#11" is host B's (unless the context says a PR / Jira number).`,
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
		"description": "For an automated sync of this machine's list: the open tasks with the Slack threads, Jira keys and PRs pulled out of their links, " +
			"the refs already in each timeline, and `since` (the last sync) to look for new activity from.",
		"inputSchema": schema(map[string]any{}),
		"annotations": readOnlyHint,
	},
}

var taskWriteTools = []map[string]any{
	{
		"name": "add_task",
		"description": "Add a task to the to-do list (this machine's unless host says otherwise). Give it a key when it has one (Jira key, PR URL, misc:<slug>) so automated syncs recognise it. " +
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
		"description": "Put tasks in this order at the top of their bucket (pass the refs of one bucket, all on one host).",
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
		"description": "For an automated sync of this machine's list (after task_context): merge what you found. Tasks match by key. The operator's decisions win: " +
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

func init() {
	for _, t := range append(append([]map[string]any(nil), taskReadTools...), taskWriteTools...) {
		props := t["inputSchema"].(map[string]any)["properties"].(map[string]any)
		switch t["name"] {
		case "list_tasks":
			props["host"] = hostTasks
		case "add_task":
			props["host"] = hostAddTask
		case "task_context", "sync_tasks":
		default:
			props["host"] = hostTaskRef
		}
	}
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
	Host            string               `json:"host"`
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
			out, err = s.getTask(ctx, a.Task, a.Host)
		}
	case "task_context":
		out, err = s.st.TaskContext(ctx)
	case "add_task":
		out, err = s.addTask(ctx, a)
	case "update_task":
		if err = needTask(); err == nil {
			out, err = s.patchTask(ctx, a.Task, a.Host, tasks.Patch{Title: a.Title, Key: a.Key, Priority: a.Priority, Due: a.Due,
				Project: a.Project, Note: a.Note, AddLinks: a.AddLinks, RemoveLinks: a.RemoveLinks})
		}
	case "set_task_status":
		if err = needTask(); err == nil {
			p := tasks.Patch{Status: &a.Status, Why: a.Why}
			if a.Archive {
				yes := true
				p.Archived = &yes
			}
			out, err = s.patchTask(ctx, a.Task, a.Host, p)
		}
	case "plan_task":
		if err = needTask(); err == nil {
			if a.Plan == nil {
				return nil, true, fmt.Errorf("plan is required")
			}
			out, err = s.patchTask(ctx, a.Task, a.Host, tasks.Patch{Plan: a.Plan, Why: a.Why})
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
		out, err = s.orderTasks(ctx, a.Tasks, a.Host)
	case "archive_task":
		if err = needTask(); err == nil {
			v := a.Archived == nil || *a.Archived
			out, err = s.patchTask(ctx, a.Task, a.Host, tasks.Patch{Archived: &v})
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
	Ref         string   `json:"ref"`
	Host        string   `json:"host,omitempty"`
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
	hs, offline, err := s.hostsFor(ctx, a.Host)
	if err != nil {
		return nil, err
	}
	ls, errs, err := s.taskListsOn(ctx, hs, a.IncludeArchived)
	if err != nil {
		return nil, err
	}
	now := s.now()
	today := tasks.DateString(tasks.Day(now))
	var sessions map[string]located
	limit := a.Limit
	if limit <= 0 {
		limit = 100
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	type ranked struct {
		r      taskRow
		bucket int
	}
	order := map[string]int{}
	for i, k := range tasks.BucketKeys() {
		order[k] = i
	}
	var all []ranked
	headings := map[string]string{}
	for i, l := range ls {
		h := hs[i]
		for _, b := range l.Buckets {
			if headings[b.Key] == "" {
				headings[b.Key] = b.Heading
			}
		}
		for _, t := range l.Tasks {
			if t.Finished() && !a.IncludeFinished && !t.Archived ||
				a.Bucket != "" && t.Bucket != a.Bucket ||
				a.Project != nil && t.Project != *a.Project ||
				a.Status != "" && t.Status != a.Status ||
				q != "" && !strings.Contains(taskText(t), q) && !strings.EqualFold(q, taskRefOn(h, t.ID)) {
				continue
			}
			r := taskRow{Ref: taskRefOn(h, t.ID), ID: t.ID, Key: t.Key, Title: t.Title, Priority: t.Priority, Status: t.Status, Bucket: t.Bucket,
				Plan: t.Plan, CarriedOver: tasks.CarriedOver(t, now), Due: t.Due, Overdue: !t.Finished() && t.Due != "" && t.Due < today,
				Project: t.Project, Archived: t.Archived, Links: len(t.Links)}
			if len(hs) > 1 {
				r.Host = hostLabel(h)
			}
			if n := len(t.Timeline); n > 0 {
				r.Latest, r.LatestAt = clip(t.Timeline[n-1].Text, 200), t.Timeline[n-1].At
			}
			if len(t.Sessions) > 0 && sessions == nil {
				sessions = s.sessionIndex(ctx)
			}
			for _, sid := range t.Sessions {
				if l, ok := sessions[sid]; ok && l.ref() != "" {
					r.Sessions = append(r.Sessions, l.ref())
				} else {
					r.Sessions = append(r.Sessions, sid)
				}
			}
			all = append(all, ranked{r, order[t.Bucket]})
		}
	}
	// Each host's list is in its own order; across hosts, bucket by bucket.
	sort.SliceStable(all, func(i, j int) bool { return all[i].bucket < all[j].bucket })
	rows := []taskRow{}
	for _, x := range all {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, x.r)
	}
	res := map[string]any{"today": today, "buckets": headings, "matched": len(all), "tasks": rows}
	if len(hs) > 1 {
		res["hosts"] = hostNames(hs)
	}
	noteHosts(res, offline, errs)
	return res, nil
}

func taskText(t model.Task) string {
	parts := []string{t.Key, t.Title, t.Project, t.Note}
	for _, e := range t.Timeline {
		parts = append(parts, e.Text)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func (s *Server) getTask(ctx context.Context, ref, hostArg string) (any, error) {
	ht, err := s.locateTask(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	return s.taskOut(ctx, ht), nil
}

// taskOut is a whole task with its ref and its sessions spelled out.
func (s *Server) taskOut(ctx context.Context, ht hostTask) map[string]any {
	now := s.now()
	t := ht.t
	out := map[string]any{"ref": ht.ref(), "task": t, "bucket": tasks.BucketOf(t, now)}
	if !ht.h.self {
		out["host"] = hostLabel(ht.h)
	}
	if tasks.CarriedOver(t, now) {
		out["carried_over"] = true
	}
	if len(t.Sessions) > 0 {
		idx := s.sessionIndex(ctx)
		var ss []map[string]any
		for _, sid := range t.Sessions {
			l, ok := idx[sid]
			if !ok {
				ss = append(ss, map[string]any{"session_id": sid})
				continue
			}
			x := l.x
			row := map[string]any{"ref": l.ref(), "title": x.DisplayTitle(), "status": string(x.Status), "session_id": x.SessionID}
			if !l.h.self {
				row["host"] = hostLabel(l.h)
			}
			if s.hubURL != "" {
				row["url"] = s.hubURL + "/s/" + url.PathEscape(x.SessionID)
			}
			ss = append(ss, row)
		}
		out["sessions"] = ss
	}
	return out
}

// withSelf adds the calling session to the patch, when it can tell. The
// session may live on another host than the task: links are session ids.
func (s *Server) withSelf(p *tasks.Patch) {
	if sid := s.self(); sid != "" {
		p.AddSessions = append(p.AddSessions, sid)
	}
}

func (s *Server) patchTask(ctx context.Context, ref, hostArg string, p tasks.Patch) (any, error) {
	ht, err := s.locateTask(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	p.Source = tasks.SourceClaude
	s.withSelf(&p)
	t, err := ht.h.st.UpdateTask(ctx, ht.id(), p)
	if err != nil {
		return nil, ht.h.explain(err)
	}
	return s.taskOut(ctx, hostTask{t, ht.h}), nil
}

// resolveSessionIDs turns session refs into ids, on whichever host they run.
func (s *Server) resolveSessionIDs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	for _, r := range refs {
		l, err := s.locate(ctx, r, "")
		if err != nil {
			return nil, err
		}
		ids = append(ids, l.x.SessionID)
	}
	return ids, nil
}

func (s *Server) addTask(ctx context.Context, a taskArgs) (any, error) {
	h, err := s.hostNamed(ctx, a.Host)
	if err != nil {
		return nil, err
	}
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
	t, err := h.st.AddTask(ctx, in)
	if err != nil {
		return nil, h.explain(err)
	}
	return s.taskOut(ctx, hostTask{t, h}), nil
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
	ht, err := s.locateTask(ctx, a.Task, a.Host)
	if err != nil {
		return nil, err
	}
	t, err := ht.h.st.AddTaskNote(ctx, ht.id(), model.TaskNote{At: at, Source: src, Text: a.Text, Link: a.Link, Ref: a.Ref})
	if err != nil {
		return nil, ht.h.explain(err)
	}
	if sid := s.self(); sid != "" {
		if t2, err := ht.h.st.UpdateTask(ctx, ht.id(), tasks.Patch{AddSessions: []string{sid}, Source: tasks.SourceClaude}); err == nil {
			t = t2
		}
	}
	return s.taskOut(ctx, hostTask{t, ht.h}), nil
}

func (s *Server) linkTask(ctx context.Context, a taskArgs) (any, error) {
	ids, err := s.resolveSessionIDs(ctx, a.Sessions)
	if err != nil {
		return nil, err
	}
	if len(a.Links) == 0 && len(ids) == 0 {
		return nil, fmt.Errorf("links or sessions is required")
	}
	ht, err := s.locateTask(ctx, a.Task, a.Host)
	if err != nil {
		return nil, err
	}
	p := tasks.Patch{Source: tasks.SourceClaude}
	if a.Unlink {
		p.RemoveLinks, p.RemoveSessions = a.Links, ids
	} else {
		p.AddLinks, p.AddSessions = a.Links, ids
	}
	t, err := ht.h.st.UpdateTask(ctx, ht.id(), p)
	if err != nil {
		return nil, ht.h.explain(err)
	}
	return s.taskOut(ctx, hostTask{t, ht.h}), nil
}

func (s *Server) orderTasks(ctx context.Context, refs []string, hostArg string) (any, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("tasks is required")
	}
	var on host
	ids := make([]int64, 0, len(refs))
	out := make([]string, 0, len(refs))
	for i, r := range refs {
		ht, err := s.locateTask(ctx, r, hostArg)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			on = ht.h
		} else if !sameHost(on, ht.h) {
			return nil, fmt.Errorf("order_tasks orders one host's list: %s and %s are on different hosts", out[0], ht.ref())
		}
		ids = append(ids, ht.t.ID)
		out = append(out, ht.ref())
	}
	if err := on.st.OrderTasks(ctx, ids); err != nil {
		return nil, on.explain(err)
	}
	return map[string]any{"ordered": out}, nil
}
