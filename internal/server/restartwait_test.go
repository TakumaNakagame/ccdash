package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestScheduledRestart: {"when":"idle"} waits while a hosted session works
// or declared a background wait, then restarts (saving both sessions for
// resume) once they are idle; DELETE cancels a schedule.
func TestScheduledRestart(t *testing.T) {
	fakeClaude(t)
	oldEvery, oldQuiet := restartCheckEvery, restartQuiet
	restartCheckEvery, restartQuiet = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { restartCheckEvery, restartQuiet = oldEvery, oldQuiet })

	s, d, tok := newTestServer(t)
	t.Cleanup(s.closeAllPTYs)
	ctx := context.Background()
	var fired atomic.Bool
	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	s.ctx, s.CanRestart = sctx, true
	s.cancelFn = func() { fired.Store(true) }
	dir := t.TempDir()
	for _, x := range []model.Session{
		{SessionID: "busy", Num: 1, Cwd: dir, Status: model.StatusActive},
		{SessionID: "bg", Num: 2, Cwd: dir, Status: model.StatusIdle},
	} {
		if err := d.UpsertSession(ctx, &x); err != nil {
			t.Fatal(err)
		}
		if r := do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{"sessionId":"`+x.SessionID+`","resumeId":"`+x.SessionID+`","cwd":"`+dir+`"}`)); r.status != http.StatusOK {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
	}
	_ = d.SetWait(ctx, "bg", model.WaitBackground, "CI")
	state := func() model.RestartState {
		var st model.RestartState
		r := do(t, s, http.MethodGet, "/api/restart", tok, nil)
		_ = json.Unmarshal(r.body, &st)
		return st
	}

	// Cancel first.
	do(t, s, http.MethodPost, "/api/restart", tok, []byte(`{"when":"idle","by":"test"}`))
	if st := state(); !st.Pending || st.By != "test" || len(st.Waiting) != 2 || st.Hosted != 2 {
		t.Fatalf("pending state = %+v", st)
	}
	do(t, s, http.MethodDelete, "/api/restart", tok, nil)
	if st := state(); st.Pending || st.Last != "cancelled" {
		t.Fatalf("after cancel = %+v", st)
	}

	do(t, s, http.MethodPost, "/api/restart", tok, []byte(`{"when":"idle"}`))
	time.Sleep(200 * time.Millisecond)
	if fired.Load() {
		t.Fatal("restarted while sessions were busy")
	}
	if st := state(); len(st.Waiting) != 2 || st.Waiting[0].Why == "" {
		t.Fatalf("waiting = %+v", st.Waiting)
	}
	_ = d.UpsertSession(ctx, &model.Session{SessionID: "busy", Cwd: dir, Status: model.StatusIdle})
	_ = d.SetWait(ctx, "bg", model.WaitReport, "done")
	deadline := time.Now().Add(3 * time.Second)
	for !fired.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !fired.Load() {
		t.Fatalf("no restart once idle; state = %+v", state())
	}
	p, _ := resumeFilePath()
	var list []resumeEntry
	b, _ := os.ReadFile(p)
	if json.Unmarshal(b, &list) != nil || len(list) != 2 {
		t.Errorf("resume list = %s", b)
	}
	if st := state(); st.Pending {
		t.Error("still pending after the restart")
	}
}

// TestScheduledRestartExpires: a schedule that outwaits its deadline gives up.
func TestScheduledRestartExpires(t *testing.T) {
	fakeClaude(t)
	oldEvery := restartCheckEvery
	restartCheckEvery = 10 * time.Millisecond
	t.Cleanup(func() { restartCheckEvery = oldEvery })
	s, d, tok := newTestServer(t)
	t.Cleanup(s.closeAllPTYs)
	s.CanRestart, s.cancelFn = true, func() { t.Error("restarted") }
	dir := t.TempDir()
	_ = d.UpsertSession(context.Background(), &model.Session{SessionID: "busy", Cwd: dir, Status: model.StatusActive})
	do(t, s, http.MethodPost, "/pty/start", tok, []byte(`{"sessionId":"busy","resumeId":"busy","cwd":"`+dir+`"}`))
	_ = s.scheduleRestart(restartReq{When: "idle"})
	s.restartMu.Lock()
	s.restartSched.deadline = time.Now()
	s.restartMu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if st := s.restartState(context.Background()); st.Pending || st.Last != "expired" {
		t.Errorf("state = %+v", st)
	}
	s.CanRestart = false
	if r := do(t, s, http.MethodPost, "/api/restart", tok, []byte(`{"when":"idle"}`)); r.status != http.StatusNotImplemented {
		t.Errorf("can't-restart collector: %d", r.status)
	}
}
