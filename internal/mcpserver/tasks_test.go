package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
)

// TestTaskTools: add / note / status / list through the tools, the calling
// session linked on every edit, session rows naming their tasks, and
// --read-only refusing writes.
func TestTaskTools(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "sess-a", Cwd: "/a", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	s := New(store.NewLocal(d), "test", false)
	s.self = func() string { return "sess-a" }
	call := func(name, args string) string {
		t.Helper()
		out, err := s.call(ctx, name, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	call("add_task", `{"title":"PR を見る","key":"https://github.com/o/r/pull/1","plan":"today","project":"web"}`)
	call("add_task_note", `{"task":"#1","text":"レビューした"}`)
	out := call("set_task_status", `{"task":"https://github.com/o/r/pull/1","status":"waiting","why":"返事待ち"}`)
	if !strings.Contains(out, `"status": "waiting"`) || !strings.Contains(out, `"ref": "#1"`) {
		t.Fatalf("set_task_status: %s", out)
	}
	out = call("list_tasks", `{"bucket":"today"}`)
	if !strings.Contains(out, `"matched": 1`) || !strings.Contains(out, "返事待ち") {
		t.Fatalf("list_tasks: %s", out)
	}
	if out = call("list_sessions", `{}`); !strings.Contains(out, `"tasks": [`) {
		t.Fatalf("list_sessions lacks the task link: %s", out)
	}
	if _, err := s.call(ctx, "archive_task", json.RawMessage(`{"task":"1"}`)); err == nil {
		t.Fatal("archiving a waiting task must fail")
	}
	out = call("sync_tasks", `{"synced_at":"2026-10-13T12:00:00+09:00","tasks":[{"key":"K-1","title":"from slack","project":"nope"}]}`)
	if !strings.Contains(out, "from slack") || !strings.Contains(out, "nope") {
		t.Fatalf("sync_tasks: %s", out)
	}
	if out = call("task_context", `{}`); !strings.Contains(out, `"since": "2026-10-13T12:00:00+09:00"`) {
		t.Fatalf("task_context: %s", out)
	}

	ro := New(store.NewLocal(d), "test", true)
	if _, err := ro.call(ctx, "add_task", json.RawMessage(`{"title":"x"}`)); err == nil {
		t.Fatal("read-only server added a task")
	}
	if _, err := ro.call(ctx, "list_tasks", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}
