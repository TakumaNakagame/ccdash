# ccdash — Usage

A practical walkthrough for the day-to-day operator. Pairs with the
[README](../README.md), which is the high-level landing page; this doc
focuses on workflows, keybindings, and what each surface is for.

> 日本語版: [usage_jp.md](./usage_jp.md)

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

This appends ccdash's hook entries to `~/.claude/settings.json` —
idempotently, so you can re-run it any time. Existing user hooks are
preserved (they don't carry the `X-Ccdash-Managed: true` marker).
PermissionRequest is an HTTP hook (it waits for your allow / deny); every
other event runs a small forwarder script, `$XDG_STATE_HOME/ccdash/hook.sh`,
that posts in the background via `curl`, so a stopped ccdash never slows
claude down.

Confirm:

```sh
ccdash --version
```

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
- **Today** (sessions seen today)
- **Yesterday** (sessions seen the previous day)
- **This week** (2-7 days ago)
- **Earlier this month** (8-30 days)
- `Month YYYY` (older)

`f` toggles favorite. Favorites pin to the top of the list.

## 4. Group navigation

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
ccdash --group home-lab          # locks the dashboard, hides the strip
ccdash --group "deploy review"   # spaces are fine for user-named groups
```

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

`Ctrl+X` archives every session in the current group in one action.
Required confirmations:

1. The active filter must be a specific tab (not `All`) — bulk
   archiving "All" is refused as an obvious footgun.
2. ccdash asks `archive all N sessions in '<tab>'? press 'y'` —
   any other key cancels.
3. After confirmation, ccdash auto-advances to the next tab.

In archive view (`X` toggles), `Ctrl+X` becomes a bulk-unarchive of
the same tab.

## 7. Summarize (`s`)

Press `s` on a session to ask Claude to summarize its conversation.
ccdash builds a compact digest (USER prompts + CLAUDE replies + TOOL
calls, dropping noisy tool_results and thinking blocks). It then masks
common secret patterns through `internal/redact` and pipes the digest
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

### Session numbers and generated titles (`ctrl+t`)

Every session gets a short sequential number shown in front of its title,
e.g. `#4839 ccdash 改善`. Existing sessions are numbered in the order they
first appeared; search accepts `#4839` (or `4839`) to jump to one.

`ctrl+t` asks `claude -p` for a short, ticket-style title. The banner
offers two targets:

- `y` — the selected session only
- `a` — the **recent batch**: sessions in the current view (tab / search /
  account filter) active within the last 24 h that have no `t` rename and
  either no generated title yet or new activity since it was generated —
  newest first, at most 10. They all go to Claude in **one** call.

The list row shows `⏳ titling` while it runs. Title precedence is: your
`t` rename > generated title > first prompt. Generation shares the
summary's gate (`summary_enabled`) and timeout.

## 8. Attach (`enter`) and the live right pane

Pressing `enter` on a session attempts to bring it to you:

| Session state | Action |
| --- | --- |
| Running, in a tmux pane | `tmux switch-client -t <pane>` |
| Running, hosted by the ccdash server | focus the live pane (keys go to claude) |
| Stopped | `claude --resume <session_id>` from the session's cwd, hosted by the server, shown live in the right pane |

Sessions the ccdash server hosts (anything you started with `n`, or
resumed with `enter`) render as a **real terminal** in the right pane:
the server runs a terminal emulator on the PTY, the TUI mirrors its
screen, and the pane header reads `⬡ live`.

| Key | Action |
| --- | --- |
| `enter` / `ctrl+]` / click the pane | Give claude the keyboard. The terminal cursor moves into the pane so your IME composes in place. |
| `ctrl+]` / `ctrl+d` / click elsewhere | Back to the dashboard (claude keeps running) |
| `F` | Fullscreen: hand the whole terminal to the same session; `ctrl+d` returns |
| mouse wheel over the pane | Scrolls claude, not the transcript |
| click a session in the list | Select it (the right pane previews it) |
| double-click a session | Same as `enter` |

Opening (`enter` / double-click) a session whose claude is still running in another terminal asks first: a red **already running** window warns that resuming it here starts a second claude on the same conversation. Only `y` (or clicking **Yes**) opens it anyway; `n`, `esc`, `enter` or clicking **No** cancel. The restart confirmation has clickable **Restart** / **Cancel** buttons too. In both windows the arrow keys / `h` `j` `k` `l` / `tab` move between the buttons and `enter` presses the focused one (focus starts on **No** / **Restart**); other keys are ignored. Sessions in a tmux pane just switch to it.

While the pane has focus every other key goes to claude, including `q`.
The emulator lives in the server, so the screen survives TUI restarts
and is already drawn when you come back.

Sessions started elsewhere (another terminal, tmux) cannot be mirrored;
they keep the transcript tail, and `enter` switches tmux when a pane is
known. Once a stopped session is resumed through ccdash it becomes
hosted and goes live.

### Run a skill in a new session (`S`)

`S` lists the installed skills and slash commands (`~/.claude/skills`,
`~/.claude/commands`, and enabled plugins), plus project skills
(`<project>/.claude/skills`) of the projects your sessions ran in —
those are tagged with the project directory, and picking one starts the
directory step inside that project. Type to filter, `enter` to
pick, then choose the directory exactly as with `n` — the new session
starts as `claude "/<skill>"` and shows live in the right pane. `tab`
copies the highlighted name into the input so you can add arguments
(`/code-flow:ship-pr 123`); `enter` with no match sends the input as-is,
which covers built-ins like `/simplify`. In remote mode only typed
commands are offered.

## 9. Search (`/`)

Press `/` to open a footer search input. Submit with `Enter` to filter
the list by case-insensitive substring against:

- `s.DisplayTitle()` (custom title or auto-derived)
- `s.UserGroup`
- `s.Repo`
- `s.Cwd`
- `s.Branch`
- `s.Summary`
- `s.SessionID`

Search composes with the project filter and archive view by
intersection. The header shows `🔍 <query>` while a search is active;
press `Esc` (with the search input closed) to clear.

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

- **Approval blocking** — when off, ccdash never holds PermissionRequest
  hooks. Claude prompts in its own terminal as before; `a`/`A`/`d` are
  disabled.
- **Summarize via claude -p** — when off, `s` is disabled and no digest
  leaves the host.
- **Attach (enter)** — when off, `enter` only shows session info, never
  spawns subprocesses, and the live right pane stays disconnected.
- **Auto-rewrite settings.json** — when off, server start does not
  silently update `~/.claude/settings.json` even after a token rotation.
- **Hub connection** — when off, the collector does not connect to the
  hub joined via `ccdash hub join` (see §15), so the web portal can't
  reach this machine.

The **Apply secure preset** action flips all five to off in one shot.

### Layout

- **Vertical layout** (auto / on / off): auto picks horizontal vs
  vertical from terminal width. Default `auto`.
- **Vertical auto threshold (cols)**: the width at which auto-mode
  flips vertical. Default 100. The row shows your live terminal width
  next to the value, e.g. `(now: 142 cols, ≥ threshold)`.
- **Session list size (%)**: the session list's share of the screen
  (width side-by-side, height when vertical); the work pane gets the
  rest. Default 50, range 10–90. `<` / `>` on the dashboard adjust it
  in 5% steps.
- **Invert list scroll**: reverse the mouse wheel over the session
  list (wheel down moves the selection up). The right pane is
  unaffected. Default off.

### Update and restart

**Update ccdash** (system section, above Restart) checks for a new
release, shows its notes, installs it on `y`, and then opens the restart
confirmation so the new binary runs right away. On a dev build it only
explains how to update by hand.

### Restart

**Restart ccdash** — the last row, in the **system** section at the bottom of the settings page next to the running version — stops the collector and relaunches ccdash with the
same arguments, so a freshly installed binary (`go install`, `ccdash
update`) takes effect — quitting the TUI alone leaves the old collector
running. A confirmation window first lists what it breaks: live sessions
hosted by ccdash are stopped mid-reply (resume them with `enter`), and
pending approvals are released. Sessions started outside ccdash are not
affected. In remote mode it only relaunches the local dashboard.
- **Newest at bottom** — reverses the list so the most recent session
  is at the bottom (matching the transcript tail orientation).

### Tunables

- **Right-pane tail budget (KB)**: how much transcript is loaded for
  the live tail. Default 256 KB.
- **Summary timeout (s)**: ceiling for `claude -p`. Default 180.
- **Refresh interval (ms)**: how often the TUI re-queries the DB.
  Default 1000.

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

## 12. Uninstall

```sh
ccdash uninstall-hooks       # remove ccdash's entries from ~/.claude/settings.json
rm -rf ~/.local/state/ccdash # remove DB, token, log
rm $(which ccdash)           # remove the binary itself
```

`uninstall-hooks` only removes ccdash's own entries (tagged with
`X-Ccdash-Managed: true` or pointing at `ccdash/hook.sh`) and deletes
`hook.sh`; any other hooks you added stay in place.

