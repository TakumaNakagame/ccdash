package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// fakeClaude swaps the PTY child for `cat` and records each spawn's args.
func fakeClaude(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	old := newPTYCommand
	newPTYCommand = func(args ...string) *exec.Cmd {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		return exec.Command("sh", "-c", "cat")
	}
	t.Cleanup(func() { newPTYCommand = old })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), calls...)
	}
}

// TestPTYRestart: restarting a hosted session kills its claude and resumes
// the same session in a new PTY under the same key; a PTY without a
// session yet is refused.
func TestPTYRestart(t *testing.T) {
	calls := fakeClaude(t)
	s, d, tok := newTestServer(t)
	t.Cleanup(s.closeAllPTYs)
	dir := t.TempDir()
	if err := d.UpsertSession(context.Background(), &model.Session{SessionID: "sess-r", Cwd: dir, Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	if r := do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{"sessionId":"sess-r","resumeId":"sess-r","cwd":"`+dir+`"}`)); r.status != http.StatusOK {
		t.Fatalf("start: %d %s", r.status, r.body)
	}
	s.ptyMu.Lock()
	before := s.ptyMap["sess-r"]
	s.ptyMu.Unlock()

	r := do(t, s, http.MethodPost, "/pty/sess-r/restart", tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("restart: %d %s", r.status, r.body)
	}
	s.ptyMu.Lock()
	after := s.ptyMap["sess-r"]
	s.ptyMu.Unlock()
	if after == nil || after == before || !after.sess.Alive() {
		t.Fatal("restart did not put a new alive PTY under the session key")
	}
	if before.sess.Alive() {
		t.Error("old claude still alive")
	}
	got := calls()
	if len(got) != 2 || strings.Join(got[1], " ") != "--resume sess-r" {
		t.Errorf("spawns = %v", got)
	}

	// A fresh spawn with no session yet: nothing to resume.
	r = do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{}`))
	var started struct{ PtyKey string }
	_ = json.Unmarshal(r.body, &started)
	if r := do(t, s, http.MethodPost, "/pty/"+started.PtyKey+"/restart", tok, nil); r.status != http.StatusConflict {
		t.Errorf("restart of unregistered pty = %d, want 409", r.status)
	}
	if r := do(t, s, http.MethodPost, "/pty/nope/restart", tok, nil); r.status != http.StatusNotFound {
		t.Errorf("restart of unknown pty = %d, want 404", r.status)
	}
}

// TestResumeRoundTrip: /shutdown?resume=1 saves the hosted sessions, and
// the next start resumes them once; a stale file is ignored.
func TestResumeRoundTrip(t *testing.T) {
	calls := fakeClaude(t)
	s, d, tok := newTestServer(t)
	t.Cleanup(s.closeAllPTYs)
	dir := t.TempDir()
	_ = d.UpsertSession(context.Background(), &model.Session{SessionID: "sess-a", Cwd: dir, Status: model.StatusIdle})
	do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{"sessionId":"sess-a","resumeId":"sess-a","cwd":"`+dir+`","cols":100,"rows":30}`))
	do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{}`)) // no session: not saved

	if r := do(t, s, http.MethodPost, "/shutdown?resume=1", tok, nil); r.status != http.StatusNoContent {
		t.Fatalf("shutdown: %d", r.status)
	}
	p, _ := resumeFilePath()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var list []resumeEntry
	_ = json.Unmarshal(b, &list)
	if len(list) != 1 || list[0].SessionID != "sess-a" || list[0].Cwd != dir || list[0].Cols != 100 {
		t.Fatalf("saved = %+v", list)
	}

	s.closeAllPTYs()
	n := len(calls())
	s.resumeSaved()
	if got := calls(); len(got) != n+1 || strings.Join(got[n], " ") != "--resume sess-a" {
		t.Fatalf("resume spawns = %v", got[n:])
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("resume file not consumed")
	}

	// Stale: written long ago → ignored (and removed).
	_ = os.WriteFile(p, b, 0o600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	n = len(calls())
	s.resumeSaved()
	if len(calls()) != n {
		t.Error("stale resume file was honored")
	}
}

// TestRestartNeedsCanRestart: without CanRestart (no re-exec available)
// POST /api/restart is 501 and saves nothing.
func TestRestartNeedsCanRestart(t *testing.T) {
	s, _, tok := newTestServer(t)
	if r := do(t, s, http.MethodPost, "/api/restart", tok, nil); r.status != http.StatusNotImplemented {
		t.Errorf("restart = %d, want 501", r.status)
	}
}
