package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/tasks"
)

func ptr[T any](v T) *T { return &v }

// TestTaskLifecycle: add (project created), note, status + archive, the
// rev moving with each change, lookup by key, and ids never reused.
func TestTaskLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	a, err := d.AddTask(ctx, tasks.Input{Key: "ESRE-1", Title: "first", Project: "web", Plan: "today", Links: []string{"https://x"}, Sessions: []string{"s1"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || a.Plan == "" || a.PlanBy != "manual" || len(a.Sessions) != 1 {
		t.Fatalf("added = %+v", a)
	}
	ps, _ := d.ListProjects(ctx)
	if len(ps) != 1 || ps[0].Name != "web" || ps[0].Tasks != 1 {
		t.Fatalf("projects = %+v", ps)
	}
	rev0, _ := d.TasksRev(ctx)

	if _, err := d.AddTask(ctx, tasks.Input{Key: "ESRE-1", Title: "dup"}); err == nil {
		t.Fatal("a second task with the same key must fail")
	}
	if _, err := d.AddTaskNote(ctx, "ESRE-1", model.TaskNote{Text: "progress", Source: "claude", Ref: "r1"}); err != nil {
		t.Fatal(err)
	}
	// Same ref again: no change, no rev bump.
	rev1, _ := d.TasksRev(ctx)
	if _, err := d.AddTaskNote(ctx, "#1", model.TaskNote{Text: "progress", Source: "claude", Ref: "r1"}); err != nil {
		t.Fatal(err)
	}
	if rev2, _ := d.TasksRev(ctx); rev2 != rev1 || rev1 <= rev0 {
		t.Fatalf("rev %d → %d → %d", rev0, rev1, rev2)
	}

	if _, err := d.UpdateTask(ctx, "1", tasks.Patch{Archived: ptr(true)}); err == nil {
		t.Fatal("archiving an open task must fail")
	}
	got, err := d.UpdateTask(ctx, "1", tasks.Patch{Status: ptr(model.TaskDone), Archived: ptr(true), Why: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived || got.Status != "done" || len(got.Timeline) != 2 {
		t.Fatalf("done+archived = %+v", got)
	}
	if ts, _ := d.ListTasks(ctx, false); len(ts) != 0 {
		t.Fatalf("archived task still listed: %+v", ts)
	}
	if ts, _ := d.ListTasks(ctx, true); len(ts) != 1 {
		t.Fatalf("archive view = %+v", ts)
	}
	if ps, _ := d.ListProjects(ctx); ps[0].Tasks != 0 {
		t.Fatalf("finished task still counted: %+v", ps)
	}
	if _, err := d.GetTask(ctx, "#99"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestTaskRenameProject(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if _, err := d.AddTask(ctx, tasks.Input{Title: "x", Project: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameProject(ctx, "old", "new"); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetTask(ctx, "1")
	if got.Project != "new" {
		t.Fatalf("project = %q", got.Project)
	}
}

func TestTaskSyncAndOrder(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if _, err := d.AddTask(ctx, tasks.Input{Title: "manual", Plan: "today"}); err != nil {
		t.Fatal(err)
	}
	res, err := d.SyncTasks(ctx, tasks.Proposal{SyncedAt: "2026-10-13T12:00:00+09:00", Tasks: []tasks.ProposalTask{
		{Key: "K-1", Title: "from slack", Plan: "today", Timeline: []model.TaskNote{{At: "2026-10-13T10:00:00+09:00", Source: "slack", Text: "asked", Ref: "slack:1"}}},
	}})
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if v, _ := d.LastTaskSync(ctx); v != "2026-10-13T12:00:00+09:00" {
		t.Fatalf("last sync = %q", v)
	}
	if err := d.OrderTasks(ctx, []int64{2, 1}); err != nil {
		t.Fatal(err)
	}
	ts, _ := d.ListTasks(ctx, false)
	tasks.Sort(ts, ts[0].UpdatedAt.Local())
	if len(ts) != 2 || ts[0].ID != 2 || ts[0].Timeline[0].Ref != "slack:1" {
		t.Fatalf("ordered = %+v", ts)
	}
	if err := d.OrderTasks(ctx, []int64{42}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestTaskImport(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	f := tasks.LegacyFile{NextID: 50, LastTimelineSync: "2026-10-05T18:37:31+09:00", Tasks: []model.Task{
		{ID: 11, Key: "A-1", Title: "a", Status: "todo", Priority: 1, Project: "Alert",
			Timeline: []model.TaskNote{{At: "2026-10-01T09:00:00+09:00", Source: "slack", Text: "x", Ref: "slack:1"}}},
		{ID: 12, Title: "b", Status: "done", DoneOn: "2020-01-01"},
	}}
	opt := tasks.ImportOptions{Map: map[string]string{"Alert": "Alert D4U"}}
	plan, err := d.ImportTasks(ctx, f, opt, true)
	if err != nil || plan.Tasks != 2 {
		t.Fatalf("dry run: %+v %v", plan, err)
	}
	if ts, _ := d.ListTasks(ctx, true); len(ts) != 0 {
		t.Fatal("dry run wrote tasks")
	}
	if _, err := d.ImportTasks(ctx, f, opt, false); err != nil {
		t.Fatal(err)
	}
	a, err := d.GetTask(ctx, "11")
	if err != nil || a.Project != "Alert D4U" || len(a.Timeline) != 1 {
		t.Fatalf("imported = %+v %v", a, err)
	}
	if b, _ := d.GetTask(ctx, "12"); !b.Archived {
		t.Fatalf("old done task not archived: %+v", b)
	}
	n, err := d.AddTask(ctx, tasks.Input{Title: "next"})
	if err != nil || n.ID != 50 {
		t.Fatalf("next id = %d %v", n.ID, err)
	}
	if v, _ := d.LastTaskSync(ctx); v != f.LastTimelineSync {
		t.Fatalf("last sync = %q", v)
	}
	if _, err := d.ImportTasks(ctx, f, opt, false); err == nil {
		t.Fatal("importing into a non-empty list must fail")
	}
}

func TestTaskSyncDryRun(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	rev0, _ := d.TasksRev(ctx)
	res, err := d.SyncTasks(ctx, tasks.Proposal{DryRun: true, SyncedAt: "2026-10-13T12:00:00+09:00",
		Tasks: []tasks.ProposalTask{{Key: "K-1", Title: "x"}}})
	if err != nil || len(res.Added) != 1 || !res.DryRun || !strings.Contains(res.String(), "（dry-run）") {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	if ts, _ := d.ListTasks(ctx, true); len(ts) != 0 {
		t.Fatal("dry run saved a task")
	}
	if v, _ := d.LastTaskSync(ctx); v != "" {
		t.Fatalf("dry run moved synced_at: %q", v)
	}
	if rev, _ := d.TasksRev(ctx); rev != rev0 {
		t.Fatal("dry run bumped the rev")
	}
}
