package attach

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// StreamResult carries why a single StreamClient.Run call returned.
type StreamResult struct {
	Detached bool
	ExitErr  error
}

// StreamClient implements tea.ExecCommand. It connects to the server's
// /pty/:id/stream endpoint (HTTP/1.1 upgrade to raw TCP), relays the
// operator's stdin/stdout, and handles Ctrl+D detach and SIGWINCH resize.
type StreamClient struct {
	// SessionID is the ptyKey (UUID or real sessionID) registered with the server.
	SessionID string
	// Addr is "host:port" of the ccdash server.
	Addr string
	// Token is the shared auth token (X-Ccdash-Token header).
	Token string
	// Result is populated by Run before it returns.
	Result StreamResult
}

// SetStdin / SetStdout / SetStderr satisfy the tea.ExecCommand interface but
// are intentionally unused — Run reads/writes os.Stdin/os.Stdout directly
// because Bubble Tea wraps the input in a cancelreader that hides the
// underlying *os.File needed for raw-mode operations.
func (c *StreamClient) SetStdin(io.Reader)  {}
func (c *StreamClient) SetStdout(io.Writer) {}
func (c *StreamClient) SetStderr(io.Writer) {}

// Run connects to the server, enters raw mode, and relays I/O until the
// operator detaches (Ctrl+D) or the server closes the connection.
func (c *StreamClient) Run() error {
	if c == nil {
		return errors.New("stream: nil StreamClient")
	}

	stdinFile := os.Stdin
	stdoutFile := os.Stdout
	if !term.IsTerminal(int(stdinFile.Fd())) {
		return errors.New("stream: stdin is not a terminal")
	}

	conn, err := dialStream(c.Addr, c.SessionID, c.Token)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send the initial terminal size as a JSON handshake so the server can
	// call pty.Setsize before the first render.
	cols, rows, sizeErr := term.GetSize(int(stdinFile.Fd()))
	if sizeErr != nil {
		cols, rows = 80, 24
	}
	if err := json.NewEncoder(conn).Encode(struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	}{Rows: rows, Cols: cols}); err != nil {
		return fmt.Errorf("stream: handshake: %w", err)
	}

	oldState, err := term.MakeRaw(int(stdinFile.Fd()))
	if err != nil {
		return fmt.Errorf("stream: MakeRaw: %w", err)
	}
	defer func() { _ = term.Restore(int(stdinFile.Fd()), oldState) }()

	// conn → stdout pump.
	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		_, _ = io.Copy(stdoutFile, conn)
	}()

	// SIGWINCH → resize POST to server.
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer func() {
		signal.Stop(winchCh)
		close(winchCh)
	}()
	go func() {
		for range winchCh {
			ws, err := pty.GetsizeFull(stdinFile)
			if err != nil {
				continue
			}
			_ = c.postResize(ws.Rows, ws.Cols)
		}
	}()

	// stdin → conn with Ctrl+D interception (self-pipe to interrupt Poll).
	stopR, stopW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return fmt.Errorf("stream: stop pipe: %w", pipeErr)
	}
	defer stopR.Close()

	detachReq := make(chan struct{}, 1)
	stdinFd := int(stdinFile.Fd())
	stopFd := int(stopR.Fd())
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 4096)
		pfds := []unix.PollFd{
			{Fd: int32(stdinFd), Events: unix.POLLIN},
			{Fd: int32(stopFd), Events: unix.POLLIN},
		}
		for {
			_, perr := unix.Poll(pfds, -1)
			if perr != nil {
				if perr == unix.EINTR {
					continue
				}
				return
			}
			if pfds[1].Revents != 0 {
				return
			}
			if pfds[0].Revents&unix.POLLIN == 0 {
				continue
			}
			n, rerr := stdinFile.Read(buf)
			if n > 0 {
				idx := -1
				for i := 0; i < n; i++ {
					if buf[i] == EscapeByte {
						idx = i
						break
					}
				}
				if idx >= 0 {
					if idx > 0 {
						_, _ = conn.Write(buf[:idx])
					}
					select {
					case detachReq <- struct{}{}:
					default:
					}
					return
				}
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	stopInput := func() {
		_ = stopW.Close()
		<-inputDone
	}

	select {
	case <-connDone:
		stopInput()
		c.Result = StreamResult{}
		return nil
	case <-detachReq:
		stopInput()
		c.Result = StreamResult{Detached: true}
		return nil
	}
}

// postResize sends a POST /pty/:id/resize to update the server-side PTY size.
func (c *StreamClient) postResize(rows, cols uint16) error {
	body, _ := json.Marshal(struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}{Rows: rows, Cols: cols})
	url := "http://" + c.Addr + "/pty/" + c.SessionID + "/resize"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Ccdash-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// dialStream opens a TCP connection to addr and performs the HTTP/1.1 upgrade
// handshake for the /pty/:id/stream endpoint. Returns the raw conn ready for
// bidirectional PTY I/O after the 101 response.
func dialStream(addr, sessionID, token string) (net.Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("stream: dial %s: %w", addr, err)
	}

	host := addr
	req := "GET /pty/" + sessionID + "/stream HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"X-Ccdash-Token: " + token + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: pty-raw\r\n" +
		"\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("stream: write upgrade request: %w", err)
	}

	// Read the response headers (until blank line).
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("stream: read status line: %w", err)
	}
	statusLine = strings.TrimSpace(statusLine)
	if !strings.Contains(statusLine, "101") {
		// Drain for an error body.
		var errBody strings.Builder
		for {
			line, err := br.ReadString('\n')
			errBody.WriteString(line)
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		conn.Close()
		return nil, fmt.Errorf("stream: server returned %q (expected 101)", statusLine)
	}
	// Consume remaining headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("stream: read headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Return a conn that wraps the bufio.Reader so any bytes the reader
	// already buffered past the headers are not lost.
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn wraps a net.Conn so that reads go through a bufio.Reader that
// may already hold bytes buffered beyond the HTTP response headers.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
