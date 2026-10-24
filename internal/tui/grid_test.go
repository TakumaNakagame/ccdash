package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/screen"
	"github.com/TakumaNakagame/ccdash/internal/transcript"
)

func TestGridRects(t *testing.T) {
	for _, tc := range []struct{ n, w, h int }{
		{1, 120, 30}, {2, 120, 30}, {3, 120, 30}, {4, 90, 30}, {5, 200, 40}, {5, 120, 40}, {7, 240, 50},
	} {
		rs := gridRects(tc.n, tc.w, tc.h)
		if len(rs) != tc.n {
			t.Fatalf("%+v: %d rects", tc, len(rs))
		}
		// No gaps, no overlaps: the tiles cover the body exactly.
		area := 0
		for _, r := range rs {
			area += r.w * r.h
			if r.x < 0 || r.y < 0 || r.x+r.w > tc.w || r.y+r.h > tc.h {
				t.Fatalf("%+v: %+v outside the body", tc, r)
			}
		}
		if area != tc.w*tc.h {
			t.Fatalf("%+v: tiles cover %d of %d cells", tc, area, tc.w*tc.h)
		}
	}
	// 5 in 2 columns: the last row is one wide tile.
	if cols, _ := gridDims(5, 120, 40); cols == 2 {
		rs := gridRects(5, 120, 40)
		if last := rs[4]; last.w != 120 {
			t.Fatalf("last tile %+v, want the full width", last)
		}
	}
}

func TestGridBelow(t *testing.T) {
	// 5 tiles: 3 on top, 2 stretched below.
	rs := gridRects(5, 120, 30)
	if cols, rows := gridDims(5, 120, 30); cols != 3 || rows != 2 || rs[3].w != 60 {
		t.Fatalf("layout %dx%d, %+v", cols, rows, rs)
	}
	for _, tc := range []struct{ i, dir, want int }{
		{0, 1, 3}, {2, 1, 4}, {4, -1, 2}, {3, -1, 0}, {0, -1, -1}, {3, 1, -1},
	} {
		if got := gridBelow(rs, tc.i, tc.dir); got != tc.want {
			t.Errorf("gridBelow(%d, %d) = %d, want %d", tc.i, tc.dir, got, tc.want)
		}
	}
}

func gridModel() *model {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 30
	m.settings.WorkAnim = true
	m.sessions = []mdl.Session{
		{SessionID: "a", Num: 1, Title: "テストを直す", Status: mdl.StatusActive, Cwd: "/x/a", Tool: "Bash", ToolSummary: "go test ./..."},
		{SessionID: "b", Num: 2, Title: "quiet", Status: mdl.StatusIdle, Cwd: "/x/b"},
		{SessionID: "c", Num: 3, Title: "asks", Status: mdl.StatusIdle, Cwd: "/x/c", Attention: mdl.AttentionNeedsYou, AttentionReason: "AskUserQuestion"},
	}
	m.allSessions = m.sessions
	m.selSess = 1
	return m
}

func TestGridView(t *testing.T) {
	m := gridModel()
	key := func(s string) {
		m.Update(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
	}

	key("V")
	if !m.gridOpen {
		t.Fatal("V did not open the grid")
	}
	// No marks: the busy sessions — a (working) and c (needs you); the
	// focus moves from the quiet selection to the first tile.
	if got := m.gridSessions(); len(got) != 2 || m.sessions[m.selSess].SessionID != "a" {
		t.Fatalf("tiles %v", got)
	}
	m.gridTails = map[string]*gridTail{"a": {msgs: []transcript.Message{{Kind: transcript.KindAssistant, Text: "running the tests now"}}}}
	out := ansi.Strip(m.View().Content)
	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		t.Fatalf("%d lines > height %d", len(lines), m.height)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("line wider than the terminal (%d): %q", w, l)
		}
	}
	for _, want := range []string{"#1", "Bash: go test ./...", "running the tests now", "? AskUserQuestion"} {
		if !strings.Contains(out, want) {
			t.Fatalf("grid lacks %q:\n%s", want, out)
		}
	}

	// tab moves the focus = the selection.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.sessions[m.selSess].SessionID != "c" {
		t.Fatalf("tab: selected %q", m.sessions[m.selSess].SessionID)
	}

	// Marks replace the automatic choice.
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if got := m.gridSessions(); len(got) != 1 || m.sessions[got[0]].SessionID != "c" {
		t.Fatalf("after mark: %v", got)
	}

	// esc closes it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.gridOpen {
		t.Fatal("esc did not close the grid")
	}
}

func TestTileFrame(t *testing.T) {
	body := []string{"short", strings.Repeat("x", 50), "日本語"}
	for tick := 0; tick < 60; tick++ {
		out := tileFrame(body, 30, 6, "240", true, tick)
		lines := strings.Split(out, "\n")
		if len(lines) != 6 {
			t.Fatalf("tick %d: %d lines", tick, len(lines))
		}
		for _, l := range lines {
			if w := ansi.StringWidth(l); w != 30 {
				t.Fatalf("tick %d: width %d: %q", tick, w, ansi.Strip(l))
			}
		}
	}
	// The light runs round the whole frame: over a lap every edge lights.
	edges := map[string]bool{}
	for tick := 0; tick < gridLoopTicks; tick++ {
		lines := strings.Split(ansi.Strip(tileFrame(body, 30, 6, "240", true, tick)), "\n")
		if strings.Contains(lines[0], "━") {
			edges["top"] = true
		}
		if strings.Contains(lines[5], "━") {
			edges["bottom"] = true
		}
		for _, l := range lines[1:5] {
			if strings.HasPrefix(l, "┃") {
				edges["left"] = true
			}
			if strings.HasSuffix(l, "┃") {
				edges["right"] = true
			}
		}
	}
	if len(edges) != 4 {
		t.Fatalf("lit edges %v", edges)
	}
	if strings.Contains(ansi.Strip(tileFrame(body, 30, 6, "240", false, 3)), "━") {
		t.Fatal("lit without glow")
	}
}

