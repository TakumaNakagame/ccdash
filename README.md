# ccdash

Local-only dashboard for monitoring multiple concurrent Claude Code sessions.

ccdash collects events (prompts, tool calls, permission requests) from one or
more Claude Code sessions via HTTP hooks, stores them in SQLite, and surfaces
them in a TUI.

**Status:** Phase 1 (observation only). Phase 2 (interactive approve / deny)
not yet implemented — pending approvals are recorded but Claude Code falls back
to its built-in interactive prompt.

## Build

```sh
go build -o ./bin/ccdash ./cmd/ccdash
```

## Setup

```sh
# 1. (one-time) Wire ccdash hooks into ~/.claude/settings.json. Idempotent —
#    preserves existing user hooks; safe to run multiple times.
ccdash install-hooks

# 2. Run claude as you normally would.
claude

# 3. Open the dashboard. ccdash spawns the collector inline for the duration
#    of the TUI session, so a single command is enough.
ccdash
```

When you quit the TUI the embedded collector shuts down with it. Hook events
that fire while the TUI is closed are not recorded.

To keep a collector running across TUI sessions:

```sh
ccdash -k           # spawn detached collector, then open TUI; collector
                    # outlives the TUI
# or
ccdash server &     # foreground collector you manage yourself
ccdash              # detects the existing server and just opens the TUI
```

The detached collector started by `-k` writes to `/tmp/ccdash-server.log`.
Stop it with `pkill -f 'ccdash server'` (or kill its PID) when done.

git repo / branch / commit are derived server-side from each session's `cwd`,
so they show up correctly even for plain `claude` invocations.

`ccdash claude` is an **optional** wrapper that additionally captures the tmux
pane / session and the wrapper PID. Use it only if you want those:

```sh
ccdash claude              # passes any args through to claude
ccdash claude --resume foo
```

If you want to remove the hooks:

```sh
ccdash uninstall-hooks
```

## Usage

```sh
ccdash                         # open the TUI
ccdash sessions                # list sessions (plain text)
ccdash events <session_id>     # event log for a session
ccdash approvals               # list pending permission requests
```

TUI keys:

| Key | Action |
| --- | --- |
| `↑` / `↓` (or `j` / `k`) | move selection |
| `g` / `G` | jump to top / bottom |
| `enter` | attach to selected session (see below) |
| `o` | open transcript viewer for the selected session |
| `tab` | switch between Sessions and Approvals views |
| `r` | force refresh |
| `q` / `ctrl+c` | quit |

Transcript viewer keys (`o`):

| Key | Action |
| --- | --- |
| `↑` / `↓` | scroll one line |
| `pgup` / `pgdn` (or `ctrl+u` / `ctrl+d` / space) | half-page scroll |
| `g` / `G` | top / bottom |
| `r` | reload from disk |
| `esc` / `q` / `tab` | back to sessions |

When a session has pending permission requests, its row is tinted yellow and
the header shows a `⚠ pending: N` badge. The terminal bell is rung once when
the total pending count transitions from zero to non-zero.

### Attach behaviour (`enter`)

ccdash discovers idle/active sessions automatically by reading
`~/.claude/sessions/<pid>.json` (Claude Code's own state file) and the
transcripts at `~/.claude/projects/*/<session_id>.jsonl`. Pressing `enter` on
a session does:

| State | Action |
| --- | --- |
| Running, in a tmux pane | `tmux switch-client -t <pane>` |
| Running, no tmux pane detected | flash with PID + TTY so you can switch terminal manually |
| Stopped (Claude no longer running) | `claude --resume <session_id>` from the session's cwd |

Tmux integration is automatic — if your claude sessions live in tmux panes,
ccdash will detect them via `tmux list-panes` and offer one-keystroke switching.

## Layout

```
cmd/ccdash/main.go              CLI entry
internal/cli/                   cobra command tree
internal/server/                HTTP hook receiver (127.0.0.1:9123)
internal/db/                    SQLite layer (sessions / events / approvals)
internal/model/                 data types
internal/tui/                   Bubble Tea UI
internal/wrapper/               `ccdash claude` exec wrapper
internal/hookcfg/               install-hooks settings.json merge
internal/paths/                 state dir / db / settings paths
docs/research.md                Phase 0 research on Claude Code hook spec
```

## State

- DB: `$XDG_STATE_HOME/ccdash/ccdash.sqlite` (default `~/.local/state/ccdash/`),
  permissions `0600`
- Server bind: `127.0.0.1:9123` (loopback only — never exposed externally)
- ccdash hook entries are tagged with the `X-Ccdash-Managed: true` header so
  install / uninstall are idempotent and don't collide with user hooks.
