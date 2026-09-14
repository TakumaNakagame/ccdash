package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/takumanakagame/ccmanage/internal/attach"
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