## 13. Files and locations

| Path | Contents |
| --- | --- |
| `~/.claude/settings.json` | hook entries (managed by `install-hooks`) |
| `$XDG_STATE_HOME/ccdash/ccdash.sqlite` | sessions, events, approvals, settings |
| `$XDG_STATE_HOME/ccdash/token` | loopback shared-secret (mode 0600) |
| `$XDG_STATE_HOME/ccdash/hook.sh` | fire-and-forget hook forwarder (written by `install-hooks`) |
| `$XDG_STATE_HOME/ccdash/hub.json` | hub URL + device token from `ccdash hub join` (mode 0600) |
| `$XDG_STATE_HOME/ccdash/ccdash.log` | embedded-collector log when launched via the TUI |
| `/tmp/ccdash-server.log` | detached collector log (`-k` mode) |

`$XDG_STATE_HOME` defaults to `~/.local/state` on Linux/macOS.

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

## 15. Web portal (hub)

The hub is a web page that lists your machines and lets you use each one's
ccdash from a browser or phone. It runs on a home server
(`ccdash hub serve`, normally the `ghcr.io/takumanakagame/ccdash` container
behind your reverse proxy, with OIDC login — see README "Hub mode").

**Joining a machine**

1. In the portal: **Add device**, give it a name. A one-time token is shown.
2. On the machine:

   ```sh
   ccdash hub join --url https://ccdash.example.net   # paste the token
   ```

