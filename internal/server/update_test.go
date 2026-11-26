package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/selfupdate"
	"github.com/TakumaNakagame/ccdash/internal/settings"
)

// fakeRelease makes the newest release `tag` and counts installs.
func fakeRelease(t *testing.T, tag string, fail error) *int {
	t.Helper()
	n := 0
	oldRun, oldLatest, oldNow := runSelfUpdate, latestRelease, updateNow
	latestRelease = func(context.Context, selfupdate.Channel) (string, error) { return tag, nil }
	runSelfUpdate = func(_ context.Context, cur string, _ selfupdate.Channel) (selfupdate.Result, error) {
		if fail != nil {
			return selfupdate.Result{}, fail
		}
		if !selfupdate.Newer(cur, tag) {
			return selfupdate.Result{OldVersion: cur, NewVersion: tag, NoOp: true, Reason: "already"}, nil
		}
		n++
		return selfupdate.Result{OldVersion: cur, NewVersion: tag, BinaryPath: "/x/ccdash"}, nil
	}
	t.Cleanup(func() { runSelfUpdate, latestRelease, updateNow = oldRun, oldLatest, oldNow })
	return &n
}

func waitUpdate(t *testing.T, s *Server, tok string) model.UpdateState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var st model.UpdateState
		_ = json.Unmarshal(do(t, s, "GET", "/api/update", tok, nil).body, &st)
		if !st.Running && st.Last != nil {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("update didn't finish: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateButton(t *testing.T) {
	s, _, tok := newTestServer(t)
	n := fakeRelease(t, "v99.0.0", nil)
	s.checkUpdate(context.Background(), selfupdate.ChannelStable, true)
	var st model.UpdateState
	_ = json.Unmarshal(do(t, s, "GET", "/api/update", tok, nil).body, &st)
	if !st.Available || st.Latest != "v99.0.0" || st.Auto != "off" {
		t.Fatalf("state = %+v", st)
	}
	if r := do(t, s, "POST", "/api/update", tok, []byte(`{"channel":"nightly"}`)); r.status != 400 {
		t.Fatalf("bad channel = %d", r.status)
	}
	if r := do(t, s, "POST", "/api/update", tok, []byte(`{}`)); r.status != 200 {
		t.Fatalf("update = %d %s", r.status, r.body)
	}
	st = waitUpdate(t, s, tok)
	if *n != 1 || !st.Last.OK || st.Last.By != "portal" || st.Installed != "v99.0.0" || st.Available {
		t.Fatalf("after update: n=%d %+v %+v", *n, st, st.Last)
	}
	// A test server can't restart itself: the operator is told.
	if !st.RestartNeeded {
		t.Fatal("restart_needed not set for a collector that can't restart")
	}
	// Pressing again installs nothing more.
	do(t, s, "POST", "/api/update", tok, []byte(`{}`))
	waitUpdate(t, s, tok)
	if *n != 1 {
		t.Fatalf("installed again: %d", *n)
	}
}

func TestUpdateFails(t *testing.T) {
	s, _, tok := newTestServer(t)
	fakeRelease(t, "v99.0.0", errors.New("github api: 403 rate limited"))
	do(t, s, "POST", "/api/update", tok, []byte(`{}`))
	st := waitUpdate(t, s, tok)
	if st.Last.OK || st.Last.Error == "" || st.Installed != "" {
		t.Fatalf("failed update = %+v %+v", st, st.Last)
	}
}

func TestAutoUpdateNightly(t *testing.T) {
	s, d, _ := newTestServer(t)
	ctx := context.Background()
	n := fakeRelease(t, "v99.0.0", nil)
	at := func(h int) { updateNow = func() time.Time { return time.Date(2026, 11, 26, h, 10, 0, 0, time.Local) } }

	at(3)
	s.updateTick(ctx) // auto_update off
	if *n != 0 {
		t.Fatal("updated with auto_update off")
	}
	cfg, _ := settings.Load(ctx, d)
	if _, err := settings.Set(ctx, d, cfg, "auto_update", "dev"); err != nil {
		t.Fatal(err)
	}
	at(14)
	s.updateTick(ctx) // daytime
	if *n != 0 {
		t.Fatal("updated in the daytime")
	}
	at(3)
	s.updateTick(ctx)
	if *n != 1 {
		t.Fatalf("nightly update ran %d times", *n)
	}
	s.upd.mu.Lock()
	s.upd.installed = "" // as if the restart into it failed
	s.upd.mu.Unlock()
	s.updateTick(ctx) // same night: no second try
	if *n != 1 {
		t.Fatalf("second try the same night: %d", *n)
	}
}
