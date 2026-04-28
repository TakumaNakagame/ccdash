// Package attach runs an interactive subprocess (typically `claude --resume`)
// inside a PTY while ccdash holds onto the operator's real terminal.
//
// A Session represents one long-lived child. The first Attach call spawns
// claude in a PTY; subsequent Attach calls (after a Ctrl+D detach) re-enter
// the same child. While detached, ccdash is back in its dashboard but the
// PTY pump is still draining claude's output (into io.Discard, by default)
// so claude doesn't block on a full kernel write buffer. Detach does NOT
// terminate claude — the child only dies when it exits on its own or
// Session.Close is called explicitly (e.g. on operator-initiated kill).
//
// AttachCmd wraps a Session as a bubbletea.ExecCommand so the same Session
// can be passed through tea.Exec on every attach.
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// EscapeByte is Ctrl+D (ASCII EOT). When pressed during attach, ccdash
// pulls control back to the dashboard but leaves claude running. The byte
// is intercepted before reaching claude, so claude never sees an EOF on
// stdin — its session remains live, ready for the next attach.
const EscapeByte = 0x04

// Banner is what we print just before handing control to claude so the
// operator knows how to come back. Single line, ASCII-safe.
const Banner = "[ccdash] attached — press Ctrl+D to detach (claude keeps running)\r\n"

// Result carries why a single Attach call returned. Detached means the
// operator hit Ctrl+D and the child is still alive in the background.
// ExitErr is set when the child terminated on its own (or Close was
// called from another goroutine).
type Result struct {
	Detached bool
	ExitErr  error
}

// Session is one claude child plus its PTY, reusable across attach/detach
// cycles. Construct with New and pass to AttachCmd; do not copy.
type Session struct {
	cmd *exec.Cmd

	// spawnOnce + spawnErr ensure pty.Start is called exactly once even if
	// Attach is invoked before the previous Attach has returned (which
	// shouldn't happen, but defending against it is cheap).
	spawnOnce sync.Once
	spawnErr  error

	pty *os.File

	// sinkMu guards sink. The PTY reader goroutine consults it on every
	// chunk; foreground vs background is a single pointer swap.
	sinkMu sync.Mutex
	sink   io.Writer

	// childExit is closed once the child has been Wait()ed on. childErr
	// holds the result. Set exactly once, before close.
	childExit chan struct{}
	childErr  error

	// pumpDone closes when the PTY reader goroutine exits (i.e. EOF / hard
	// error on the PTY). Used to wait for output drain on close.
	pumpDone chan struct{}

	// closed flips to 1 when Close has been called, so subsequent Attach
	// calls bail out cleanly instead of trying to re-spawn.
	closed atomic.Int32
}

// New builds a Session for the given exec.Cmd. The child is not started
// until the first Attach call.
func New(cmd *exec.Cmd) *Session {
	return &Session{cmd: cmd, sink: io.Discard}
}

// Alive reports whether the child is still running. A Session whose child
// has exited is not Alive; the caller should usually drop it from any
// per-session map and create a fresh one if it wants to attach again.
func (s *Session) Alive() bool {
	if s == nil {
		return false
	}
	if s.closed.Load() != 0 {
		return false
	}
	if s.childExit == nil {
		// Hasn't been spawned yet, but as far as ccdash is concerned the
		// session is "ready to attach". Treat that as alive.
		return true
	}
	select {
	case <-s.childExit:
		return false
	default:
		return true
	}
}

// Close terminates the child (SIGTERM, then waits) and releases the PTY.
// Idempotent; safe to call from any goroutine.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if !s.closed.CompareAndSwap(0, 1) {
		return nil
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	if s.childExit != nil {
		<-s.childExit
	}
	if s.pty != nil {
		_ = s.pty.Close()
	}
	if s.pumpDone != nil {
		<-s.pumpDone
	}
	return nil
}

