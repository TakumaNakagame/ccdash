# ccdash

> Hobby project, designed for single-user / loopback-only operation. See the
> [Threat model](#threat-model) section before adopting it broadly.

ccdash is a local TUI dashboard for monitoring **multiple concurrent
Claude Code sessions** at the same time. It collects prompts, tool calls,
and permission requests via HTTP hooks, persists them to SQLite, and
surfaces what each session is doing in a single screen — with optional
control over approvals.

## What it does

| | |
| --- | --- |
| **Session inventory** | Auto-discovered from `~/.claude/sessions/<pid>.json` + transcript JSONLs. Idle / busy / recent / stopped status, age grouping (Today / Yesterday / This week / month buckets), per-session ★ favorites. |
| **Live transcript** | Right pane streams the latest USER / CLAUDE / TOOL / RESULT exchanges with role-coloured blocks. Tail-reads only the last 256 KB so 30 MB transcripts scroll smoothly. |
| **Per-session controls** | Rename, custom user-named tab assignment, archive / unarchive, attach via `tmux switch-pane` or `claude --resume`. |
| **Approvals** | When enabled, pending permission requests appear in a yellow banner; press `a` / `A` (keep) / `d` to allow / keep-allow-for-session / deny without leaving the dashboard. |
| **Summarize** | `s` spawns `claude -p` against a redacted digest of the transcript and shows a 3-5 bullet summary inline. |
| **Tabs** | Browser-style strip across the top filters by repo or operator-named tab. Slides on overflow. |
| **Search** | `/` filters the list by case-insensitive substring across title, tab, repo, summary, session id. |
| **Settings page** | `,` opens a persisted preferences modal: layout (auto / vertical / horizontal), refresh rate, summary timeout, secure-mode toggles, and an "observation only" preset. |
| **Self-update** | `ccdash update` pulls the latest GitHub release in place. |

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
```

Drops a binary in `$HOME/.local/bin` (override with `CCDASH_INSTALL_DIR=...`).
Already installed? Just `ccdash update` — it talks to the GitHub API and
swaps the binary atomically.

### Build from source

```sh
go build -o ./bin/ccdash ./cmd/ccdash
# or system-wide:
go install ./cmd/ccdash
```

## Setup

```sh
# 1. (one-time) wire ccdash hooks into ~/.claude/settings.json. Idempotent;
#    preserves any existing user hooks.
ccdash install-hooks

# 2. Run claude as you normally would.
claude

# 3. Open the dashboard. The TUI embeds the collector for the duration of
#    the session — single command, no daemons.
ccdash
```

Closing the TUI shuts down the collector. Hook events that fire while
the TUI is closed are not recorded.

To keep collecting across TUI restarts:

```sh
ccdash -k           # spawn a detached collector and open the TUI; the
                    # collector outlives the TUI
# or run a foreground collector you manage yourself:
ccdash server &
ccdash              # picks up the existing server
```

The detached collector (`-k`) writes to `/tmp/ccdash-server.log`; stop it
with `pkill -f 'ccdash server'`.

The optional `ccdash claude` wrapper passes args through to `claude` and
also captures the tmux pane and wrapper PID for richer attach information.

To unwire ccdash from Claude:

```sh
ccdash uninstall-hooks
```

## Usage

```sh
ccdash                         # open the TUI (default)
ccdash sessions                # list sessions (plain text)
ccdash events <session_id>     # event log for a session
ccdash approvals               # list pending permission requests
ccdash update                  # upgrade in place from latest release
ccdash --version               # report the current version
```

### TUI keys

**Sessions view (default)**

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | move selection |
| `g` / `G` | jump to top / bottom |
| `tab` / `shift+tab` | cycle project / user-named tabs |
| `R` | toggle "auto repo tabs" in the cycle |
| `T` | edit the user-named tab for this session |
| `t` | rename the session (operator override of auto title) |
| `f` | toggle ★ favorite (favorites pin to the top) |
| `x` | archive / unarchive |
| `X` | toggle archive view (operator-archived sessions) |
| `o` | full-screen transcript viewer |
| `s` | run `claude -p` and cache a 3–5 bullet summary |
| `enter` | attach to the session (tmux switch / `claude --resume`) |
| `a` / `A` / `d` | allow / keep-allow / deny the oldest pending approval |
| `Shift+J` `Shift+K` | scroll the right pane one line at a time |
| `pgup` / `pgdn` | half-page scroll on the right pane |
| `/` | search across session metadata |
| `,` | open the settings page |
| `r` | force refresh |
| `q` / `ctrl+c` | quit |

**Transcript modal (`o`)**

| Key | Action |
| --- | --- |
| `↑` `↓` | scroll one line |
| `pgup` `pgdn` / `ctrl+u` `ctrl+d` / space | half-page |
| `g` / `G` | top / bottom |
| `r` | reload from disk |
| `esc` / `q` / `tab` | back to sessions |

**Settings page (`,`)**

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | navigate rows |
| `space` / `enter` | toggle bool, cycle enum, edit int, run action |
| `esc` / `q` / `,` | back to sessions |

### Attach behaviour (`enter`)

ccdash discovers running sessions from `~/.claude/sessions/<pid>.json` and
its on-disk transcripts. Pressing `enter` does:

| State | Action |
| --- | --- |
| Running, in a tmux pane | `tmux switch-client -t <pane>` |
| Running, no tmux pane detected | flash with PID + TTY so you can switch terminals manually |
| Stopped | `claude --resume <session_id>` from the session's cwd |

When a session has pending permission requests its row tints yellow and
the header shows a `⚠ pending: N` badge. The terminal bell rings once
when the total pending count crosses 0 → 1.

## Secure mode

Settings page (`,`) exposes per-feature kill switches so the operator
can dial back ccdash's reach when they want pure observation:

- **Approval blocking**: when off, ccdash never holds PermissionRequest
  hooks — Claude shows its own prompt as before, and the `a` / `A` / `d`
  shortcuts are disabled.
- **Summarize via `claude -p`**: when off, `s` is disabled and no
  digest leaves the host.
- **Attach (enter)**: when off, `enter` only shows session info, never
  spawns `claude --resume` or runs `tmux switch-client`.
- **Auto-rewrite settings.json**: when off, server boot does *not*
  silently update `~/.claude/settings.json` even after a token rotation.

The **"Apply secure preset"** action flips all four to off in one shot
for a fully observation-only deployment.

## Threat model

ccdash is built for a single user managing their own Claude Code sessions
on a single workstation.

**Inside the trust boundary** (assumed honest):
- The local user account running ccdash and `claude`
- The `claude` binary and its on-disk state under `~/.claude/`
- Files the operator opens or edits via Claude

**Outside the trust boundary** (treated as adversarial):
- Other UNIX users on the same host
- Repositories the operator opens whose `.claude/settings.json` could try
  to inject hooks (Claude Code's concern; ccdash never writes into a
  project-scoped settings file, only `~/.claude/settings.json`)
- Network access of any kind — ccdash's collector binds to `127.0.0.1`
  and will not accept connections from other interfaces

**Explicitly out of scope**:
- Multi-user / shared-host deployments
- Public or LAN exposure of the collector
- Web UI / browser access (there is none — TUI only)
- Defense against the operator pasting their own secrets into prompts;
  the best ccdash can do is mask common token patterns before persisting
  them, and Claude already keeps a copy of every prompt in
  `~/.claude/projects/*.jsonl` regardless of ccdash

**Mitigations in this codebase**:
- Loopback-only bind, hardcoded — no flag changes the host
- DB file at `$XDG_STATE_HOME/ccdash/ccdash.sqlite` with `0600` permissions
- Hook entries in `~/.claude/settings.json` carry an `X-Ccdash-Managed`
  marker so `install-hooks` and `uninstall-hooks` round-trip them
  idempotently without disturbing other user hooks
- Random shared token at `$XDG_STATE_HOME/ccdash/token` (mode `0600`) is
  required on every hook + decision request, so other UNIX users on the
  same host can't forge events or approve tools by reaching the loopback
  port. The server auto-rewrites the hook headers when it rotates.
- Token-bucket rate limit on every authenticated route (50 QPS / 100
  burst) bounds the impact of runaway loops or scripted floods
- Pattern-based masking on hook payloads / titles / summaries before
  they reach the DB (Bearer tokens, `KEY=VALUE` env, AWS / GitHub /
  OpenAI / Anthropic key formats, URL credentials, etc.). The masking
  matters mainly for the summarize feature (which does send a digest
  over the network); on-disk Claude transcripts are unaffected.

## Layout

```
cmd/ccdash/main.go              CLI entry
internal/cli/                   cobra command tree
internal/server/                HTTP hook receiver (127.0.0.1:9123)
internal/db/                    SQLite layer (sessions / events / approvals / settings)
internal/model/                 data types
internal/tui/                   Bubble Tea UI
internal/transcript/            ~/.claude/projects/*.jsonl parser + tail reader
internal/discovery/             session-list discovery (sessions/<pid>.json + projects)
internal/procmap/               PID ↔ session_id ↔ tmux pane mapping
internal/summarize/             claude -p driver for the summary feature
internal/redact/                pattern-based secret masking
internal/auth/                  loopback shared-token loader
internal/settings/              persisted preferences (settings table + spec)
internal/selfupdate/            `ccdash update` self-replace logic
internal/hookcfg/               install-hooks settings.json merge
internal/wrapper/               `ccdash claude` exec wrapper
internal/paths/                 state dir / db / settings paths
internal/gitinfo/               git repo / branch / commit lookup
docs/research.md                Phase 0 research on Claude Code hook spec
install.sh                      curl-installable shell installer
.github/workflows/              CI + release pipelines
```

## State

- DB: `$XDG_STATE_HOME/ccdash/ccdash.sqlite` (default
  `~/.local/state/ccdash/`), `0600`
- Token: `$XDG_STATE_HOME/ccdash/token`, `0600`
- Server bind: `127.0.0.1:9123` (loopback only — never exposed externally)
- ccdash hook entries are tagged with the `X-Ccdash-Managed: true`
  header so install / uninstall are idempotent and don't collide with
  user hooks
</content>