3. Keep a collector running there — `ccdash server` (e.g. a systemd user
   unit / launchd agent) or `ccdash -k`. It dials the hub within ~30 s; the
   dot in the device list turns green.

`ccdash hub status` / `ccdash hub leave` show and remove the join. The
machine never accepts inbound connections — its collector dials out.

**Using it**

- Pick a device → its sessions, grouped like the TUI's tabs, plus pending
  approvals with Allow / Always allow / Deny.
- Opening a session shows it as a **chat**: your prompts, Claude's replies,
  and tool calls (tap to expand). Type at the bottom and send (Enter on a
  keyboard, the button on a phone). If the session isn't running under
  ccdash, sending resumes it there with your message.
- When Claude shows a menu in its terminal (a permission dialog, plan
  approval, the "trust this folder?" question), a card with the relevant
  part of the screen and answer buttons appears above the input.
- **Terminal** switches to a full terminal view of the same claude (useful
  for anything the chat can't express); **Chat** goes back. **Stop**
  (`中断`) sends Esc while Claude is working; **End** (`終了`) stops the
  claude process — the session can be resumed later.
- **New session** picks a directory on the device and starts `claude` there.
- The session list has a **Latest** tab (everything, newest first, with
  date sections) and one tab per group, like the TUI's strip.
- **↑ Load older history** at the top of a chat reads further back.
- **＋** next to the input attaches images (camera on a phone); pasting or
  dropping an image works too. They're saved on the device under
  `$XDG_STATE_HOME/ccdash/uploads/` (kept 7 days) and attached to the prompt.
- When Claude asks a multiple-choice question, a **question card** appears:
  tap an option (single choice moves on by itself), toggle options and press
  **Next** for multi-select, or type into **Other**. A review step confirms
  multi-question sets.
- An open portal tab reloads itself when the hub is upgraded.
- **⋯** in a chat does what the TUI's per-session keys do: rename (`t`),
  set the group (`T`), favorite (`f`), archive (`x`), summarize (`s`) and
  generate a title (`ctrl+t`). **Archive** on the session list shows the
  archived ones. The summary appears at the top of the chat.
- **New session** takes an optional first message; typing `/` lists the
  device's skills and slash commands (project ones move the directory to
  their project), like the TUI's `S`.
