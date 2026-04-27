# ccdash — Usage

A practical walkthrough for the day-to-day operator. Pairs with the
[README](../README.md), which is the high-level landing page; this doc
focuses on workflows, keybindings, and what each surface is for.

> 日本語版: [usage_jp.md](./usage_jp.md)

---

## 1. First-time setup

Install the binary (latest GitHub release):

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
```

The script picks an install dir that's already on `PATH` for your host:

| Host | Default location |
| --- | --- |
| macOS with Homebrew | `$(brew --prefix)/bin` |
| macOS without Homebrew (writable `/usr/local/bin`) | `/usr/local/bin` |
| Linux / fallback | `~/.local/bin` |

Override with `CCDASH_INSTALL_DIR=/custom/path` if you want it elsewhere,
or pin a specific version with `CCDASH_VERSION=v0.1.x` (handy when the
GitHub anonymous API rate-limit hits).

Wire ccdash into Claude Code once:

```sh
ccdash install-hooks
```

This appends ccdash's HTTP hook entries to `~/.claude/settings.json` —
idempotently, so you can re-run it any time. Existing user hooks are
preserved (they don't carry the `X-Ccdash-Managed: true` marker).

Confirm:

```sh
ccdash --version
```

---

## 2. Daily workflow

Run `claude` as you normally would. ccdash watches what you do and
shows it in the dashboard.

```sh
claude               # in one tmux pane / terminal tab
ccdash               # in another — opens the TUI and embeds the
                     # collector for the lifetime of the TUI
```

Closing the TUI with `q` shuts the collector down. Hook events fired
while the TUI is closed are not recorded.

If you want events recorded even when the dashboard is closed:

```sh
ccdash -k            # spawns a detached collector, then opens the TUI;
                     # the collector outlives the TUI

# or run a foreground collector you manage yourself:
ccdash server &
ccdash               # picks up the existing server
```

The detached collector logs to `/tmp/ccdash-server.log`. Stop it with
`pkill -f 'ccdash server'`.

---

## 3. Reading the dashboard

```
ccdash                                              sessions: 4   ⚠ pending: 1   12:34:56
─────────────────────────────────────────────────────
  All     home-lab   ccmanage   deploy review        ← tab strip
─────────────────────────────────────────────────────
▶ ● 1m   @task.md の内容から、具体的な作業内容を   transcript  (a574854b)
         確認して
         ccmanage:HEAD · a574854b · pid:394903 · ⚠1   USER
                                                       @task.md ...
  ● 10m  @task.md を参照して実装しましょう…           CLAUDE
         home-lab:main · 66eec245 · pid:179833         はい、内容を確認します
                                                       ...