// Attach blocks while the operator is in attach mode. It puts the
// terminal in raw mode, forwards stdin to the PTY (intercepting Ctrl+D),
// and routes PTY output to stdout for the duration of the call. On
// detach or child exit it tears down its raw-mode + signal-handler
// state and returns; the Session itself stays usable until Close.
func (s *Session) Attach() (Result, error) {
	if s.closed.Load() != 0 {
		return Result{}, errors.New("attach: session is closed")
	}
	stdinFile := os.Stdin
	stdoutFile := os.Stdout
	if !term.IsTerminal(int(stdinFile.Fd())) {
		return Result{}, errors.New("attach: stdin is not a terminal")
	}

	// Lazy spawn on first Attach. spawnOnce makes the Wait goroutine + PTY
	// pump exactly singleton across the Session's lifetime.
	s.spawnOnce.Do(func() {
		s.spawnErr = s.spawn()
	})
	if s.spawnErr != nil {
		return Result{}, s.spawnErr
	}
	if !s.Alive() {
		return Result{ExitErr: s.childErr}, errors.New("attach: child already exited")
	}

	if _, err := io.WriteString(stdoutFile, Banner); err != nil {
		return Result{}, fmt.Errorf("attach: write banner: %w", err)
	}

	// Match the PTY size to the operator's terminal up front, then keep
	// them in sync via SIGWINCH for the duration of the attach.
	_ = syncWinsize(stdinFile, s.pty)
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)
	go func() {
		for range winchCh {
			_ = syncWinsize(stdinFile, s.pty)
		}
	}()

	oldState, err := term.MakeRaw(int(stdinFile.Fd()))
	if err != nil {
		return Result{}, fmt.Errorf("attach: MakeRaw: %w", err)
	}
	defer func() { _ = term.Restore(int(stdinFile.Fd()), oldState) }()

	// Ask the PTY reader goroutine to start writing chunks to the
	// operator's terminal. When we return, we'll flip it back to discard.
	s.setSink(stdoutFile)
	defer s.setSink(io.Discard)

	// Nudge claude to redraw against the (possibly changed) winsize so
	// the operator sees a clean screen instead of stale bytes from before
	// they detached. SIGWINCH is the gentlest "redraw please" signal.
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGWINCH)
	}

	// Detach signal — set when the input pump sees Ctrl+D.
	detachReq := make(chan struct{}, 1)

	// Stdin → PTY with Ctrl+D interception. Tracked via inputDone so we
	// can guarantee the goroutine is gone before Run returns; otherwise
	// it would race Bubble Tea for the next keystroke.
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
						_, _ = s.pty.Write(buf[:idx])
					}
					select {
					case detachReq <- struct{}{}:
					default:
					}
					return
				}
				if _, werr := s.pty.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	stopInput := func() {
		_ = stdinFile.SetReadDeadline(time.Unix(1, 0))
		<-inputDone
		_ = stdinFile.SetReadDeadline(time.Time{})
	}

	select {
	case <-s.childExit:
		stopInput()
		return Result{ExitErr: s.childErr}, nil
	case <-detachReq:
		stopInput()
		return Result{Detached: true}, nil
	}
}

// spawn opens the PTY and starts the child. Sets up the long-lived Wait
// goroutine and the PTY reader pump. Called exactly once per Session via
// spawnOnce.
func (s *Session) spawn() error {
	f, err := pty.Start(s.cmd)
	if err != nil {
		return fmt.Errorf("attach: pty.Start: %w", err)
	}
	s.pty = f
	s.childExit = make(chan struct{})
	go func() {
		s.childErr = s.cmd.Wait()
		close(s.childExit)
		// Closing the PTY here ensures the reader pump exits. Without it,
		// the pump would block forever on a closed child's PTY which the
		// kernel tends to leave half-open.
		_ = s.pty.Close()
	}()
	s.pumpDone = make(chan struct{})
	go func() {
		defer close(s.pumpDone)
		buf := make([]byte, 4096)
		for {
			n, err := s.pty.Read(buf)
			if n > 0 {
				s.sinkMu.Lock()
				w := s.sink
				s.sinkMu.Unlock()
				_, _ = w.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

func (s *Session) setSink(w io.Writer) {
	s.sinkMu.Lock()
	s.sink = w
	s.sinkMu.Unlock()
}

// AttachCmd implements bubbletea.ExecCommand. It's a thin shim that calls
// Session.Attach and stashes the Result so the TUI's callback can decide
// what flash message to show.
type AttachCmd struct {
	Session *Session
	Result  Result
}

// SetStdin / SetStdout / SetStderr satisfy the interface but are unused —
// Attach reads/writes os.Stdin / os.Stdout directly because Bubble Tea
// wraps the input in cancelreader, which hides the underlying *os.File
// we need for raw-mode operations.
func (a *AttachCmd) SetStdin(io.Reader)  {}
func (a *AttachCmd) SetStdout(io.Writer) {}
func (a *AttachCmd) SetStderr(io.Writer) {}

// Run delegates to Session.Attach. Run-level errors (e.g. raw-mode setup
// failures) are returned directly; success/detach state lives in Result.
func (a *AttachCmd) Run() error {
	if a == nil || a.Session == nil {
		return errors.New("attach: nil Session")
	}
	res, err := a.Session.Attach()
	a.Result = res
	return err
}

// syncWinsize reads the operator's terminal size and applies it to the PTY
// so claude renders against the right geometry. Called from startup and
// the SIGWINCH handler.
func syncWinsize(tty *os.File, ptyFile *os.File) error {
	ws, err := pty.GetsizeFull(tty)
	if err != nil {
		return err
	}
	return pty.Setsize(ptyFile, ws)
}
