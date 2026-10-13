package tasks

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// Timeline sources. claude = an interactive Claude session, manual = the
// person in the TUI / portal, auto = an automated sync.
const (
	SourceClaude = "claude"
	SourceManual = "manual"
	SourceAuto   = "auto"
)

// Input is a new task.
type Input struct {
	Key      string   `json:"key,omitempty"`
	Title    string   `json:"title"`
	Priority int      `json:"priority,omitempty"`
	Status   string   `json:"status,omitempty"`
	Plan     string   `json:"plan,omitempty"` // as ParsePlan takes it
	Due      string   `json:"due,omitempty"`
	Project  string   `json:"project,omitempty"`
	Note     string   `json:"note,omitempty"`
	Links    []string `json:"links,omitempty"`
	Sessions []string `json:"sessions,omitempty"` // session ids
	Source   string   `json:"source,omitempty"`
}

// New builds a task from in (ID left 0 for the store to assign).
func New(in Input, now time.Time) (model.Task, error) {
	t := model.Task{
		Key:       strings.TrimSpace(in.Key),
		Title:     strings.TrimSpace(in.Title),
		Priority:  in.Priority,
		Status:    in.Status,
		Due:       strings.TrimSpace(in.Due),
		Project:   strings.TrimSpace(in.Project),
		Note:      in.Note,
		Links:     dedup(nil, in.Links),
		Sessions:  dedup(nil, in.Sessions),
		Source:    in.Source,
		CreatedOn: DateString(Day(now)),
		Timeline:  []model.TaskNote{},
	}
	if t.Title == "" {
		return t, fmt.Errorf("title is required")
	}
	if t.Priority == 0 {
		t.Priority = 3
	}
	if t.Priority < 1 || t.Priority > 4 {
		return t, fmt.Errorf("priority must be 1–4")
	}
	if t.Status == "" {
		t.Status = model.TaskTodo
	}
	if !ValidStatus(t.Status) || t.Status == model.TaskDone || t.Status == model.TaskCancelled {
		return t, fmt.Errorf("a new task's status is todo / doing / waiting")
	}
	if !ValidDate(t.Due) {
		return t, fmt.Errorf("due must be YYYY-MM-DD: %q", t.Due)
	}
	if t.Source == "" {
		t.Source = SourceClaude
	}
	plan, err := ParsePlan(in.Plan, now)
	if err != nil {
		return t, err
	}
	t.Plan = plan
	if plan != "" {
		t.PlanBy, t.PlanOn = planByFor(t.Source), DateString(Day(now))
	}
	return t, nil
}

func planByFor(source string) string {
	if source == SourceAuto {
		return "auto"
	}
	return "manual"
}

