package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// at is a local time on the given date (2026-10-13 is a Tuesday).
func at(date string, hm ...int) time.Time {
	d, err := time.ParseInLocation(dateLayout, date, time.Local)
	if err != nil {
		panic(err)
	}
	if len(hm) == 2 {
		d = d.Add(time.Duration(hm[0])*time.Hour + time.Duration(hm[1])*time.Minute)
	}
	return d
}

func TestParsePlan(t *testing.T) {
	cases := []struct{ now, in, want string }{
		{"2026-10-13", "today", "2026-10-13"},
		{"2026-10-13", "tomorrow", "2026-10-14"},
		{"2026-10-16", "tomorrow", "2026-10-19"}, // Friday → Monday
		{"2026-10-17", "tomorrow", "2026-10-19"}, // Saturday → Monday
		{"2026-10-13", "week", "2026-10-16"},
		{"2026-10-13", "nextweek", "2026-10-19"},
		{"2026-10-18", "nextweek", "2026-10-19"}, // Sunday: tomorrow's Monday
		{"2026-10-13", "none", ""},
		{"2026-10-13", "2026-11-02", "2026-11-02"},
	}
	for _, c := range cases {
		got, err := ParsePlan(c.in, at(c.now, 9, 0))
		if err != nil || got != c.want {
			t.Errorf("ParsePlan(%q) on %s = %q, %v; want %q", c.in, c.now, got, err, c.want)
		}
	}
	if _, err := ParsePlan("week", at("2026-10-17")); err == nil {
		t.Error("week on a Saturday should fail: this Friday has passed")
	}
	if _, err := ParsePlan("someday", at("2026-10-13")); err == nil {
		t.Error("garbage should fail")
	}
}

func TestBucketOf(t *testing.T) {
	now := at("2026-10-13", 10, 0) // Tue
	cases := []struct {
		task model.Task
		want string
	}{
		{model.Task{Status: "todo", Plan: "2026-10-01"}, BucketToday}, // carried over
		{model.Task{Status: "todo", Plan: "2026-10-13"}, BucketToday},
		{model.Task{Status: "todo", Plan: "2026-10-14"}, BucketTomorrow},
		{model.Task{Status: "todo", Plan: "2026-10-16"}, BucketWeek},
		{model.Task{Status: "todo", Plan: "2026-10-19"}, BucketNextWeek},
		{model.Task{Status: "todo", Plan: "2026-10-26"}, BucketLater},
		{model.Task{Status: "todo"}, BucketNone},
		{model.Task{Status: "done", DoneOn: "2026-10-13"}, BucketToday},
		{model.Task{Status: "cancelled", DoneOn: "2026-10-12"}, BucketFinished},
	}
	for _, c := range cases {
		if got := BucketOf(c.task, now); got != c.want {
			t.Errorf("BucketOf(%+v) = %s, want %s", c.task, got, c.want)
		}
	}
	// On Friday, Monday is "tomorrow", not next week.
	if got := BucketOf(model.Task{Status: "todo", Plan: "2026-10-19"}, at("2026-10-16")); got != BucketTomorrow {
		t.Errorf("Friday → Monday = %s, want tomorrow", got)
	}
}

func TestHeading(t *testing.T) {
	now := at("2026-10-13")
	if got := Heading(BucketToday, now); got != "今日（10/13 火）" {
		t.Errorf("today = %q", got)
	}
	if got := Heading(BucketNextWeek, at("2026-10-16")); got != "来週（10/20 火〜10/23 金）" {
		t.Errorf("next week on Friday = %q", got)
	}
	if got := Heading(BucketNone, now); got != "未定" {
		t.Errorf("none = %q", got)
	}
}

func TestSortOrder(t *testing.T) {
	one := 1.0
	ts := []model.Task{
		{ID: 1, Status: "todo", Priority: 3, Plan: "2026-10-13"},
		{ID: 2, Status: "todo", Priority: 1, Plan: "2026-10-13"},
		{ID: 3, Status: "todo", Priority: 4, Plan: "2026-10-13", Order: &one},
		{ID: 4, Status: "todo", Priority: 1},
		{ID: 5, Status: "todo", Priority: 2, Plan: "2026-10-12"},
	}
	Sort(ts, at("2026-10-13"))
	var ids []int64
	for _, x := range ts {
		ids = append(ids, x.ID)
	}
	// manual order first, then the earlier plan (carried over), priority; unplanned last.
	want := []int64{3, 5, 2, 1, 4}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
}

