// vtspike is a throwaway prototype that answers one question: can ccdash
// host a live `claude` session inside its right pane with correct IME
// behaviour and CJK column tracking?
//
// It is deliberately self-contained: Bubble Tea v2 + charmbracelet/x/vt +
// creack/pty. Nothing here is wired into the real TUI yet.
//
//	go run ./cmd/vtspike                 # spawns `claude` in the right pane
//	go run ./cmd/vtspike -- zsh -l       # or any other command
//	go run ./cmd/vtspike -left 30 -- claude --resume <id>
//
// Keys while the right pane has focus:
//
//	Ctrl+]   toggle focus between left pane (spike UI) and right pane (child)
//	Ctrl+Q   quit (kills the child)
//
// Everything else is forwarded to the child. Mouse wheel over the right pane
// is forwarded too.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

const (
	keyToggleFocus = 0x1d // Ctrl+]
	frameInterval  = 16 * time.Millisecond
)

// child bundles the PTY, the emulator and the goroutines pumping between them.
type child struct {
	cmd  *exec.Cmd
	ptmx *os.File
	emu  *vt.Emulator
	mu   sync.Mutex // guards emu for everything except Read

	cursorHidden atomic.Bool
	cursorStyle  atomic.Int32 // vt.CursorStyle
	cursorBlink  atomic.Bool

	exited atomic.Bool
	exitErr error
}

func startChild(args []string, cols, rows int) (*child, error) {
	c := &child{}
	c.emu = vt.NewEmulator(cols, rows)
	c.cursorBlink.Store(true)
	c.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { c.cursorHidden.Store(!visible) },
		CursorStyle: func(style vt.CursorStyle, blink bool) {
			c.cursorStyle.Store(int32(style))
			c.cursorBlink.Store(blink)
		},
	})

	c.cmd = exec.Command(args[0], args[1:]...)
	c.cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	ptmx, err := pty.StartWithSize(c.cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	c.ptmx = ptmx

	// emulator → pty: key encodings, DSR/DA replies, paste brackets.
	go func() { _, _ = io.Copy(ptmx, c.emu) }()
	return c, nil
}

// pump reads the PTY and feeds the emulator. It sends at most one
// frameMsg per frameInterval so a chatty child doesn't flood the renderer.
func (c *child) pump(p *tea.Program) {
	buf := make([]byte, 32*1024)
	var last time.Time
	var timer *time.Timer
	for {
		n, err := c.ptmx.Read(buf)
		if n > 0 {
			c.mu.Lock()
			_, _ = c.emu.Write(buf[:n])
			c.mu.Unlock()
			if since := time.Since(last); since >= frameInterval {
				last = time.Now()
				p.Send(frameMsg{})
			} else if timer == nil {
				timer = time.AfterFunc(frameInterval-since, func() {
					p.Send(frameMsg{})
				})
			} else {
				timer.Reset(frameInterval - since)
			}
		}
		if err != nil {
			break
		}
	}
	c.exitErr = c.cmd.Wait()
	c.exited.Store(true)
	p.Send(exitMsg{})
}