- The device's settings are shown read-only at the bottom of its page —
  the hub can't change them.
- **🔔** in the header turns on push notifications for this browser (the
  installed app on a phone): a new approval, a question or confirmation on
  a ccdash-hosted claude's screen, and a session finishing its turn.
- On a wide screen (≥ 1024 px, i.e. a PC) a device opens TUI-style in two
  panes: the session list on the left, the selected session (chat, terminal
  or diff) on the right. Drag the divider (or focus it and use ← →) to resize; the width is
  remembered, and a double-click resets it to the device's TUI "Session
  list size".
- **Grid** (header): every running session across devices on one screen —
  needs-you first, then working, then idle — each tile with its state, the
  reason it wants you, the latest exchange (live) and a one-line reply.
  Tiles fill the screen (the column count is picked for the largest tiles;
  more sessions → smaller tiles); double-click a tile's title to maximize
  it (again or Esc to restore); ↗ opens the full chat.
- **Front page board**: 要対応 (needs you — approval, question, menu on
  screen) / 作業中 (working) / 未確認 (finished, not yet looked at), across
  all devices; opening a session marks it read. The app icon badge shows the
  count. In the TUI the same state shows as `?` / `✓` in the list and the
  header; `!` filters to those sessions.
- **差分 (diff)** in a chat opens the working-tree diff: tap a line to leave
  a note, then **Claude に送る** sends every note (and an optional overall
  comment) as one prompt.
- **Cost**: the `$` chip in a chat header shows that session's tokens and
  API-price estimate (by model, subagents included); a device page shows
  today / 7 days with a daily bar chart; the TUI header shows today. On a
  subscription this is an estimate at list price, not your bill.
- Above the input: **quick commands** (tap to send; ✎ edits them, shared
  across browsers), 🎤 voice input, `/` completion, and new sessions take a
  permission mode (manual / acceptEdits / auto / plan).
- **Subagents**: when a session has run subagents (the Agent tool), a bar
  under the chat header shows how many are running; open it for each one's
  type, description, elapsed time and latest tool call, and tap one for its
  own transcript and, once done, its report.
- The portal is a PWA: "Install app" / "Add to Home screen" gives it its own
  icon and window. Nothing is cached offline — it always talks to the hub.

**Read tokens for bots.** A machine client (e.g. an alert-triage bot) can
read the hub without logging in: start the hub with
`CCDASH_HUB_READ_TOKENS=<token>[,<token>…]` or `--read-token-file <file>`
(tokens ≥ 32 characters) and send `Authorization: Bearer <token>`. Only
these GETs work: `/api/board`, `/api/active`, `/api/devices`,
`/api/d/{id}/api/sessions`, `/api/d/{id}/api/sessions/{sid}/transcript`
(`?lines=20` for the last 20 JSONL records; tail only). Everything else is
403. Response shapes are in README "Hub mode".

```sh
curl -H "Authorization: Bearer $TOKEN" https://ccdash.example.net/api/board
curl -H "Authorization: Bearer $TOKEN" \
  "https://ccdash.example.net/api/d/$DEV/api/sessions/$SID/transcript?lines=20" \
  | jq -r .data | base64 -d
```

What the portal may do is limited by the device's own settings: with
**Attach** off it can only read; with **Approval blocking** off it can't
decide approvals; with **Hub connection** off the device is offline.
