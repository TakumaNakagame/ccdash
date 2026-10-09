package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
)

func TestServe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tp := filepath.Join(dir, "a.jsonl")
	_ = os.WriteFile(tp, []byte(
		`{"type":"user","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"add a login page"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-09-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Done: login.tsx"}]}}`+"\n"), 0o600)
	for _, s := range []model.Session{
		{SessionID: "a", Cwd: "/w/web", Title: "login page", TranscriptPath: tp, Status: model.StatusIdle},
		{SessionID: "b", Cwd: "/w/infra", Title: "k8s", Status: model.StatusStopped, LastSeen: time.Now().Add(-time.Hour)},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetProject(ctx, "a", "web")
	_ = d.SetAttention(ctx, "a", model.AttentionNeedsYou, "question")

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sessions","arguments":{"project":"web"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_session","arguments":{"session":"#1"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"overview"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":8,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := New(store.NewLocal(d), "test", false).Serve(ctx, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 8 { // the notification gets no reply
		t.Fatalf("got %d replies:\n%s", len(lines), out.String())
	}
	type reply struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Tools           []struct{ Name string }
			IsError         bool `json:"isError"`
			Content         []struct{ Text string }
		} `json:"result"`
		Error *struct{ Code int } `json:"error"`
	}
	r := make([]reply, len(lines))
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &r[i]); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if r[0].Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize: %s", lines[0])
	}
	if len(r[1].Result.Tools) != 9 {
		t.Errorf("tools/list: %s", lines[1])
	}
	body := func(i int) string { return r[i].Result.Content[0].Text }
	if !strings.Contains(body(2), `"name": "web"`) || !strings.Contains(body(2), `"needs_you": 1`) {
		t.Errorf("list_projects: %s", body(2))
	}
	if !strings.Contains(body(3), `"matched": 1`) || !strings.Contains(body(3), "login page") {
		t.Errorf("list_sessions: %s", body(3))
	}
	if !strings.Contains(body(4), "add a login page") || !strings.Contains(body(4), "Done: login.tsx") {
		t.Errorf("get_session: %s", body(4))
	}
	if !strings.Contains(body(5), `"attention_reason": "question"`) {
		t.Errorf("overview: %s", body(5))
	}
	if !r[6].Result.IsError {
		t.Errorf("unknown tool: %s", lines[6])
	}
	if r[7].Error == nil || r[7].Error.Code != -32601 {
		t.Errorf("unknown method: %s", lines[7])
	}
}

func TestOrganize(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/w", Status: model.StatusStopped}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(store.NewLocal(d), "test", false)
	call := func(name, args string) (string, error) { return srv.call(ctx, name, json.RawMessage(args)) }
	if _, err := call("set_project", `{"sessions":["#1","2"],"project":"web"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_project", `{"sessions":["c"],"project":"api"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_project", `{"sessions":["#1","#99"],"project":"x"}`); err == nil {
		t.Fatal("unknown session accepted")
	}
	if out, err := call("rename_project", `{"from":"api","to":"web"}`); err != nil || !strings.Contains(out, "merged") {
		t.Fatalf("merge: %s %v", out, err)
	}
	if c, _, _ := d.GetSession(ctx, "c"); c.Project != "web" {
		t.Fatalf("c project %q", c.Project)
	}
	if _, err := call("set_title", `{"session":"#3","title":"API work"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_archived", `{"sessions":["#2"],"archived":true}`); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := d.GetSession(ctx, "b"); !b.Archived {
		t.Fatal("not archived")
	}
	if c, _, _ := d.GetSession(ctx, "c"); c.CustomTitle != "API work" {
		t.Fatalf("title %q", c.CustomTitle)
	}
	ro := New(store.NewLocal(d), "test", true)
	if _, err := ro.call(ctx, "set_project", json.RawMessage(`{"sessions":["#1"],"project":"z"}`)); err == nil {
		t.Fatal("read-only server accepted a write")
	}
	if len(ro.tools()) != 4 {
		t.Fatalf("read-only tools: %d", len(ro.tools()))
	}
}
