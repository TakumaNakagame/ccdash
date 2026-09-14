# 0002 — Live right pane via a server-side terminal emulator

- Date: 2026-09-10
- Status: Accepted (supersedes 0001 for sessions hosted by the ccdash server)

## Context

0001 retired the v0.3.0 windowed attach because two things broke under
Japanese input: the vt10x emulator advanced one column per rune (CJK
drift), and the OS IME had no real cursor to anchor its pre-edit overlay
on, so every Bubble Tea re-render fought the overlay.

`cmd/vtspike/FINDINGS.md` (2026-09-09) re-tested the idea with different
parts and found both walls gone in principle:

- **Bubble Tea v2** lets `View()` return a `tea.View` with a `Cursor`
  field. The real terminal cursor is placed at the emulator's cursor in
  the same write as the frame, so the IME has a stable anchor.
- **charmbracelet/x/vt** tracks display width per cell (grapheme
  clusters), so wide characters land where claude thinks they are.

## Decision

Bring the embedded view back, but with the emulator living in the
**server**, not the TUI:

- Every PTY the server spawns (`POST /pty/start`) is wrapped in an x/vt
  emulator that consumes the child's output continuously
  (`internal/server/screen.go`). The screen is therefore already
  populated when a viewer connects, survives TUI restarts, and needs no
  SIGWINCH / Ctrl+L nudge.
- The TUI subscribes with `GET /pty/{key}/screen` (HTTP upgrade to a
  newline-delimited JSON stream, `internal/screen`). Frames carry only
  changed rows; keys / paste / wheel / resize / focus go back on the same
  connection. Frames are coalesced to 16 ms per viewer.
- The right pane (`internal/tui/live.go`) splices the rows in below a
  one-line status header and places `View.Cursor` at the emulator cursor
  while the pane has focus. `Enter` / `Ctrl+]` / mouse click give claude
  the keyboard; `Ctrl+]` / `Ctrl+D` / clicking elsewhere give it back.
- Fullscreen attach stays available (`F`) on top of the same PTY via the
  raw relay (`/pty/{key}/stream`). While a raw client is connected the
  emulator keeps consuming bytes but stops answering DSR/DA queries so
  the operator's real terminal is the only responder.

Sessions that are **not** hosted by the ccdash server (started in some
other terminal or tmux) cannot be mirrored — their PTY belongs to
another process. Those rows keep the transcript tail, and `Enter` still
does `tmux switch-client` when a pane is known. Stopped sessions are
resumed into a server PTY on `Enter`, after which they are live.

## Consequences

- `internal/tui` now depends on Bubble Tea v2 (`charm.land/bubbletea/v2`).
  `tea.KeyMsg` → `tea.KeyPressMsg`, `msg.Type/Runes` → `msg.Code/Text`,
  mouse messages are per-type, `" "` → `"space"`, alt-screen and mouse
  mode are declared on the `tea.View`.
- `x/vt` is pre-v1 and untagged; `go.mod` pins a commit. Bump
  deliberately and re-run the IME check below.
- The terminal bell for new approvals is written to stdout directly
  (`ringBellCmd`) because the v2 cell renderer does not pass control
  characters through view content.
- **IME behaviour cannot be checked automatically.** The gate that killed
  v0.3.x was OS-level composition. Before a release touching this path,
  open a hosted session, focus the pane, and type Japanese through the
  IME in at least Terminal.app and one of Ghostty / iTerm2: pre-edit
  text must sit at the cursor, no ghost spaces, no drift after confirm.
  `go run ./cmd/vtspike` remains as a standalone harness for the same
  check outside ccdash.