func (c *child) resize(cols, rows int) {
	c.mu.Lock()
	c.emu.Resize(cols, rows)
	c.mu.Unlock()
	_ = pty.Setsize(c.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (c *child) sendKey(k tea.KeyPressMsg) {
	ev := uv.KeyPressEvent{
		Text:        k.Text,
		Mod:         k.Mod,
		Code:        k.Code,
		ShiftedCode: k.ShiftedCode,
		BaseCode:    k.BaseCode,
		IsRepeat:    k.IsRepeat,
	}
	c.mu.Lock()
	c.emu.SendKey(ev)
	c.mu.Unlock()
}

func (c *child) paste(s string) {
	c.mu.Lock()
	c.emu.Paste(s)
	c.mu.Unlock()
}

func (c *child) wheel(m tea.Mouse) {
	c.mu.Lock()
	c.emu.SendMouse(vt.MouseWheel{X: m.X, Y: m.Y, Button: vt.MouseButton(m.Button), Mod: m.Mod})
	c.mu.Unlock()
}

// snapshot renders the emulator into padded, per-row strings plus the
// cursor position. Called from View on the Bubble Tea goroutine.
func (c *child) snapshot() (rows []string, cur uv.Position) {
	c.mu.Lock()
	raw := c.emu.Render()
	cur = c.emu.CursorPosition()
	w, h := c.emu.Width(), c.emu.Height()
	c.mu.Unlock()

	rows = strings.Split(raw, "\n")
	for len(rows) < h {
		rows = append(rows, "")
	}
	rows = rows[:h]
	for i, r := range rows {
		if pad := w - ansi.StringWidth(r); pad > 0 {
			r += strings.Repeat(" ", pad)
		}
		rows[i] = r + ansi.ResetStyle
	}
	return rows, cur
}

func (c *child) close() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.ptmx.Close()
}

type frameMsg struct{}
type exitMsg struct{}

type model struct {
	ch        *child
	args      []string
	leftW     int
	width     int
	height    int
	focusPane bool // true = keystrokes go to the child
	keyLog    []string
	frames    int
}

func (m model) Init() tea.Cmd { return nil }

func (m model) paneGeom() (x, w, h int) {
	x = m.leftW + 1 // 1 col separator
	w = m.width - x
	if w < 10 {
		w = 10
	}
	return x, w, m.height
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		_, w, h := m.paneGeom()
		if m.ch == nil {
			ch, err := startChild(m.args, w, h)
			if err != nil {
				fmt.Fprintln(os.Stderr, "spawn:", err)
				return m, tea.Quit
			}
			m.ch = ch
			return m, func() tea.Msg { return startedMsg{} }
		}
		m.ch.resize(w, h)
		return m, nil

	case startedMsg:
		// The pump needs the *Program; we get it through a closure set in main.
		go m.ch.pump(prog)
		return m, nil

	case frameMsg:
		m.frames++
		return m, nil

	case exitMsg:
		return m, tea.Quit

	case tea.KeyPressMsg:
		if msg.Mod == tea.ModCtrl && msg.Code == 'q' {
			return m, tea.Quit
		}
		if msg.Mod == tea.ModCtrl && msg.Code == ']' {
			m.focusPane = !m.focusPane
			return m, nil
		}
		if m.focusPane && m.ch != nil {
			m.ch.sendKey(msg)
			return m, nil
		}
		m.keyLog = append(m.keyLog, msg.String())
		if len(m.keyLog) > 8 {
			m.keyLog = m.keyLog[1:]
		}
		return m, nil

	case tea.PasteMsg:
		if m.focusPane && m.ch != nil {
			m.ch.paste(msg.Content)
		}
		return m, nil

	case tea.MouseWheelMsg:
		x, _, _ := m.paneGeom()
		if m.ch != nil && msg.X >= x {
			mm := msg.Mouse()
			mm.X -= x
			m.ch.wheel(mm)
		}
		return m, nil
	}
	return m, nil
}

type startedMsg struct{}

func (m model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.width == 0 || m.ch == nil {
		v.SetContent("starting...")
		return v
	}

	x, w, h := m.paneGeom()
	rows, cur := m.ch.snapshot()

	focus := "LEFT"
	if m.focusPane {
		focus = "RIGHT"
	}
	left := []string{
		"vtspike",
		"",
		fmt.Sprintf("term %dx%d", m.width, m.height),
		fmt.Sprintf("pane %dx%d", w, h),
		fmt.Sprintf("cursor %d,%d", cur.X, cur.Y),
		fmt.Sprintf("hidden %v", m.ch.cursorHidden.Load()),
		fmt.Sprintf("frames %d", m.frames),
		fmt.Sprintf("focus %s", focus),
		"",
		"Ctrl+] focus",
		"Ctrl+Q quit",
		"",
		"keys (left):",
	}
	left = append(left, m.keyLog...)

	var b strings.Builder
	for y := 0; y < h; y++ {
		l := ""
		if y < len(left) {
			l = left[y]
		}
		l = ansi.Truncate(l, m.leftW, "")
		if pad := m.leftW - ansi.StringWidth(l); pad > 0 {
			l += strings.Repeat(" ", pad)
		}
		b.WriteString(l)
		b.WriteString("|")
		if y < len(rows) {
			b.WriteString(rows[y])
		}
		if y < h-1 {
			b.WriteByte('\n')
		}
	}
	v.SetContent(b.String())

	if m.focusPane && !m.ch.cursorHidden.Load() {
		shape := tea.CursorBlock
		switch vt.CursorStyle(m.ch.cursorStyle.Load()) {
		case vt.CursorUnderline:
			shape = tea.CursorUnderline
		case vt.CursorBar:
			shape = tea.CursorBar
		}
		v.Cursor = &tea.Cursor{
			Position: tea.Position{X: x + cur.X, Y: cur.Y},
			Shape:    shape,
			Blink:    m.ch.cursorBlink.Load(),
		}
	}
	return v
}

var prog *tea.Program

func main() {
	leftW := flag.Int("left", 24, "left pane width in columns")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"claude"}
	}

	m := model{args: args, leftW: *leftW, focusPane: true}
	prog = tea.NewProgram(m)
	final, err := prog.Run()
	if fm, ok := final.(model); ok && fm.ch != nil {
		fm.ch.close()
		if fm.ch.exitErr != nil {
			fmt.Fprintln(os.Stderr, "child:", fm.ch.exitErr)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