// Patch is a partial edit. Nil fields are left alone.
type Patch struct {
	Title    *string `json:"title,omitempty"`
	Key      *string `json:"key,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	Due      *string `json:"due,omitempty"`
	Project  *string `json:"project,omitempty"`
	Note     *string `json:"note,omitempty"`

	// Status changes leave a timeline entry, with Why when given.
	Status *string `json:"status,omitempty"`
	// Plan takes what ParsePlan takes ("none" clears). Auto marks an
	// automated replan: it needs Why and doesn't move a plan the person
	// set today.
	Plan *string `json:"plan,omitempty"`
	Auto bool    `json:"auto,omitempty"`
	Why  string  `json:"why,omitempty"`

	AddLinks       []string `json:"add_links,omitempty"`
	RemoveLinks    []string `json:"remove_links,omitempty"`
	AddSessions    []string `json:"add_sessions,omitempty"`
	RemoveSessions []string `json:"remove_sessions,omitempty"`

	// Archived archives (only a done / cancelled task) or brings it back.
	Archived *bool `json:"archived,omitempty"`

	// Source is who makes the edit, for the timeline (default claude).
	Source string `json:"source,omitempty"`
}

// Apply edits t in place, appending timeline entries for status and plan
// changes. It reports whether anything changed.
func Apply(t *model.Task, p Patch, now time.Time) (bool, error) {
	src := p.Source
	if src == "" {
		src = SourceClaude
	}
	why := strings.TrimSpace(p.Why)
	changed := false
	set := func(dst *string, v *string, trim bool) {
		if v == nil {
			return
		}
		nv := *v
		if trim {
			nv = strings.TrimSpace(nv)
		}
		if *dst != nv {
			*dst = nv
			changed = true
		}
	}
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return false, fmt.Errorf("title can't be empty")
	}
	if p.Priority != nil && (*p.Priority < 1 || *p.Priority > 4) {
		return false, fmt.Errorf("priority must be 1–4")
	}
	if p.Due != nil && !ValidDate(strings.TrimSpace(*p.Due)) {
		return false, fmt.Errorf("due must be YYYY-MM-DD: %q", *p.Due)
	}
	if p.Status != nil && !ValidStatus(*p.Status) {
		return false, fmt.Errorf("status must be todo / doing / waiting / done / cancelled")
	}
	set(&t.Title, p.Title, true)
	set(&t.Key, p.Key, true)
	set(&t.Due, p.Due, true)
	set(&t.Project, p.Project, true)
	set(&t.Note, p.Note, false)
	if p.Priority != nil && *p.Priority != t.Priority {
		t.Priority = *p.Priority
		changed = true
	}

	if p.Status != nil && *p.Status != t.Status {
		SetStatus(t, *p.Status, why, src, now)
		changed = true
	}

	if p.Plan != nil {
		plan, err := ParsePlan(*p.Plan, now)
		if err != nil {
			return changed, err
		}
		moved, err := Replan(t, plan, why, p.Auto, src, now)
		if err != nil {
			return changed, err
		}
		changed = changed || moved
	}

	if links := dedup(t.Links, p.AddLinks); len(links) != len(t.Links) {
		t.Links, changed = links, true
	}
	if len(p.RemoveLinks) > 0 {
		kept := slices.DeleteFunc(slices.Clone(t.Links), func(l string) bool { return slices.Contains(p.RemoveLinks, l) })
		if len(kept) != len(t.Links) {
			t.Links, changed = kept, true
		}
	}
	if ss := dedup(t.Sessions, p.AddSessions); len(ss) != len(t.Sessions) {
		t.Sessions, changed = ss, true
	}
	if len(p.RemoveSessions) > 0 {
		kept := slices.DeleteFunc(slices.Clone(t.Sessions), func(s string) bool { return slices.Contains(p.RemoveSessions, s) })
		if len(kept) != len(t.Sessions) {
			t.Sessions, changed = kept, true
		}
	}

	if p.Archived != nil && *p.Archived != t.Archived {
		if *p.Archived && !t.Finished() {
			return changed, fmt.Errorf("#%d is %s: only a done or cancelled task can be archived", t.ID, StatusLabel[t.Status])
		}
		t.Archived = *p.Archived
		changed = true
	}
	return changed, nil
}

// SetStatus changes the status and logs it. Finishing stamps done_on (so
// the list keeps the task in today for the rest of the day); reopening
// clears it and brings the task out of the archive.
func SetStatus(t *model.Task, status, why, source string, now time.Time) {
	before := t.Status
	if before == "" {
		before = model.TaskTodo
	}
	t.Status = status
	if t.Finished() {
		t.DoneOn = DateString(Day(now))
	} else {
		t.DoneOn = ""
		t.Archived = false
	}
	verb := "に変更"
	if source == SourceAuto {
		verb = "に自動で変更"
	}
	text := fmt.Sprintf("状態を「%s」から「%s」%s", StatusLabel[before], StatusLabel[status], verb)
	if why != "" {
		text += "（" + why + "）"
	}
	AddNote(t, model.TaskNote{At: Stamp(now), Source: source, Text: text, Ref: fmt.Sprintf("%s:status:%s", source, Stamp(now))})
}

// Replan moves the plan to plan (a date or ""). An automated replan needs
// a reason and leaves a plan the person set today alone (it returns an
// error saying why). A manual one marks the plan as the person's for
// today. Either way the manual order is dropped: the task joins the end
// of its new bucket.
func Replan(t *model.Task, plan, why string, auto bool, source string, now time.Time) (bool, error) {
	if plan == t.Plan {
		return false, nil
	}
	today := DateString(Day(now))
	if auto {
		if t.Finished() {
			return false, fmt.Errorf("finished task")
		}
		if why == "" {
			return false, fmt.Errorf("no reason given")
		}
		if t.PlanBy == "manual" && t.PlanOn == today {
			return false, fmt.Errorf("the plan was set by hand today")
		}
	}
	before := t.Plan
	t.Plan = plan
	t.Order = nil
	t.PlanOn = today
	t.PlanBy = "manual"
	if auto {
		t.PlanBy = "auto"
		source = SourceAuto
	}
	if auto || why != "" {
		verb := "に変更"
		if auto {
			verb = "に自動で変更"
		}
		text := fmt.Sprintf("やる日を「%s」から「%s」%s", orUnset(before), orUnset(plan), verb)
		if why != "" {
			text += "（" + why + "）"
		}
		AddNote(t, model.TaskNote{At: Stamp(now), Source: source, Text: text, Ref: fmt.Sprintf("%s:plan:%s", source, Stamp(now))})
	}
	return true, nil
}

func orUnset(s string) string {
	if s == "" {
		return "未定"
	}
	return s
}

// AddNote appends n unless its ref is already on the task, keeping the
// timeline in time order. An empty ref gets one derived from source, time
// and text. It reports whether n was added.
func AddNote(t *model.Task, n model.TaskNote) bool {
	n.Text = strings.TrimSpace(n.Text)
	if n.Text == "" || n.At == "" {
		return false
	}
	if n.Source == "" {
		n.Source = SourceClaude
	}
	if n.Ref == "" {
		n.Ref = fmt.Sprintf("%s:%s:%s", n.Source, n.At, clip(n.Text, 20))
	}
	for _, e := range t.Timeline {
		if e.Ref == n.Ref {
			return false
		}
	}
	t.Timeline = append(t.Timeline, n)
	slices.SortStableFunc(t.Timeline, func(a, b model.TaskNote) int { return strings.Compare(a.At, b.At) })
	return true
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func dedup(have, add []string) []string {
	out := slices.Clone(have)
	if out == nil {
		out = []string{}
	}
	for _, v := range add {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