─────────────────────────────────────────────────────
↑/↓ sel  h/l tabs  /search  enter attach  ...         ← key hint footer
```

Left pane is the session list. Right pane is the live transcript tail
of the selected session. Tabs filter the list by repo or operator-named
tab; `All` shows every session.

### Status dot legend

| Color | Status | Meaning |
| --- | --- | --- |
| 🟢 green | `active` | Claude is processing (`status: busy`) |
| 🔵 cyan | `idle` | Claude is alive, awaiting input |
| 🟡 yellow | `recent` | Process exited within the last 6 hours |
| ⚪ gray | `stopped` | Process exited > 6 h ago |

### Date grouping

Sessions are bucketed by `last_seen`:

- ★ **Favorites** (anything pinned, regardless of date)
- **Today**
- **Yesterday**
- **This week** (2-7 days ago)
- **Earlier this month** (8-30 days)
- `Month YYYY` (older)

`f` toggles favorite. Favorites pin to the top of the list.

---

## 4. Tab navigation

The strip across the top shows every distinct grouping. The active tab
is highlighted; cycle with `h` / `l` (or `Tab` / `Shift+Tab`). When the
total exceeds the terminal width the strip slides and surfaces `‹` `›`
arrows for off-screen entries.

A tab can be:

- **Auto-derived** from the session's repo name (`s.Repo`) or cwd
  basename. Toggleable in the settings page (`R`).
- **Operator-named** via `T` on a session. The custom name overrides
  the auto-derived one. Multiple sessions can share the same custom
  tab — e.g. group `frontend-repo` + `backend-repo` sessions under
  `feature-x`.

If the active tab disappears (you archive the last session in it,
discovery rotates one out), ccdash auto-advances to the next tab.

Pinning to a single tab at launch:

```sh
ccdash --tab home-lab          # locks the dashboard, hides the strip
ccdash --tab "deploy review"   # spaces are fine for user-named tabs
```

---

## 5. Right-pane transcript

The transcript pane streams the latest USER / CLAUDE / TOOL exchanges
of the selected session. It tail-reads the session's
`~/.claude/projects/<...>/<session_id>.jsonl` file (last 256 KB by
default) so even very long histories scroll snappily.

Each role gets its own background tint:

- **USER** — dark blue
- **CLAUDE** — dark green
- **TOOL `<name>`** — teal
- **↳ result** — dark gray
- **↳ ERROR** — dark red

Tool calls and their results are visually attached: no blank line
between them, and the result body is indented one level deeper.

### Scrolling

| Key / Action | Effect |
| --- | --- |
| `Shift+J` / `Shift+K` | Scroll the transcript one line newer / older |
| `pgdn` / `pgup` | Half-page in the same direction |
| Mouse wheel (over the right pane) | Scroll, identical semantics |
| Shift+J back to bottom | Resumes auto-tailing the latest content |

The scroll offset (`tailScroll`) resets to 0 — meaning "auto-tail at
bottom" — whenever you switch sessions.

### Full-screen viewer (`o`)

For longer reading sessions, press `o` on a session to open the full
transcript modal. This loads the entire JSONL (not just the tail) and
lets you scroll through everything.

---

## 6. Approvals

By default, ccdash holds the `PermissionRequest` hook for up to 25
seconds, giving you a window to decide via the TUI before Claude falls
back to its own interactive prompt.

When a session has pending approvals:

- The session row tints yellow with a `⚠ N pending` badge
- The right-pane bottom shows the approval panel with the pending
  tool name and input
- The header shows `⚠ pending: N` for the dashboard total
- The terminal bell rings once when the count crosses 0 → 1

### Decision keys

| Key | Decision |
| --- | --- |
| `a` | Allow the oldest pending approval (one-shot) |
| `A` | Allow + remember the rule for this session (e.g. `Bash(git *)`) |
| `d` | Deny the oldest pending approval |

`A` (keep-allow) sends an `updatedPermissions` block back to Claude
with `scope: "session"` so future equivalent calls don't re-prompt.
For `Bash` commands, the rule globs on the first whitespace-separated
token (`Bash(git status)` becomes `Bash(git *)`).

### Bulk archive

`Ctrl+X` archives every session in the current tab in one action.
Required confirmations:

1. The active filter must be a specific tab (not `All`) — bulk
   archiving "All" is refused as an obvious footgun.
2. ccdash asks `archive all N sessions in '<tab>'? press 'y'` —
   any other key cancels.
3. After confirmation, ccdash auto-advances to the next tab.

In archive view (`X` toggles), `Ctrl+X` becomes a bulk-unarchive of
the same tab.

---

## 7. Summarize (`s`)

Press `s` on a session to ask Claude to summarize its conversation.
ccdash builds a compact digest (USER prompts + CLAUDE replies + TOOL
calls, dropping noisy tool_results and thinking blocks), runs it
through `internal/redact` to mask common secret patterns, and pipes it
to `claude -p` in an isolated subprocess.

The first press shows a `y/n` confirmation banner. After confirming:

- The list row gains a `⏳ summarizing` indicator
- Up to 180 seconds (configurable) of background work
- On success: the summary is inserted into the transcript stream at
  the time of generation, with a `summary <age> ago` label. As new
  activity comes in, the summary scrolls up like any other old
  message.
- On failure: a `✗ summary error` indicator and the error text inline

The `claude -p` spawn is isolated with `--setting-sources project` and
cwd `/tmp` so it doesn't inherit ccdash's hooks (otherwise the spawn
would create another session in the dashboard).

---

## 8. Attach (`enter`)

Pressing `enter` on a session attempts to switch focus to it:

| Session state | Action |
| --- | --- |
| Running, in a tmux pane | `tmux switch-client -t <pane>` |
| Running, no tmux pane detected | flash with PID + TTY so you can switch terminal apps manually |
| Stopped | `claude --resume <session_id>` from the session's cwd |

Tmux integration is automatic when the session's pane is detected via
`tmux list-panes`. No extra setup needed — install tmux, run `claude`
inside a tmux pane, and `enter` becomes a single-keystroke pane switch.

---

## 9. Search (`/`)

Press `/` to open a footer search input. Submit with `Enter` to filter
the list by case-insensitive substring against:

- `s.DisplayTitle()` (custom title or auto-derived)
- `s.UserTab`
- `s.Repo`
- `s.Cwd`
- `s.Branch`
- `s.Summary`
- `s.SessionID`

Search composes with the project filter and archive view by
intersection. The header shows `🔍 <query>` while a search is active;
press `Esc` (with the search input closed) to clear.

---

## 10. Settings page (`,`)

Open with `,` from the sessions view. Keys:

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | Navigate rows |
| `space` / `enter` | Toggle bool, cycle enum, edit int, run action |
| `esc` / `q` / `,` | Back to the sessions view |

Settings persist across launches in `settings` table of the DB.

### Risk-bearing toggles

These let you scale ccdash's reach down to "observation only":

- **Approval blocking**: when off, ccdash never holds PermissionRequest
  hooks. Claude prompts in its own terminal as before; `a`/`A`/`d` are
  disabled.
- **Summarize via claude -p**: when off, `s` is disabled and no digest
  leaves the host.
- **Attach (enter)**: when off, `enter` only shows session info, never
  spawns subprocesses.
- **Auto-rewrite settings.json**: when off, server start does not
  silently update `~/.claude/settings.json` even after a token rotation.

The **Apply secure preset** action flips all four to off in one shot.

### Layout

- **Vertical layout** (auto / on / off): auto picks horizontal vs
  vertical from terminal width. Default `auto`.
- **Vertical auto threshold (cols)**: the width at which auto-mode
  flips vertical. Default 100. The row shows your live terminal width
  next to the value, e.g. `(now: 142 cols, ≥ threshold)`.
- **Newest at bottom**: reverses the list so the most recent session
  is at the bottom (matching the transcript tail orientation).

### Tunables

- **Right-pane tail budget (KB)**: how much transcript is loaded for
  the live tail. Default 256 KB.
- **Summary timeout (s)**: ceiling for `claude -p`. Default 180.
- **Refresh interval (ms)**: how often the TUI re-queries the DB.
  Default 1000.

---

## 11. Self-update

```sh
ccdash update
```

Reaches GitHub, finds the latest release, downloads the matching
asset, verifies the sha256 sidecar, and replaces the running binary
with `os.Rename`. No-ops when already on the latest tag.

If the GitHub API rate-limits the lookup (60/hr anonymous), the error
includes the hint and the install script accepts an explicit version
override:

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh \
  | CCDASH_VERSION=v0.1.3 sh
```