func TestGridLiveTile(t *testing.T) {
	m := gridModel()
	m.settings.AttachEnabled = true
	m.ptyAlive = map[string]bool{"a": true}
	m.Update(tea.KeyPressMsg{Code: 'V', Text: "V"})
	if _, ok := m.gridWant()["a"]; !ok || !m.gridConnecting["a"] {
		t.Fatalf("live tile not dialed: want=%v connecting=%v", m.gridWant(), m.gridConnecting)
	}
	// Pretend the stream delivered a screen.
	m.gridLive = map[string]*liveScreen{"a": {key: "a", rows: []string{"> claude says hi", "  ⏺ Bash(go test)"}}}
	out := ansi.Strip(m.View().Content)
	for _, want := range []string{"claude says hi", "#1", "Bash: go test ./..."} {
		if !strings.Contains(out, want) {
			t.Fatalf("live tile lacks %q:\n%s", want, out)
		}
	}
	// No client yet: enter can't type, so it opens the dashboard instead.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.gridTyping || m.gridOpen {
		t.Fatalf("typing=%v open=%v", m.gridTyping, m.gridOpen)
	}
	if len(m.gridLive) != 0 {
		t.Fatal("tile streams kept after the grid closed")
	}
}

// fakeScreenServer speaks /pty/{key}/screen well enough for a client:
// the upgrade, one full frame, then every input it reads goes to inputs.
func fakeScreenServer(t *testing.T, rows []string) (addr string, inputs chan screen.Input) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	inputs = make(chan screen.Input, 64)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					l, err := br.ReadString('\n')
					if err != nil || strings.TrimSpace(l) == "" {
						break
					}
				}
				fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", screen.UpgradeProtocol)
				f := screen.Frame{Type: "frame", W: 40, H: len(rows), Full: true, Cursor: screen.Cursor{X: 2, Y: 1, Visible: true}}
				for i, r := range rows {
					f.Lines = append(f.Lines, screen.Line{Y: i, S: r})
				}
				_ = json.NewEncoder(conn).Encode(f)
				dec := json.NewDecoder(br)
				for {
					var in screen.Input
					if dec.Decode(&in) != nil {
						return
					}
					inputs <- in
				}
			}()
		}
	}()
	return ln.Addr().String(), inputs
}

func TestGridLiveStream(t *testing.T) {
	addr, inputs := fakeScreenServer(t, []string{"live screen row", "> typed here"})
	m := gridModel()
	m.settings.AttachEnabled = true
	m.ptyAlive = map[string]bool{"a": true}
	m.Update(tea.KeyPressMsg{Code: 'V', Text: "V"})
	g := m.gridWant()["a"]

	// Run the dial against the fake server, then pump the frames in.
	c, err := screen.Dial(addr, "a", "tok", g.w, g.h)
	if err != nil {
		t.Fatal(err)
	}
	wait := m.handleGridConnected(gridConnectedMsg{key: "a", client: c})
	if wait == nil {
		t.Fatal("no frame wait after connect")
	}
	m.Update(wait())
	if in := <-inputs; in.Type != screen.InputTypeResize || in.Cols != g.w || in.Rows != g.h {
		t.Fatalf("first input %+v, want resize %dx%d", in, g.w, g.h)
	}
	if out := ansi.Strip(m.View().Content); !strings.Contains(out, "live screen row") {
		t.Fatalf("frame not drawn:\n%s", out)
	}

	// enter types into the tile; keys and paste go to the stream; the
	// real cursor sits on the tile's emulator cursor.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.gridTyping {
		t.Fatal("enter on a live tile did not start typing")
	}
	if in := <-inputs; in.Type != screen.InputTypeFocus || !in.Focus {
		t.Fatalf("want focus in, got %+v", in)
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if in := <-inputs; in.Type != screen.InputTypeKey || in.Key == nil || in.Key.Text != "x" {
		t.Fatalf("want key x, got %+v", in)
	}
	m.Update(tea.PasteMsg{Content: "pasted"})
	if in := <-inputs; in.Type != screen.InputTypePaste || in.Text != "pasted" {
		t.Fatalf("want paste, got %+v", in)
	}
	cur := m.View().Cursor
	bodyTop, _ := m.bodyLayout()
	if cur == nil || cur.X != g.x+2 || cur.Y != bodyTop+g.y+1 {
		t.Fatalf("cursor %+v, want (%d,%d)", cur, g.x+2, bodyTop+g.y+1)
	}

	// ctrl+] hands the keyboard back; the grid keys work again.
	m.Update(tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	if m.gridTyping {
		t.Fatal("ctrl+] did not stop typing")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.sessions[m.selSess].SessionID != "c" {
		t.Fatalf("tab after typing: selected %q", m.sessions[m.selSess].SessionID)
	}

	// Closing the grid closes the tile stream.
	m.Update(tea.KeyPressMsg{Code: 'V', Text: "V"})
	if len(m.gridLive) != 0 {
		t.Fatal("stream kept after the grid closed")
	}
}
