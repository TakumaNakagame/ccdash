package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/takumanakagame/ccmanage/internal/attach"
	"github.com/takumanakagame/ccmanage/internal/procmap"
	"github.com/takumanakagame/ccmanage/internal/screen"
)

// startEntry spawns a shell one-liner inside a server-side emulator and
// waits for the child to exit so the screen is settled.
func startEntry(t *testing.T, script string, cols, rows int) *ptyEntry {
	t.Helper()
	sess := attach.New(exec.Command("sh", "-c", script))
	e := newPTYEntry(sess, "test", cols, rows)
	if err := sess.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.startPumps()
	t.Cleanup(func() { _ = sess.Close() })
	select {
	case <-sess.ChildExit():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	// The exit watcher runs on its own goroutine; give it a beat.
	deadline := time.Now().Add(2 * time.Second)
	for !e.exited.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return e
}

func TestSnapshotWideCharsFillCells(t *testing.T) {
	e := startEntry(t, `printf 'こんにちは世界\nab\n'`, 20, 4)
	rows, w, h, _ := e.snapshot()
	if w != 20 || h != 4 || len(rows) != 4 {
		t.Fatalf("geometry = %dx%d rows=%d, want 20x4/4", w, h, len(rows))
	}
	for i, r := range rows {
		if got := ansi.StringWidth(r); got != 20 {
			t.Errorf("row %d width = %d, want 20 (%q)", i, got, r)
		}
		if !strings.HasSuffix(r, ansi.ResetStyle) {
			t.Errorf("row %d lacks trailing reset", i)
		}
	}
	if plain := ansi.Strip(rows[0]); !strings.HasPrefix(plain, "こんにちは世界") {
		t.Errorf("row 0 = %q, want CJK text at column 0", plain)
	}
	if plain := ansi.Strip(rows[1]); !strings.HasPrefix(plain, "ab") {
		t.Errorf("row 1 = %q, want \"ab\"", plain)
	}
}

func TestViewerDiffAndExitFrame(t *testing.T) {
	e := startEntry(t, `printf 'one\n'`, 10, 3)
	v := &screenViewer{entry: e}
	v.needFull.Store(true)
	f := v.buildFrame()
	if !f.Full || len(f.Lines) != 3 {
		t.Fatalf("first frame full=%v lines=%d, want full with 3 lines", f.Full, len(f.Lines))
	}
	if f.Type != screen.FrameTypeExit {
		t.Fatalf("frame type = %q, want exit (child already gone)", f.Type)
	}
	// Nothing changed: the diff must be empty.
	f2 := v.buildFrame()
	if f2.Full || len(f2.Lines) != 0 {
		t.Fatalf("second frame full=%v lines=%d, want incremental/empty", f2.Full, len(f2.Lines))
	}
}

func TestDiffRows(t *testing.T) {
	prev := []string{"a", "b", "c"}
	cur := []string{"a", "B", "c"}
	got := diffRows(prev, cur, false)
	if len(got) != 1 || got[0].Y != 1 || got[0].S != "B" {
		t.Fatalf("diff = %+v, want [{1 B}]", got)
	}
	if got := diffRows(prev, cur[:2], false); len(got) != 2 {
		t.Fatalf("geometry change should force full diff, got %d lines", len(got))
	}
}

func TestPadRowsTruncatesAndPads(t *testing.T) {
	rows := padRows("abcdefgh\nx", 5, 3)
	want := []string{"abcde", "x    ", "     "}
	for i, r := range rows {
		if got := ansi.Strip(r); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}
}

// TestScreenStreamRoundTrip exercises the HTTP side: start a PTY through
// the real handler, subscribe to its screen, type a key, and expect the
// echoed character to come back in a frame.
func TestScreenStreamRoundTrip(t *testing.T) {
	old := newPTYCommand
	newPTYCommand = func(args ...string) *exec.Cmd { return exec.Command("sh", "-c", "cat") }
	t.Cleanup(func() { newPTYCommand = old })

	s := &Server{ptyMap: map[string]*ptyEntry{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handlePTY))
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	resp, err := http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{"cols":30,"rows":5}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var started struct{ PtyKey string }
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pty/"+started.PtyKey, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})

	c, err := screen.Dial(addr, started.PtyKey, "", 30, 5)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)

	first := <-c.Frames
	if !first.Full || first.W != 30 || first.H != 5 {
		t.Fatalf("first frame = %+v, want full 30x5", first)
	}
	if err := c.SendKey(uv.Key{Text: "x", Code: 'x'}); err != nil {
		t.Fatalf("send key: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-c.Frames:
			for _, ln := range f.Lines {
				if strings.HasPrefix(ansi.Strip(ln.S), "x") {
					return
				}
			}
		case <-deadline:
			t.Fatal("echoed key never appeared in a frame")
		}
	}
}

