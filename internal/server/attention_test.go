package server

import (
	"context"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func TestAttentionFromHooks(t *testing.T) {
	s, d, tok := newTestServer(t)
	att := func() (string, string) {
		t.Helper()
		ss, ok, err := d.GetSession(context.Background(), "s1")
		if err != nil || !ok {
			t.Fatalf("get session: %v %v", ok, err)
		}
		return ss.Attention, ss.AttentionReason
	}
	hook := func(path, body string) {
		t.Helper()
		if r := do(t, s, "POST", path, tok, []byte(body)); r.status != 200 {
			t.Fatalf("%s = %d %s", path, r.status, r.body)
		}
	}
	hook("/hooks/user-prompt", `{"session_id":"s1","cwd":"/tmp","prompt":"hi"}`)
	if a, _ := att(); a != "" {
		t.Fatalf("after prompt = %q", a)
	}
	hook("/hooks/pre-tool", `{"session_id":"s1","cwd":"/tmp","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"どっち？"}]}}`)
	if a, r := att(); a != model.AttentionNeedsYou || r != "質問: どっち？" {
		t.Fatalf("after AskUserQuestion = %q %q", a, r)
	}
	hook("/hooks/post-tool", `{"session_id":"s1","cwd":"/tmp","tool_name":"AskUserQuestion"}`)
	if a, _ := att(); a != "" {
		t.Fatalf("after answer = %q", a)
	}
	hook("/hooks/notification", `{"session_id":"s1","cwd":"/tmp","message":"Claude needs your permission to use Bash"}`)
	if a, _ := att(); a != model.AttentionNeedsYou {
		t.Fatalf("after permission notification = %q", a)
	}
	hook("/hooks/stop", `{"session_id":"s1","cwd":"/tmp"}`)
	if a, _ := att(); a != model.AttentionDone {
		t.Fatalf("after stop = %q", a)
	}
	// A plain tool finishing doesn't erase the unread "done".
	hook("/hooks/post-tool", `{"session_id":"s1","cwd":"/tmp","tool_name":"Bash"}`)
	if a, _ := att(); a != model.AttentionDone {
		t.Fatalf("done erased by post-tool: %q", a)
	}
	if r := do(t, s, "POST", "/api/sessions/s1/seen", tok, []byte(`{}`)); r.status != 200 {
		t.Fatalf("seen = %d", r.status)
	}
	if a, _ := att(); a != "" {
		t.Fatalf("after seen = %q", a)
	}
}

func TestAttentionFromDeclaredWait(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	get := func() model.Session {
		t.Helper()
		ss, ok, err := d.GetSession(ctx, "s1")
		if err != nil || !ok {
			t.Fatalf("get session: %v %v", ok, err)
		}
		return ss
	}
	post := func(path, body string, want int) {
		t.Helper()
		if r := do(t, s, "POST", path, tok, []byte(body)); r.status != want {
			t.Fatalf("%s = %d %s", path, r.status, r.body)
		}
	}
	post("/hooks/user-prompt", `{"session_id":"s1","cwd":"/tmp","prompt":"hi"}`, 200)
	post("/api/sessions/s1/wait", `{"wait":"nope"}`, 400)
	post("/api/sessions/s1/wait", `{"wait":"answer","note":"A案とB案どちらにしますか"}`, 200)
	post("/hooks/stop", `{"session_id":"s1","cwd":"/tmp"}`, 200)
	if x := get(); x.Attention != model.AttentionNeedsYou || x.AttentionReason != "回答待ち: A案とB案どちらにしますか" || x.Wait != model.WaitAnswer {
		t.Fatalf("after declared answer = %q %q %q", x.Attention, x.AttentionReason, x.Wait)
	}
	// Looking at it doesn't answer it.
	post("/api/sessions/s1/seen", `{}`, 200)
	if x := get(); x.Attention != model.AttentionNeedsYou {
		t.Fatalf("seen cleared needs you: %q", x.Attention)
	}
	post("/hooks/user-prompt", `{"session_id":"s1","cwd":"/tmp","prompt":"A"}`, 200)
	if x := get(); x.Attention != "" || x.Wait != "" {
		t.Fatalf("after reply = %q %q", x.Attention, x.Wait)
	}
	post("/api/sessions/s1/wait", `{"wait":"report","note":"done"}`, 200)
	post("/hooks/stop", `{"session_id":"s1","cwd":"/tmp"}`, 200)
	if x := get(); x.Attention != model.AttentionDone || x.Wait != model.WaitReport {
		t.Fatalf("after report = %q %q", x.Attention, x.Wait)
	}
	// A turn with no prompt (e.g. a background task woke it) that declares
	// nothing drops the old declaration.
	post("/hooks/stop", `{"session_id":"s1","cwd":"/tmp"}`, 200)
	if x := get(); x.Wait != "" {
		t.Fatalf("stale wait kept: %q", x.Wait)
	}
}