func TestApplyStatusArchive(t *testing.T) {
	now := at("2026-10-13", 11, 0)
	task := model.Task{ID: 7, Title: "x", Status: "todo", Priority: 3}
	yes := true
	if _, err := Apply(&task, Patch{Archived: &yes}, now); err == nil {
		t.Fatal("archiving an open task must fail")
	}
	st := model.TaskCancelled
	if _, err := Apply(&task, Patch{Status: &st, Why: "不要になった", Archived: &yes, Source: SourceManual}, now); err != nil {
		t.Fatal(err)
	}
	if !task.Archived || task.DoneOn != "2026-10-13" {
		t.Fatalf("got %+v", task)
	}
	if n := len(task.Timeline); n != 1 || !strings.Contains(task.Timeline[0].Text, "「未着手」から「中止」") || !strings.Contains(task.Timeline[0].Text, "不要になった") {
		t.Fatalf("timeline = %+v", task.Timeline)
	}
	// Reopening brings it out of the archive.
	st = model.TaskDoing
	if _, err := Apply(&task, Patch{Status: &st}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if task.Archived || task.DoneOn != "" {
		t.Fatalf("reopened = %+v", task)
	}
}

func TestReplanGuards(t *testing.T) {
	now := at("2026-10-13", 9, 0)
	task := model.Task{Status: "todo", Plan: "2026-10-14", PlanBy: "manual", PlanOn: "2026-10-13"}
	if _, err := Replan(&task, "2026-10-20", "MTG moved", true, SourceAuto, now); err == nil {
		t.Error("an automated replan must not move a plan set by hand today")
	}
	task.PlanOn = "2026-10-12"
	if _, err := Replan(&task, "2026-10-20", "", true, SourceAuto, now); err == nil {
		t.Error("an automated replan needs a reason")
	}
	one := 1.0
	task.Order = &one
	moved, err := Replan(&task, "2026-10-20", "MTG moved", true, SourceAuto, now)
	if err != nil || !moved || task.PlanBy != "auto" || task.Order != nil {
		t.Fatalf("replan: %v %v %+v", moved, err, task)
	}
	if !strings.Contains(task.Timeline[0].Text, "自動で変更（MTG moved）") {
		t.Errorf("timeline = %q", task.Timeline[0].Text)
	}
}

func TestMerge(t *testing.T) {
	now := at("2026-10-13", 12, 0)
	existing := &model.Task{ID: 1, Key: "ESRE-1", Title: "keep me", Priority: 1, Status: "todo", Note: "mine",
		Plan: "2026-10-13", PlanBy: "manual", PlanOn: "2026-10-13", Links: []string{"a"}}
	done := &model.Task{ID: 2, Key: "ESRE-2", Title: "finished", Status: "done", DoneOn: "2026-10-10"}
	p := Proposal{Tasks: []ProposalTask{
		{Key: "ESRE-1", Title: "renamed", Priority: 4, Note: "theirs", Status: "doing",
			Links:  []string{"a", "b"},
			Replan: &ProposalReplan{Plan: "2026-10-20", Why: "later"}},
		{Key: "ESRE-2", Title: "finished", Status: "doing",
			Timeline: []model.TaskNote{{At: "2026-10-13T10:00:00+09:00", Source: "slack", Text: "ping", Ref: "slack:C1:1"}}},
		{Key: "NEW-1", Title: "new one", Project: "nope", Plan: "today",
			Timeline: []model.TaskNote{{At: "2026-10-13T09:00:00+09:00", Source: "slack", Text: "asked", Ref: "slack:C1:2"}}},
		{Key: "NEW-2", Title: "already done", Done: true},
	}}
	changed, res := Merge([]*model.Task{existing, done}, p, func(string) bool { return false }, now)

	if existing.Title != "keep me" || existing.Priority != 1 || existing.Note != "mine" {
		t.Errorf("person's fields overwritten: %+v", existing)
	}
	if existing.Status != "todo" {
		t.Errorf("status changed without new timeline entries: %s", existing.Status)
	}
	if existing.Plan != "2026-10-13" || len(res.Skipped) != 1 {
		t.Errorf("plan set by hand today moved: %s, skipped %v", existing.Plan, res.Skipped)
	}
	if len(existing.Links) != 2 {
		t.Errorf("links = %v", existing.Links)
	}
	if done.Status != "done" {
		t.Errorf("finished task reopened")
	}
	if len(res.Added) != 1 || len(res.Warnings) != 1 {
		t.Errorf("added %v warnings %v", res.Added, res.Warnings)
	}
	var nt *model.Task
	for _, c := range changed {
		if c.Key == "NEW-1" {
			nt = c
		}
	}
	if nt == nil || nt.ID != 0 || nt.Project != "" || nt.Plan != "2026-10-13" || nt.PlanBy != "auto" || nt.Source != SourceAuto || len(nt.Timeline) != 1 {
		t.Fatalf("new task = %+v", nt)
	}

	// The same run again changes nothing new (refs dedup).
	_, res2 := Merge([]*model.Task{existing, done, nt}, p, nil, now)
	if len(res2.Added)+len(res2.Notes) != 0 {
		t.Errorf("second merge: %+v", res2)
	}

	// A status other than done needs new evidence in the same run.
	existing.PlanOn = "2026-10-12"
	p2 := Proposal{Tasks: []ProposalTask{{Key: "ESRE-1", Title: "x", Status: "waiting",
		Timeline: []model.TaskNote{{At: "2026-10-13T11:00:00+09:00", Source: "jira", Text: "blocked", Ref: "jira:1"}}}}}
	_, res3 := Merge([]*model.Task{existing}, p2, nil, now)
	if existing.Status != "waiting" || len(res3.Status) != 1 {
		t.Errorf("status with evidence: %s %+v", existing.Status, res3)
	}
}

func TestBuildContext(t *testing.T) {
	now := at("2026-10-13", 12, 0)
	ts := []model.Task{
		{ID: 1, Key: "ESRE-12292", Title: "x", Status: "todo", PlanBy: "manual", PlanOn: "2026-10-13",
			Links: []string{
				"https://freee.slack.com/archives/C0123/p1760000000123456?thread_ts=1759999999.000100&cid=C0123",
				"https://github.com/C-FO/repo/pull/12",
			},
			Timeline: []model.TaskNote{{At: "2026-10-12T09:00:00+09:00", Ref: "r1"}}},
		{ID: 2, Title: "done", Status: "done"},
	}
	c := BuildContext(ts, "", now)
	if len(c.Tasks) != 1 {
		t.Fatalf("tasks = %+v", c.Tasks)
	}
	ct := c.Tasks[0]
	if !ct.PlanFixedToday || ct.LastActivity != "2026-10-12" || len(ct.KnownRefs) != 1 {
		t.Errorf("ct = %+v", ct)
	}
	if len(ct.SlackThreads) != 1 || ct.SlackThreads[0].ThreadTS != "1759999999.000100" {
		t.Errorf("slack = %+v", ct.SlackThreads)
	}
	if len(ct.Jira) != 1 || ct.Jira[0] != "ESRE-12292" || len(ct.PRs) != 1 {
		t.Errorf("jira %v prs %v", ct.Jira, ct.PRs)
	}
}

func TestPlanImport(t *testing.T) {
	f := LegacyFile{NextID: 94, LastTimelineSync: "2026-10-05T18:37:31+09:00", Tasks: []model.Task{
		{ID: 1, Title: "a", Status: "todo", Project: "Alert", Priority: 2},
		{ID: 2, Title: "b", Status: "done", DoneOn: "2026-10-01", Project: "ESRE-1"},
		{ID: 3, Title: "c", Status: "done", DoneOn: "2026-10-13", Project: "ccdash"},
		{ID: 4, Title: "d", Status: "todo", Project: "misc"},
		{ID: 5, Title: "e", Status: "todo", Project: "misc"},
	}}
	exists := func(n string) bool { return n == "ccdash" || n == "Alert D4U" }
	now := at("2026-10-13", 12, 0)
	_, err := PlanImport(f, ImportOptions{Map: map[string]string{"Alert": "Alert D4U", "ESRE-1": ""}}, exists, now)
	if err == nil || !strings.Contains(err.Error(), `["misc"]`) {
		t.Fatalf("unmapped project not reported once: %v", err)
	}
	p, err := PlanImport(f, ImportOptions{Map: map[string]string{"Alert": "Alert D4U", "ESRE-1": ""}, Unmapped: "create"}, exists, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Archived != 1 || p.NextID != 94 || len(p.Create) != 1 || p.Create[0] != "misc" {
		t.Errorf("plan = %+v", p)
	}
	got := map[int64]string{}
	for _, x := range p.Converted {
		got[x.ID] = x.Project
	}
	if got[1] != "Alert D4U" || got[2] != "" || got[3] != "ccdash" || got[4] != "misc" {
		t.Errorf("projects = %v", got)
	}
}

func TestMarkdown(t *testing.T) {
	now := at("2026-10-13", 12, 0)
	md := Markdown([]model.Task{
		{ID: 65, Title: "check", Status: "todo", Priority: 1, Plan: "2026-10-06", Due: "2026-10-06",
			Timeline: []model.TaskNote{{At: "2026-10-13T11:15:00+09:00", Text: "looked"}}},
		{ID: 9, Title: "gone", Status: "cancelled", DoneOn: "2026-10-01"},
		{ID: 8, Title: "old", Status: "done", DoneOn: "2026-10-01", Archived: true},
	}, now)
	for _, want := range []string{"## 今日（10/13 火）", "- [ ] #65 check（P1 / 10/06 から持ち越し / 期限 2026-10-06）", "  - 最新 10-13 11:15: looked", "- [-] #9 gone"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "old") {
		t.Error("archived task in markdown")
	}
}
