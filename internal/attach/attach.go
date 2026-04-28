// Package attach runs an interactive subprocess (typically `claude --resume`)
// inside a PTY while ccdash holds onto the operator's real terminal. Unlike
// tea.ExecProcess — which suspends Bubble Tea and waits for the child to exit
// before returning — Run watches stdin for an escape key (Ctrl+], by default)
// and lets the operator drop back to the ccdash dashboard mid-session.
//
// The package is a tea.ExecCommand so Bubble Tea owns alt-screen
// release/restore; we own everything between those events: raw-mode setup,
// PTY I/O relay, SIGWINCH propagation, and child shutdown.
//
// Linux/macOS only — Windows is not on the roadmap.
package attach

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// EscapeByte is Ctrl+]. We picked it because it's telnet's classic escape and
// Claude has no use for it. Ctrl+\ would dump a stack trace, Ctrl+C is
// claude's interrupt, Ctrl+Z is suspend; this one's free.
const EscapeByte = 0x1d

// Banner is what we print just before handing control to claude so the
// operator knows how to come back. Single line, ASCII-safe so it doesn't
// disturb terminals with weird locales.
const Banner = "[ccdash] attached — press Ctrl+] to detach\r\n"

// Command builds an attach session for the given exec.Cmd. It implements
// bubbletea.ExecCommand so it can be passed to tea.Exec; Bubble Tea calls
// SetStdin/SetStdout/SetStderr with the operator's real terminal streams,
// then calls Run.
type Command struct {
	Cmd *exec.Cmd

	// Result is set by Run and is safe to read once Run returns.
	Result Result

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// Result carries why Run returned. Detached is true when the operator hit the
// escape key; in that case the child has been signalled and waited on, but
// any subsequent transcript work has to happen on top of the JSONL the child
// already flushed. ExitErr is set to the child's exit error if it terminated
// on its own (or to the kill error path).
type Result struct {
	Detached bool
	ExitErr  error
}

// SetStdin / SetStdout / SetStderr satisfy tea.ExecCommand. We hold the
// streams and only consult them inside Run, where the terminal is ours.
func (c *Command) SetStdin(r io.Reader)  { c.stdin = r }
func (c *Command) SetStdout(w io.Writer) { c.stdout = w }
func (c *Command) SetStderr(w io.Writer) { c.stderr = w }

// Run is the meat. Allocates a PTY, starts the child attached to it, puts
// the operator's terminal in raw mode, and pumps bytes back and forth until
// either the child exits or Ctrl+] is pressed.
func (c *Command) Run() error {
	if c.Cmd == nil {
		return errors.New("attach: nil Cmd")
	}
	if c.stdin == nil || c.stdout == nil {
		return errors.New("attach: stdin/stdout not set (Bubble Tea must call SetStdin/SetStdout first)")
	}

	// We only know how to relay against an actual terminal — Bubble Tea's
	// non-TTY paths (tests, piped runs) don't apply here.
	stdinFile, ok := c.stdin.(*os.File)
	if !ok {
		return errors.New("attach: stdin is not *os.File; cannot enter raw mode")
	}
	if !term.IsTerminal(int(stdinFile.Fd())) {
		return errors.New("attach: stdin is not a terminal")
	}

	// Print the help banner before handing the screen to claude. The banner
	// is short on purpose; claude redraws aggressively.
	if _, err := io.WriteString(c.stdout, Banner); err != nil {
		return fmt.Errorf("attach: write banner: %w", err)
	}

	ptyFile, err := pty.Start(c.Cmd)
	if err != nil {
		return fmt.Errorf("attach: pty.Start: %w", err)
	}
	defer ptyFile.Close()

	// Match the PTY size to the operator's terminal up front, then keep
	// them in sync via SIGWINCH for the duration of the session.
	if err := syncWinsize(stdinFile, ptyFile); err != nil {
		// Non-fatal: claude can run with a default 80x24 if we can't ask.
		fmt.Fprintf(c.stderr, "[ccdash] warning: initial winsize sync failed: %v\r\n", err)
	}
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)
	go func() {
		for range winchCh {
			_ = syncWinsize(stdinFile, ptyFile)
		}
	}()

	oldState, err := term.MakeRaw(int(stdinFile.Fd()))
	if err != nil {
		_ = c.Cmd.Process.Kill()
		_, _ = c.Cmd.Process.Wait()
		return fmt.Errorf("attach: MakeRaw: %w", err)
	}
	defer func() { _ = term.Restore(int(stdinFile.Fd()), oldState) }()

	// childExit fires when claude finishes on its own. Wait must run exactly
	// once; we serialise it via this goroutine and a single-receive channel.
	childExit := make(chan error, 1)
	go func() {
		childExit <- c.Cmd.Wait()
	}()

	// detachReq fires when the input pump sees Ctrl+]. We stop forwarding
	// stdin, signal the child, then drain the PTY until the child is gone
	// so claude has a chance to flush its final output before we return.
	detachReq := make(chan struct{}, 1)

	// stdin → PTY, with Ctrl+] interception. We track exit via inputDone
	// so we can guarantee the goroutine is gone before Run returns —
	// otherwise it would race Bubble Tea for the next keystroke when the
	// alt-screen comes back.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 4096)
		for {
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
						_, _ = ptyFile.Write(buf[:idx])
					}
					select {
					case detachReq <- struct{}{}:
					default:
					}
					return
				}
				if _, werr := ptyFile.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// stopInput unblocks the stdin Read so the goroutine returns. We use
	// the runtime poller's deadline support (works for TTY fds on
	// Linux/macOS); the deadline is cleared right after so Bubble Tea's
	// next reader on the same fd is unaffected.
	stopInput := func() {
		_ = stdinFile.SetReadDeadline(time.Unix(1, 0))
		<-inputDone
		_ = stdinFile.SetReadDeadline(time.Time{})
	}

	// PTY → stdout. No filtering — claude's bytes hit the terminal as-is.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(c.stdout, ptyFile)
	}()

	select {
	case err := <-childExit:
		// Child terminated on its own. Stop the input pump so it doesn't
		// race Bubble Tea, then wait for the output pump to drain.
		stopInput()
		_ = ptyFile.Close()
		<-outputDone
		c.Result.ExitErr = err
		return nil

	case <-detachReq:
		// Operator asked to leave. Send SIGTERM first (claude can flush its
		// transcript), wait for it to die, then drain remaining output.
		// stopInput is a no-op here (the goroutine already returned when
		// it saw EscapeByte) but cheap.
		c.Result.Detached = true
		if c.Cmd.Process != nil {
			_ = c.Cmd.Process.Signal(syscall.SIGTERM)
		}
		<-childExit
		stopInput()
		_ = ptyFile.Close()
		<-outputDone
		return nil
	}
}

// syncWinsize reads the operator's terminal size and applies it to the PTY
// so claude renders against the right geometry. Wraps the syscalls in a
// helper because we call it from both startup and the SIGWINCH handler.
func syncWinsize(tty *os.File, ptyFile *os.File) error {
	ws, err := pty.GetsizeFull(tty)
	if err != nil {
		return err
	}
	return pty.Setsize(ptyFile, ws)
}