// TestNormalizeKeyEncodesShiftedText drives normalizeKey through a real
// x/vt emulator: Shift+letter used to be dropped because the encoder only
// emits printable keys with Mod == 0.
func TestNormalizeKeyEncodesShiftedText(t *testing.T) {
	cases := []struct {
		name string
		key  uv.Key
		want string
	}{
		{"shift letter", uv.Key{Code: 'a', ShiftedCode: 'A', Text: "A", Mod: uv.ModShift}, "A"},
		{"shift symbol", uv.Key{Code: '1', ShiftedCode: '!', Text: "!", Mod: uv.ModShift}, "!"},
		{"caps lock", uv.Key{Code: 'b', Text: "B", Mod: uv.ModCapsLock}, "B"},
		{"plain", uv.Key{Code: 'c', Text: "c"}, "c"},
		{"multi rune", uv.Key{Code: uv.KeyExtended, Text: "日本"}, "日本"},
		{"ctrl passes through", uv.Key{Code: 'c', Mod: uv.ModCtrl}, "\x03"},
		{"shift tab passes through", uv.Key{Code: uv.KeyTab, Mod: uv.ModShift}, "\x1b[Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emu := vt.NewEmulator(80, 24)
			// emu.SendKey writes into an unbuffered pipe, so keep reading
			// until every byte we expect has arrived.
			got := make(chan string, 1)
			go func() {
				var acc []byte
				buf := make([]byte, 64)
				for len(acc) < len(tc.want) {
					n, err := emu.Read(buf)
					acc = append(acc, buf[:n]...)
					if err != nil {
						break
					}
				}
				got <- string(acc)
			}()
			for _, k := range normalizeKey(tc.key) {
				emu.SendKey(k)
			}
			select {
			case s := <-got:
				if s != tc.want {
					t.Fatalf("got %q, want %q", s, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no output from emulator")
			}
		})
	}
}

// TestPTYTagAliasesSession: a spawned child sees its hook tag in
// CCDASH_WRAPPER_PID, and a hook carrying that tag keys the PTY by the
// session ID so the TUI can mirror a fresh `n` spawn right away.
func TestPTYTagAliasesSession(t *testing.T) {
	old := newPTYCommand
	newPTYCommand = func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `printf 'tag=%s' "$CCDASH_WRAPPER_PID"; cat`)
	}
	t.Cleanup(func() { newPTYCommand = old })

	s := &Server{ptyMap: map[string]*ptyEntry{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handlePTY))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{"cols":40,"rows":3}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var started struct {
		PtyKey string
		Tag    int
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pty/"+started.PtyKey, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})
	if started.Tag < ptyTagBase {
		t.Fatalf("tag = %d, want >= %d", started.Tag, ptyTagBase)
	}

	s.ptyMu.Lock()
	entry := s.ptyMap[started.PtyKey]
	s.ptyMu.Unlock()
	want := fmt.Sprintf("tag=%d", started.Tag)
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _, _, _ := entry.snapshot()
		if strings.HasPrefix(ansi.Strip(rows[0]), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child env: row 0 = %q, want prefix %q", ansi.Strip(rows[0]), want)
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.aliasPTYByTag(started.Tag+1, "other") // unknown tag: no-op
	s.aliasPTYByTag(started.Tag, "sess-1")
	s.ptyMu.Lock()
	got, other := s.ptyMap["sess-1"], s.ptyMap["other"]
	s.ptyMu.Unlock()
	if got != entry {
		t.Fatal("sess-1 not aliased to the spawned PTY")
	}
	if other != nil {
		t.Fatal("unknown tag created an alias")
	}
}

// TestAliasPTYsByParent: without hooks, discovery's procmap entry for a
// claude whose parent is a pid-<N> PTY shell keys that PTY by session ID.
func TestAliasPTYsByParent(t *testing.T) {
	old := newPTYCommand
	// `; :` keeps sh from exec-ing sleep, so sleep is the shell's child —
	// the same shape as `$SHELL -i -c claude`.
	newPTYCommand = func(args ...string) *exec.Cmd { return exec.Command("sh", "-c", "sleep 5; :") }
	t.Cleanup(func() { newPTYCommand = old })

	s := &Server{ptyMap: map[string]*ptyEntry{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handlePTY))
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var started struct{ PtyKey string }
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pty/"+started.PtyKey, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})
	s.ptyMu.Lock()
	entry := s.ptyMap[started.PtyKey]
	s.ptyMu.Unlock()
	shell := entry.sess.PID()

	var child int
	deadline := time.Now().Add(3 * time.Second)
	for child == 0 && time.Now().Before(deadline) {
		for pid, ppid := range parentPIDs(context.Background()) {
			if ppid == shell {
				child = pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("no child process under the PTY shell")
	}

	s.aliasPTYsByParent(context.Background(), map[string]procmap.Entry{
		"sess-x":    {SessionID: "sess-x", PID: child},
		"unrelated": {SessionID: "unrelated", PID: 1},
	})
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if s.ptyMap["sess-x"] != entry {
		t.Fatal("sess-x not aliased to the PTY whose shell is its parent")
	}
	if _, ok := s.ptyMap["unrelated"]; ok {
		t.Fatal("unrelated process got aliased")
	}
}

// TestAliasPTYsByOwnPID: a shell that execs claude leaves claude with the
// PTY's own PID (no parent hop) — that must alias too.
func TestAliasPTYsByOwnPID(t *testing.T) {
	s := &Server{ptyMap: map[string]*ptyEntry{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handlePTY))
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var started struct{ PtyKey string }
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pty/"+started.PtyKey, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})
	s.ptyMu.Lock()
	entry := s.ptyMap[started.PtyKey]
	s.ptyMu.Unlock()

	s.aliasPTYsByParent(context.Background(), map[string]procmap.Entry{
		"sess-exec": {SessionID: "sess-exec", PID: entry.sess.PID()},
	})
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if s.ptyMap["sess-exec"] != entry {
		t.Fatal("sess-exec not aliased to the PTY that is its own process")
	}
}

// TestPTYStartPrompt: a prompt becomes claude's trailing argument (the
// skill picker's "/<skill>"), and one that looks like a flag is refused.
func TestPTYStartPrompt(t *testing.T) {
	old := newPTYCommand
	got := make(chan []string, 1)
	newPTYCommand = func(args ...string) *exec.Cmd {
		got <- args
		return exec.Command("sh", "-c", "cat")
	}
	t.Cleanup(func() { newPTYCommand = old })

	s := &Server{ptyMap: map[string]*ptyEntry{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handlePTY))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{"prompt":"/code-flow:ship-pr"}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var started struct{ PtyKey string }
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pty/"+started.PtyKey, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})
	if args := <-got; len(args) != 1 || args[0] != "/code-flow:ship-pr" {
		t.Fatalf("args = %q, want [/code-flow:ship-pr]", args)
	}

	resp, err = http.Post(ts.URL+"/pty/start", "application/json", strings.NewReader(`{"prompt":"--dangerously-skip-permissions"}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("flag-like prompt: status %d, want 400", resp.StatusCode)
	}
}