---

## 12. Uninstall

```sh
ccdash uninstall-hooks       # remove ccdash's entries from ~/.claude/settings.json
rm -rf ~/.local/state/ccdash # remove DB, token, log
rm $(which ccdash)           # remove the binary itself
```

`uninstall-hooks` only removes entries tagged with `X-Ccdash-Managed:
true`; any other hooks you added stay in place.

---

## 13. Files and locations

| Path | Contents |
| --- | --- |
| `~/.claude/settings.json` | hook entries (managed by `install-hooks`) |
| `$XDG_STATE_HOME/ccdash/ccdash.sqlite` | sessions, events, approvals, settings |
| `$XDG_STATE_HOME/ccdash/token` | loopback shared-secret (mode 0600) |
| `$XDG_STATE_HOME/ccdash/ccdash.log` | embedded-collector log when launched via the TUI |
| `/tmp/ccdash-server.log` | detached collector log (`-k` mode) |

`$XDG_STATE_HOME` defaults to `~/.local/state` on Linux/macOS.

---

## 14. Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| TUI is empty, no sessions | Hooks not installed or stale token. Re-run `ccdash install-hooks`. |
| All hook events return 401 in the log | Token mismatch; `ccdash install-hooks` rewrites the headers, or the server's auto-sync (default on) does it on next boot. |
| `claude -p failed: signal: killed` (summary) | Summary hit the timeout. Bump `summary_timeout_sec` in the settings page. |
| `vertical_auto_cols` flipped layout at the wrong width | Adjust the threshold in the settings page (the row shows your current width). |
| `failed to resolve latest release tag` (install/update) | Anonymous GitHub API rate limit. Use `CCDASH_VERSION=v0.1.x` to skip the API. |
| Pending count keeps growing | Approval auto-resolution failed (the discovery loop sweeps stale pendings to `timeout` after 45 s; restart the server if it's stuck). |

For anything more specific, the embedded-collector log
(`~/.local/state/ccdash/ccdash.log`) is the first place to look.
