package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

// TestTaskRoutesThroughRemote drives the task routes with store.Remote,
// so the client and the handlers are checked against each other.
func TestTaskRoutesThroughRemote(t *testing.T) {
	s, _, tok := newTestServer(t)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	r := store.NewRemote(ts.URL, tok)
	ctx := context.Background()

	key := "https://github.com/o/r/pull/1" // a key with slashes goes through the path
	a, err := r.AddTask(ctx, tasks.Input{Key: key, Title: "review", Plan: "today", Project: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || a.Source != tasks.SourceManual {
		t.Fatalf("added = %+v", a)
	}
	if got, err := r.GetTask(ctx, key); err != nil || got.ID != 1 || got.Bucket != tasks.BucketToday {
		t.Fatalf("get by key = %+v %v", got, err)
	}
	if _, err := r.AddTaskNote(ctx, "#1", model.TaskNote{Text: "looked", Ref: "x"}); err != nil {
		t.Fatal(err)
	}
	st := model.TaskDone
	if _, err := r.UpdateTask(ctx, "1", tasks.Patch{Status: &st}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := r.UpdateTask(ctx, "1", tasks.Patch{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	l, err := r.ListTasks(ctx, false)
	if err != nil || len(l.Tasks) != 0 || l.Rev == 0 || len(l.Buckets) == 0 {
		t.Fatalf("list = %+v %v", l, err)
	}
	if l, _ = r.ListTasks(ctx, true); len(l.Tasks) != 1 || len(l.Tasks[0].Timeline) != 2 {
		t.Fatalf("archive list = %+v", l)
	}

	res, err := r.SyncTasks(ctx, tasks.Proposal{Tasks: []tasks.ProposalTask{{Key: "K-2", Title: "from jira"}}})
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("sync = %+v %v", res, err)
	}
	if c, err := r.TaskContext(ctx); err != nil || len(c.Tasks) != 1 || c.Tasks[0].Key != "K-2" || len(c.Projects) != 1 {
		t.Fatalf("context = %+v %v", c, err)
	}
	if err := r.OrderTasks(ctx, []int64{2}); err != nil {
		t.Fatal(err)
	}

	// Rule violations are 400s with the reason; unknown tasks 404.
	no := 9
	if _, err := r.UpdateTask(ctx, "2", tasks.Patch{Priority: &no}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("bad priority: %v", err)
	}
	if _, err := r.GetTask(ctx, "99"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.ImportTasks(ctx, tasks.LegacyFile{Tasks: []model.Task{{ID: 5, Title: "x", Status: "todo"}}}, tasks.ImportOptions{}, false); err == nil {
		t.Fatal("import into a non-empty list must fail")
	}
}
