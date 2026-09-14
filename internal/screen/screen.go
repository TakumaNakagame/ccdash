// Package screen defines the wire protocol between the ccdash server's
// per-session terminal emulator and the TUI's right pane, plus the client
// half of that protocol.
//
// The server owns one x/vt emulator per PTY session (see
// internal/server/screen.go). A TUI that wants to show the live screen
// opens GET /pty/{key}/screen, which upgrades to a bidirectional stream of
// newline-delimited JSON:
//
//	server → client  Frame   (rendered rows, cursor, exit notice)
//	client → server  Input   (keys, paste, wheel, resize, focus)
//
// Frames carry only the rows that changed since the previous frame sent to
// that client, except right after connect or resize when Full is set and
// every row is present. Rows are ANSI-styled strings, one per screen row,
// exactly W display columns wide, so the TUI can splice them into its own
// view without re-measuring.
package screen

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// UpgradeProtocol is the value of the Upgrade header for the screen stream.
const UpgradeProtocol = "ccdash-screen"

// Frame types.
const (
	FrameTypeFrame = "frame"
	FrameTypeExit  = "exit"
	// FrameTypeClosed is synthesized client-side when the stream drops.
	FrameTypeClosed = "closed"
)

// Input types.
const (
	InputTypeKey    = "key"
	InputTypePaste  = "paste"
	InputTypeWheel  = "wheel"
	InputTypeResize = "resize"
	InputTypeFocus  = "focus"
)

// Cursor is the emulator's cursor state as of the frame.
type Cursor struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	Visible bool `json:"vis"`
	// Shape: 0 block, 1 underline, 2 bar (mirrors vt.CursorStyle).
	Shape int  `json:"shape"`
	Blink bool `json:"blink"`
}

// Line is one changed row.
type Line struct {
	Y int    `json:"y"`
	S string `json:"s"`
}

// Frame is a server → client message.
type Frame struct {
	Type   string `json:"t"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Full   bool   `json:"full,omitempty"`
	Lines  []Line `json:"lines,omitempty"`
	Cursor Cursor `json:"cur"`
	// Err carries the child's exit error text on FrameTypeExit / the stream
	// error on FrameTypeClosed.
	Err string `json:"err,omitempty"`
}

// Input is a client → server message.
type Input struct {
	Type  string    `json:"t"`
	Key   *uv.Key   `json:"key,omitempty"`
	Text  string    `json:"text,omitempty"`
	Mouse *uv.Mouse `json:"mouse,omitempty"`
	Cols  int       `json:"cols,omitempty"`
	Rows  int       `json:"rows,omitempty"`
	Focus bool      `json:"focus,omitempty"`
}

// Client is the TUI side of the screen stream. Frames arrive on Frames;
// the Send* methods push input. Close tears down the connection; Frames is
// closed once the reader goroutine exits.
type Client struct {
	Key    string
	Frames chan Frame

	conn  net.Conn
	encMu sync.Mutex
	enc   *json.Encoder

	closeOnce sync.Once
}

// Dial connects to the server's screen stream for the given ptyKey and
// immediately requests the given size. cols/rows <= 0 skip the initial
// resize.
func Dial(addr, key, token string, cols, rows int) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("screen: dial %s: %w", addr, err)
	}
	req := "GET /pty/" + key + "/screen HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"X-Ccdash-Token: " + token + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: " + UpgradeProtocol + "\r\n" +
		"\r\n"
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("screen: write upgrade: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("screen: read status: %w", err)
	}
	status = strings.TrimSpace(status)
	if !strings.Contains(status, "101") {
		// Drain headers + a short body so the error is informative.
		var body strings.Builder
		inBody := false
		for i := 0; i < 64; i++ {
			line, err := br.ReadString('\n')
			if inBody {
				body.WriteString(line)
			} else if strings.TrimSpace(line) == "" {
				inBody = true
			}
			if err != nil {
				break
			}
		}
		conn.Close()
		msg := strings.TrimSpace(body.String())
		if msg == "" {
			return nil, fmt.Errorf("screen: server returned %q", status)
		}
		return nil, fmt.Errorf("screen: %s", msg)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("screen: read headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})

	c := &Client{
		Key:    key,
		Frames: make(chan Frame, 256),
		conn:   conn,
		enc:    json.NewEncoder(conn),
	}
	if cols > 0 && rows > 0 {
		if err := c.Resize(cols, rows); err != nil {
			c.Close()
			return nil, err
		}
	}
	go c.readLoop(br)
	return c, nil
}

func (c *Client) readLoop(br *bufio.Reader) {
	dec := json.NewDecoder(br)
	for {
		var f Frame
		if err := dec.Decode(&f); err != nil {
			msg := ""
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				msg = err.Error()
			}
			// Best effort: the buffer may be full if the TUI stalled; don't
			// block forever on a dead consumer.
			select {
			case c.Frames <- Frame{Type: FrameTypeClosed, Err: msg}:
			case <-time.After(time.Second):
			}
			close(c.Frames)
			c.Close()
			return
		}
		c.Frames <- f
	}
}

func (c *Client) send(in Input) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := c.enc.Encode(in)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

// SendKey forwards one key press to the emulator.
func (c *Client) SendKey(k uv.Key) error {
	return c.send(Input{Type: InputTypeKey, Key: &k})
}

// Paste forwards pasted text (bracketed if the child asked for it).
func (c *Client) Paste(text string) error {
	return c.send(Input{Type: InputTypePaste, Text: text})
}

// Wheel forwards a mouse wheel event in screen-relative coordinates.
func (c *Client) Wheel(m uv.Mouse) error {
	return c.send(Input{Type: InputTypeWheel, Mouse: &m})
}

// Resize asks the server to resize the PTY + emulator.
func (c *Client) Resize(cols, rows int) error {
	return c.send(Input{Type: InputTypeResize, Cols: cols, Rows: rows})
}

// Focus reports focus in/out to the child (only delivered when it enabled
// focus reporting).
func (c *Client) Focus(on bool) error {
	return c.send(Input{Type: InputTypeFocus, Focus: on})
}

// Close shuts the connection. Safe to call more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
	})
}
