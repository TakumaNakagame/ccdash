# ccdash — Usage

A practical walkthrough for the day-to-day operator. Pairs with the
[README](../README.md), which is the high-level landing page; this doc
focuses on workflows, keybindings, and what each surface is for.

> 日本語版: [usage_jp.md](./usage_jp.md)

**Who it's for**: one person managing their own Claude Code sessions from
the machines and phones they own. Keep it to yourself — don't give anyone
else the hub login, the collector token or a device token. See the
README's [Who it's for](../README.md#whos-it-for) for what isn't supported.

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
ccdash setup          # hooks + the MCP server + the status rule (recommended)
ccdash install-hooks  # just the hooks
```

`ccdash setup` does what `install-hooks` does, registers ccdash's MCP server
at user scope in `~/.claude.json` and adds `mcp__ccdash__set_status` to
`permissions.allow` (see "Status declarations" in section 16). Safe to re-run.

`install-hooks` appends ccdash's hook entries to `~/.claude/settings.json` —
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
↑/↓ sel  h/l tabs  / search  enter open  ...  ? help  q quit  ← key hint footer
```

Left pane is the session list. Right pane is the live transcript tail
of the selected session. Tabs filter the list by repo or operator-named
tab; `All` shows every session.

The footer only shows the common keys and always ends with `? help  q quit`
(on a narrow terminal the middle is trimmed). `?` opens a help window that
lists every key; scroll it with `j` / `k`.

### Status dot legend

| Color | Status | Meaning |
| --- | --- | --- |
| 🟢 green | `active` | Claude is processing (`status: busy`) |
| 🔵 cyan | `idle` | Claude is alive, awaiting input |
| 🟡 yellow | `recent` | Process exited within the last 6 hours |
| ⚪ gray | `stopped` | Process exited > 6 h ago |

### Date grouping

Sessions are bucketed by `last_seen`:

- 📌 **Pinned** (pinned sessions, regardless of date — the most recently pinned first)
- **Today** (sessions seen today)
- **Yesterday** (sessions seen the previous day)
- **This week** (2-7 days ago)
- **Earlier this month** (8-30 days)
- `Month YYYY` (older)

`f` pins / unpins a session. Pins stay at the top of the list (inside a project, at the top of its block), the most recently pinned first, and activity never moves them.

`L` marks a session **watch later**: marked sessions gather in a ⏰ block at the very top (most recently marked first) until you press `L` again. Rows show **NEW** when the session made progress after you last looked at it (selected it in the TUI, or had its chat open in the portal).

`O` switches the list to **recent order**: every session in one run, the one that moved last first, ignoring projects, pins and watch later (only date groups; each row names its project in its color). `O` again goes back to project blocks. The portal has the same as the **更新順 / Recent** button next to the search box.

**Orchestrators and their children** (see the orchestrator section below) are laid out as in the portal: global orchestrators stay at the top under a **統括** heading, a project's orchestrator leads its project block, and a session started by another one is indented under it with `` `- `` (two levels at most; `↳A12` in front of a title names the parent when it is not the row above). A row with children shows `子 N · 判断待ち k` — how many sessions are below it and how many of those wait on you — and so does the project heading.

| Key | What it does |
|-----|--------------|
| `z` | Fold / unfold a global orchestrator's own children (on the orchestrator or any row under it; `[+]` marks a folded one). Remembered until you quit. |
| `Z` | Show only the selected session and everything below it. A line over the list says so; `Z` or `esc` shows everything again. |
| `m` | The selected session's children in a window, grouped 判断待ち (anywhere below it) / 作業中 / 報告あり / 終わり. `enter` jumps to that session in the list — answer its approval there with `a` / `A` / `d` as usual; the window itself answers nothing. |

With `newest_at_bottom` the 統括 block is at the bottom with the other "newest end" blocks; a parent still comes before its children.

Sessions in a **project** (`p`, see §7) are pulled out of these date
buckets and shown as project blocks at the newest end of the list.

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

### Auto-archive

Once per local day — on the collector's first discovery tick of the day,
or the first tick after midnight for a collector left running — sessions
whose `last_seen` is older than **Auto-archive after (days)** (settings
page, default 7, `0` = off) are archived. Pinned sessions, project members and
running (active / idle) sessions are never auto-archived. Resuming an
auto-archived session (it becomes active / idle again, or shows newer
activity) brings it back automatically. Sessions you archived yourself
with `x` stay archived.

## 7. Projects (`p`) and generated titles

### Projects

A project is an operator-named set of sessions — e.g. the three sessions
working on one feature across repos. Press `p` on a session to put it in
one: the prompt shows the existing projects as chips in their colors —
including projects whose sessions are all archived — (`↑` `↓` to pick one;
typing filters them), and typing a name that isn't listed creates that
project on `enter`. The last chip, `✕ remove from <project>`, takes the
session out; an empty input changes nothing. Blank lines keep the project
blocks apart in the list. `P` re-rolls the project's color; new projects get a color kept
apart from the other projects' colors.

Project members are moved to the newest end of the list (the top, or the
bottom with **Newest at bottom**), separate from the date buckets. Each
project is one block and the order never changes with activity: new
projects appear at the newest end, `[` / `]` move the selected session's
project up / down (▲ ▼ on the band in the portal, `order_projects` over
MCP), and sessions inside a block are newest-started first. Each block is
headed by a full-width band in the project's color:

```
 PROJECT feature-x  3 sessions · 1 running · 1 needs you
▌● 1m   #4839 API の型を直す
▌○ 2h   #4840 フロントの表示を合わせる
```

and every member row carries a one-cell gutter in that color. Search
(`/`) matches project names too.

Press `n` on a project member and the directory picker starts in the
project's directory (the cwd of its most recently active session); the new
session joins the project automatically once the collector learns its
session ID. In the portal, the **＋** on a project band does the same.

### Session numbers and generated titles (`ctrl+t`)

Every session gets a short sequential number shown in front of its title,
e.g. `#4839 ccdash 改善`. Existing sessions are numbered in the order they
first appeared; search accepts `#4839` (or `4839`) to jump to one.

Sessions are **titled automatically**: once a session has had two typed
prompts — or one, followed by 2 minutes of quiet — the collector titles it
with the same batched `claude -p` run (up to 10 sessions per call, at most
one call a minute, only sessions active in the last 24 h, once per session;
a failed attempt isn't retried). Turn it off with **Auto-generate titles**
(`auto_title`); it also needs **Title generation via claude -p**.

`ctrl+t` asks `claude -p` for a title on demand (or again). The banner
offers two targets:

- `y` — the selected session only
- `a` — the **recent batch**: sessions in the current view (tab / search /
  account filter) active within the last 24 h that have no `t` rename and
  either no generated title yet or new activity since it was generated —
  newest first, at most 10. They all go to Claude in **one** call.

The list row shows `⏳ titling` while it runs. Title precedence is: your
`t` rename > generated title > first prompt. Generation is gated by
**Title generation via claude -p** (`summary_enabled`) and bounded by
**Title generation timeout (s)**. The digest sent to Claude is masked
through `internal/redact` first.

The `claude -p` spawn is isolated with `--setting-sources project` and
cwd `/tmp` so it doesn't inherit ccdash's hooks (otherwise the spawn
would create another session in the dashboard).

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

While a live session is selected but the pane is not focused, the
dashboard keys still work — the footer lists `p` / `t` / `ctrl+t` / `x`
alongside `enter` / `F`.

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
- `s.Project`
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
- **Title generation via claude -p** — when off, `ctrl+t` is disabled
  and no digest leaves the host.
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

### Restart once the sessions are idle (scheduled)

When the collector runs as `ccdash server` (or `ccdash web`), the first
button of that window is **When idle**: it *schedules* the restart instead
of doing it now.

- Every 2 s the collector looks for hosted sessions that are working, hold
  an approval, show a question / dialog on screen, or declared a background
  wait (`set_status` background). Once none has been busy for a moment it
  restarts and resumes them all with `claude --resume`, so nothing is cut
  off. Sessions waiting for an answer, an action or done reporting don't
  hold it up: their text survives the resume.
- Meanwhile everyone is warned: an orange band under the TUI header, a note
  at the top of every portal page (and the Android app) — "ccdash を再起動予約中
  (待ち: #12 作業中…)" — and a push from the hub.
- Cancel it with **Cancel schedule** (Restart ccdash in the TUI), **取り消す**
  on the portal's note, or `ccdash restart --cancel`.
- It gives up after 2 h (`ccdash restart --timeout 30m`, at most 24 h)
  rather than force anything.
- Input isn't blocked: a new prompt just pushes the restart back.
- The TUI's When idle restarts the collector only; the TUI keeps its
  version until you relaunch it.

From a shell (`-r` drives a remote collector):

```sh
ccdash restart            # schedule
ccdash restart --status   # what it waits for
ccdash restart --cancel   # cancel
```

`ccdash update` points at it when an old collector is still running. The
portal's device-page restart button offers 作業が終わったら再起動 / 今すぐ再起動.
- **Newest at bottom** — reverses the list so the most recent session
  is at the bottom (matching the transcript tail orientation).

### Tunables

- **Right-pane tail budget (KB)**: how much transcript is loaded for
  the live tail. Default 256 KB.
- **Title generation timeout (s)**: ceiling for `claude -p`. Default 180.
- **Auto-archive after (days)**: archive sessions idle longer than this,
  once a day (see "Auto-archive" in §6). Default 7, `0` = off.
- **Refresh interval (ms)**: how often the TUI re-queries the DB.
  Default 1000.
- **Language**: `auto` (from `$LC_ALL` / `$LC_MESSAGES` / `$LANG`: `ja*`
  = Japanese, anything else = English), `en` or `ja`. Covers ccdash's own
  text in the TUI (task pane, header counts, reasons) and in `ccdash tasks`.
  The portal has its own per-user choice under ⚙ Display → Language
  (default: the browser's language); push notifications follow it.
- **Trusted folders**: comma-separated folders (`~/` allowed), e.g.
  `~/src`. When ccdash starts claude in one of them or
  any subfolder (`n`, the portal, MCP `start_session`), it first marks that
  folder trusted in `~/.claude.json`, so a brand-new folder doesn't stop at
  Claude Code's "do you trust this folder?" question. Empty = off. From a
  shell: `ccdash trust` lists, `ccdash trust <dir>` adds,
  `ccdash trust --remove <dir>` removes. A spawn
  that does stop at a dialog before it has a session is flagged anyway:
  a "waiting for you" row in the TUI, 要対応 under 起動中 in the portal
  (plus a push), and `waiting_on` in MCP `start_session` / `overview`.
- **Session setup command**: a shell command the collector runs before
  every session an MCP `start_session` starts (see "Notes for handed-over
  work" under MCP). Empty = off. Can only be changed on the machine itself.
- **Default model** / **Default effort**: the model (`fable`, `opus`,
  `sonnet`, `haiku` or a full model id) and effort (`low` … `max`) of every
  NEW session ccdash starts (`n`, the portal, MCP `start_session`, quick
  requests), passed as `claude --model` / `--effort`. Empty / `default` =
  leave it to claude — which keeps using the model you last picked in any
  session, so one expensive pick sticks; set a default here to stop that.
  The portal's new-session dialog and MCP `start_session` can choose
  another one for a single session. Never applied when a session is
  resumed.
- **Ignored folders**: comma-separated folders (`~/` allowed) whose
  headless claude runs (`claude -p`, e.g. a cron job) ccdash never records —
  no row in the list, the archive, the portal, MCP or the unread counts, and no
  session number (your sessions' `#N` stay consecutive).
  Sessions ccdash starts there itself, and interactive ones, still show.
  A script can instead opt a single run out with `CCDASH_IGNORE=1` in
  claude's environment (forwarded by the hooks; ccdash's own spawns never
  inherit it). A run that only ever fires SessionEnd (`claude mcp list` and
  other subcommands) is never recorded either. From a shell:
  `ccdash ignore` lists, `ccdash ignore <dir>` adds, `ccdash ignore --remove
  <dir>` removes; `ccdash ignore prune [--dir <dir>]` lists rows earlier
  automated runs left behind and `--yes` deletes them (pinned / project /
  renamed / task-linked rows are never touched).

## 11. Self-update

```sh
ccdash update
```

Reaches GitHub, finds the latest release, downloads the matching
asset, verifies the sha256 sidecar, and replaces the running binary
with `os.Rename`. No-ops when already on the latest tag.

"Latest" depends on the machine's default channel: `stable` (tagged
releases, the default) or `dev` (pre-releases / rc builds too). The TUI's
update check and settings → Update ccdash follow the same default.

```sh
ccdash update channel           # print the default channel
ccdash update channel dev       # follow rc builds from now on (stable to go back)
ccdash update --channel=dev     # just this once
CCDASH_CHANNEL=dev sh install.sh   # a fresh install from the dev channel
```

If the GitHub API rate-limits the lookup (60/hr anonymous), the error
includes the hint and the install script accepts an explicit version
override:

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh \
  | CCDASH_VERSION=v0.1.3 sh
```

## 12. Uninstall

```sh
ccdash uninstall-hooks                 # remove ccdash's entries (and the status-line relay) from ~/.claude/settings.json
claude mcp remove --scope user ccdash  # unregister the MCP server added by `ccdash setup`
ccdash hub leave                       # if joined to a hub
rm -rf ~/.local/state/ccdash ~/.config/ccdash   # DB, token, logs, client config
rm "$(command -v ccdash)"              # the binary itself
```

`ccdash setup` also added `mcp__ccdash__set_status` to `permissions.allow`
in `~/.claude/settings.json`; it is harmless, delete it by hand if you like.

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
| `claude -p failed: signal: killed` (titles) | Title generation hit the timeout. Bump **Title generation timeout (s)** (`summary_timeout_sec`) in the settings page. |
| `vertical_auto_cols` flipped layout at the wrong width | Adjust the threshold in the settings page (the row shows your current width). |
| `failed to resolve latest release tag` (install/update) | Anonymous GitHub API rate limit. Use `CCDASH_VERSION=v0.1.x` to skip the API. |
| Pending count keeps growing | Approval auto-resolution failed (the discovery loop sweeps stale pendings to `timeout` after 45 s; restart the server if it's stuck). |

For anything more specific, the embedded-collector log
(`~/.local/state/ccdash/ccdash.log`) is the first place to look.

## 15. Web portal (hub)

The hub is a web page that lists your machines and lets you use each one's
ccdash from a browser or phone. It runs on a home server
(`ccdash hub serve`, normally the `ghcr.io/takumanakagame/ccdash` container
behind your reverse proxy, with OIDC login — see [Hub mode](hub.md)).
It is for you alone: allow only your own login addresses and join only
your own machines.

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
- The paperclip next to the input attaches files (images, PDFs, text …,
  20 MB each); pasting or dropping works too. They're saved on the device
  under `$XDG_STATE_HOME/ccdash/uploads/` with the original name (kept 7
  days) and their paths go into the prompt (an image becomes
  `[Image #N]`, anything else Claude reads).
- Typing `/` in the input lists the slash commands and skills the session
  can run (its own list from the transcript plus the device's skills) — at
  the start of the message or after a space anywhere in it; ↑↓ + Enter /
  Tab or a tap picks, Esc closes. The **/** button types a `/` at the
  caret.
- A `! command` line in Claude's reply (a plain `! cmd` line, inline code or
  a `!` line in a code block) gets a **▶ コマンドを実行する** button: a dialog shows the command, and
  **Yes** sends it to that session's claude in `!` (bash) mode. The output
  shows up as **Shell output**. Only for sessions running under ccdash.
- When Claude asks a multiple-choice question, a **question card** appears:
  tap an option (single choice moves on by itself), toggle options and press
  **Next** for multi-select, or type into **Other**. A review step confirms
  multi-question sets.
- An open portal tab reloads itself when the hub is upgraded.
- **⋯** in a chat does what the TUI's per-session keys do: rename (`t`),
  set the group (`T`), set the project (`p`, a name field with the
  existing names offered) and its color (swatches / 🎲, like `P`),
  pin (`f`), archive (`x`) and generate a title (`ctrl+t`).
  **Archive** on the session list shows the archived ones.
- In the session list, project members are grouped under a band in the
  project's color, their rows tinted with a project stripe. With several
  hosts, a project name is one band (and one grid group) whatever host its
  sessions run on, in the color of the host with the first letter; sessions on
  different hosts can be put in the same project.
- The session list always merges every host. A host is picked only when a
  session is started (a project band's ＋ defaults to the host that project
  used last). To see one host only, pick it under ⚙ Display → "Hosts in
  the session list" (a device page opened from Home shows just that one).
- **New session** takes an optional first message; typing `/` lists the
  device's skills and slash commands (project ones move the directory to
  their project), like the TUI's `S`.
- **Run skill** on a device page is the TUI's `S` picker: the device's
  skills and slash commands (project skills of recent session directories
  first), filtered as you type; ↑↓ + Enter or a tap picks one, `Tab`
  completes the name so arguments can follow, and input with no match is
  sent as `/<input>`. It opens the new-session dialog with `/<skill>` as
  the first message (a project skill starts in its project).
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
- **Grid** (header button toggles it on/off): the sessions you choose on
  one screen. With none chosen it shows what wants you or is working
  (needs you, working, finished unseen); × on a tile takes it off (and
  pins the rest), **＋ 追加** places any session not already there (stopped
  ones too); ▦ on a session-list row toggles it too. The selection lives on
  the devices, so every browser and the TUI's grid (`V` / `space`) share it. A stopped session's tile has **▶ 再開** (`claude --resume`;
  replying resumes as well). Each tile shows its state, the
  reason it wants you, the latest exchange (live) and a one-line reply.
  Tiles fill the screen (the column count is picked for the largest tiles;
  more sessions → smaller tiles); double-click a tile's title to maximize
  it (again or Esc to restore); ↗ opens the full chat. **自由配置** (free
  placement) turns tiles into windows: drag a title bar to move, the
  bottom-right corner to resize; positions are remembered per browser.
  **並べ直す** tiles them again, **整列** returns to the automatic grid.
- **List checkboxes**: tick sessions to put them on / take them off the
  grid, archive (or unarchive) them, put them in a project (**▤
  プロジェクト**), or generate titles (claude -p, ten per call) in one go.
- Auto-refresh holds while you have text selected inside the chat, list or a
  tile, so drag-to-copy works.
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
  across browsers), 🎤 voice input, `/` completion (the **/** button left
  of the input opens the list at any time; with text already typed the
  command goes in front of it), and new sessions take a permission mode
  (manual / acceptEdits / auto / plan), a model (default / fable / opus /
  sonnet / haiku) and an effort (default / low … max). Model and effort
  always open on "default" — the host's **Default model** / **Default
  effort** settings, shown in brackets — and the last pick is not
  remembered.
- In a session ccdash hosts, tap the model name in the status line (or pick
  `/model` from the completion) for a model menu — default / Opus / Sonnet /
  Haiku / Fable / Opus Plan / any model id; it sends `/model <name>`.
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
403. Response shapes are in [Hub mode](hub.md).

```sh
curl -H "Authorization: Bearer $TOKEN" https://ccdash.example.net/api/board
curl -H "Authorization: Bearer $TOKEN" \
  "https://ccdash.example.net/api/d/$DEV/api/sessions/$SID/transcript?lines=20" \
  | jq -r .data | base64 -d
```

What the portal may do is limited by the device's own settings: with
**Attach** off it can only read; with **Approval blocking** off it can't
decide approvals; with **Hub connection** off the device is offline.
- **On a phone**: swipe right to go back (session → device list → board; from the left edge too when installed as an app, elsewhere in the browser, whose own gesture owns the edge). Opening a session no longer pops the keyboard.

### When Claude Code's login expires

Claude Code keeps one login per machine. When it expires, that machine's sessions stop on "Login expired · Please run /login" and ccdash marks them "Login expired" (needs you). The portal and the app show a banner ("Claude Code's login has expired" on that machine) and one push per machine.

1. Press **Log in again** on the banner (or the card in the session's chat). The machine's collector runs `claude auth login` and shows the sign-in URL.
2. **Open the sign-in page** (or **Copy URL**) in any browser and allow it.
3. Paste the code it shows and press **Send**. The account now signed in is shown.

Every session on that machine works again. Sessions ccdash runs are told to go on; the others become "Logged in again: send the next prompt". The banner also shows 3 days before the login's time limit. It needs **Attach** on for that machine. The URL and the code are never stored.

### Local portal (`ccdash web`)

The portal for this machine only, on `127.0.0.1`, with no hub server —
for a machine nobody may reach from outside (a work laptop) where you
still want the web UI.

```sh
ccdash web          # starts http://127.0.0.1:9124 in the background and opens a one-time login link
ccdash web open     # log in again later (another browser, expired cookie, launchd)
ccdash web status   # running? pid, URL, log ($XDG_STATE_HOME/ccdash/web.log)
ccdash web restart  # restart; hosted claude sessions are resumed
ccdash web stop     # stop it (add --resume to have the next start resume the sessions)
ccdash web -f       # --foreground: stay in this terminal (launchd / systemd)
```

- Runs in the background: the prompt comes back at once and closing the
  terminal doesn't stop it.
- Links to a running collector, or runs one itself (like `ccdash server`;
  hooks keep working). A collector it runs — and the claude sessions in
  it — stop with it; `POST /shutdown` to that collector (the TUI's
  **Restart ccdash**) stops the portal too.
- Loopback only. One-time login link → cookie (30 days, renewed while
  used). No device management, no logout.
- Nothing goes out except Web Push once you turn on 🔔 (through the
  browser's push service, encrypted). Behind a proxy, give `ccdash web`
  `HTTPS_PROXY`; 🔔 → **テスト通知** shows which link of the chain fails.
- Bounded by the hub allowlist and the **Attach** / **Approval blocking**
  toggles; **Hub connection** doesn't apply.
- A launchd example for macOS is in [Hub mode](hub.md), "Local portal".

## 16. MCP server (`ccdash mcp`)

`ccdash mcp` is a Model Context Protocol server on stdio, so Claude can
see what the dashboard sees and organize it. Register it once:

```sh
claude mcp add --scope user ccdash -- ccdash mcp        # this machine's collector
claude mcp add --scope user ccdash -- ccdash mcp -r     # the remote collector from `ccdash remote set`
```

Tools:

| Tool | What it returns |
|---|---|
| `overview` | sessions that need you, finished-but-unseen ones, running sessions grouped by project, today's usage estimate |
| `list_projects` | projects in list order with session / running / needs-you counts and latest activity; per host, `dir` = the project's working directory there (with `dir_from: "recent"` none is set and it is only where the newest session ran) and `has_brief` = the operator keeps a note for it |
| `list_sessions` | sessions newest first; filters: `project`, `query`, `running_only`, `attention_only`, `grid_only` (sessions on the grid, in tile order), `include_archived`, `limit` |
| `get_session` | one session (`#N` or id) with pending approvals and its recent prompts, replies and tool calls; `brief` = what it was asked when ccdash started it (first 2,000 characters, `brief_truncated` when cut) |
| `my_sessions` | the calling session's children (sessions it started with `start_session` or adopted), on every reachable host, grouped `needs_operator` / `working` / `reported` / `finished`; each row has ref, title, project, host, status, the declared status + note, `last_reply` (first 300 characters of the latest reply), `url`, `peer`, `reach` (how to send it a message: `SendMessage:<peer>` or `tell_session`), `age`. `reported` is a finished turn the operator hasn't looked at **or** that ended since the caller's previous `my_sessions` call (so a report isn't lost because some open ccdash screen already marked it seen). `since_last: true` = only rows changed since this session's previous call. Also offered with `--read-only` |
| `set_project` | put sessions into a project (a new name creates it) or take them out (`""`) |
| `rename_project` | rename a project; renaming onto an existing one merges them |
| `order_projects` | set the project order (listed ones first) |
| `set_project_color` | `#rrggbb` or `random` |
| `set_title` | rename a session (`""` = back to the automatic title) |
| `set_later` | mark / clear "watch later" (gathered at the top of the list) |
| `set_grid` | put sessions on / take them off the grid shared by the TUI's `V` and the portal's grid; tiles go in the order they were put there |
| `set_pinned` | pin / unpin sessions |
| `set_color` | a session's color: `#rrggbb`, `random` (apart from the running sessions' colors) or `""` (automatic) |
| `mark_seen` | mark sessions seen (clears NEW and the finished-unseen mark; approvals, questions and 回答待ち / 作業待ち declarations stay) |
| `set_archived` | archive / unarchive sessions |
| `archive_finished` | archive the sessions whose claude has exited (running ones are skipped, never stopped); keeps pinned, watch-later and unread / needs-you ones (`include_unread` takes unread too); narrow with `sessions` / `project` / `idle_hours`, preview with `dry_run`; `children_only: true` = only the calling session's children (every reachable host) |
| `start_session` | start a new claude session in a directory (absolute path; `cwd` may be left out when `project` has a working directory on that host), like the TUI's `n`. After a non-empty prompt ccdash appends the operator's notes (see "Notes for handed-over work" below) and lists them in `notes`; `plain: true` appends nothing. Optional `project`, first `prompt`, `permission_mode`, `model` (alias or model id) and `effort`; the result reports the model / effort it started with (the caller's, else the Default model / Default effort settings). Refused while the Attach setting is OFF. The caller becomes the new session's parent and the prompt is kept as its brief; the result carries `ref`, `session_id`, `url` and `peer` once claude has registered (waits up to ~10 s), else `pending: true` with the PTY key (`started`), plus `waiting_on` when it stopped at a dialog |
| `adopt_session` | make the calling session the parent of an existing session (`session`), or of every child of another session (`from`); replaces the parent it had |
| `release_session` | stop being the parent of one of your children |
| `tell_session` | send a message to one of the calling session's children, or to its own parent (`session: "parent"`) — nobody else. For a session on another machine or one whose claude has stopped (see "Messages between sessions" below). Result: `delivered` / `resumed` / `queued`. Plain words only (no leading `/ ! # -`, no control characters, at most 8000 characters). Refused while Attach is OFF |
| `stop_session` | stop one session's claude that ccdash hosts (SIGTERM, then a kill); work in progress is cut off, the transcript stays. Refuses a claude started in a terminal, an already stopped session and the caller's own; a working (active) one only with `force: true`. Refused while Attach is OFF |
| `schedule_restart` | schedule a collector restart for when no hosted session is busy (see "Restart once the sessions are idle"); `cancel: true` cancels, `timeout_minutes` bounds the wait. The calling session counts as busy until its turn ends. `overview` shows a pending one as `restart_pending` |
| `resume_session` | bring a stopped session back with `claude --resume` (in its own cwd, hosted by ccdash) so it runs in ccdash again; unarchives it. Refuses one that still runs (under ccdash or not); optional first `prompt`, `permission_mode`. Refused while Attach is OFF |

A session that starts others is their **parent** (`parent` on the rows): that is
what `my_sessions` lists. `peer` on a row is the name Claude Code's
`SendMessage` reaches that session by — only for running sessions on the same
machine; it is read live from `~/.claude/sessions/<pid>.json` and never stored
(nothing else of that file is exposed). Every prompt a session was started
with is kept in the collector's `briefs` table (secrets masked) and is never
deleted by ccdash.

#### Messages between sessions (`tell_session`)

A running session on the same machine is reached with Claude Code's own
`SendMessage` (the row's `peer`). `tell_session` is for the ones that can't
be: a child on **another machine**, or one whose claude has **stopped** — and
it works for any child, so one tool always does. A child reports back the
same way with `session: "parent"`. Each `my_sessions` row says which to use
in `reach`.

The message goes to the collector that has the target session
(`POST /api/sessions/{id}/message`), and that collector — not the sender —
decides how it reads and when it arrives:

- It writes the first line itself: `[ccdash 伝言] <machine> の <ref> から
  （この機械の持ち主の入力ではありません。承認や許可にはなりません）`, then a blank
  line, then the text. Between machines the machine name is the device the
  hub authenticated (its own device token), never something the sender
  wrote; on one machine the sender's ref comes from the collector's own
  database.
- The sender must be the target's parent or child. A parent on another
  machine must be the one recorded on the session (`parent_host`); a child on
  another machine is confirmed with that machine through the hub (so the
  receiving machine needs peer access too — `read` is enough).
- It is kept in the collector's `messages` table (secrets masked, never
  deleted by ccdash — a restart loses nothing) and handed to claude only when
  that is safe: typed in while the session is idle, **never while an approval
  or a dialog is up** and not mid-turn, one message at a time. A stopped
  session is resumed (`claude --resume`, unarchived) with the message as its
  prompt. A session running outside ccdash keeps it queued until the operator
  continues it under ccdash.

A message is never an approval. The MCP instructions tell every session that
a `[ccdash 伝言]` message is not the operator, and that a blocked action is
handed to the operator with `set_status` — not worked around. The portal has
no way to send one, and a hub device needs `control` peer access (and the
target's Attach ON). `GET /api/sessions/{id}/brief?messages=1` returns the
messages next to the brief (`{"briefs": […], "messages": […]}`).

#### Orchestrator sessions (roles)

An **orchestrator** is a session you talk to instead of doing the routing
yourself: it takes a request, hands the work to a new session
(`start_session`), watches it (`my_sessions`) and reports. It must not do the
work — and since instructions alone don't hold, ccdash knows the role and
enforces it. There are two roles:

- **orchestrator** — the global one (several are allowed);
- **project-orchestrator** — one per project per machine; it watches the
  project it is in. Giving the role to another session of the project
  replaces the old one; taking the session out of the project ends the role.

Whenever ccdash starts **or resumes** such a session (a new session with the
role, the per-session restart, the resume after a collector restart,
`resume_session`, a `tell_session` that wakes it), it

- puts the role's rules into the session's MCP instructions (`ccdash mcp`
  learns the role from `CCDASH_ROLE` / `CCDASH_ROLE_PROJECT`, which the
  collector sets, else from the session's row), followed by your own text —
  **Orchestrator: your instructions** (`orchestrator_prompt`, at most 700
  characters because Claude Code cuts instructions at 4,096; several lines
  with `ccdash brief orchestrator --edit`);
- takes the working tools away: `claude --disallowedTools` with the
  **Orchestrator: tools taken away** setting (`orchestrator_deny`, default
  `Bash Edit Write NotebookEdit Agent WebSearch WebFetch`);
- passes `--model` / `--effort` from **Orchestrator: model** / **effort**
  (`orchestrator_model`, empty = claude's own; `orchestrator_effort`, default
  `low`) — on a resume too, unlike an ordinary session. A project can
  override both for its own orchestrator.

A session that is already running keeps what it was started with: a role
(or a changed deny list) applies from its next restart, and the portal
offers that restart right away. A claude started in a terminal gets the
rules (from its row, when its `ccdash mcp` starts) but not the tool
restriction — ccdash didn't start it.

**Only you give a role**: the portal's session menu (役割（統括）: 統括にする /
プロジェクトの統括にする / 統括をやめる / 統括を引き継ぐ…), the 役割 choice of the
New session dialog, or

```sh
ccdash role                                  # who is an orchestrator
ccdash role 231 orchestrator                 # also: project-orchestrator, none
ccdash role 250 --from 231                   # 250 takes over 231's role and its children
ccdash role --project web --model haiku --effort low   # what web's orchestrator runs with
ccdash brief orchestrator --edit             # your own rules (--set-file f, --clear)
```

No MCP tool sets a role, `start_session` can't start an orchestrator, and
another machine can't either (the hub refuses a device token on the role
routes and a `role` in its start). Routes: `POST /api/sessions/{id}/role
{"role", "from"}` → `{role, project, replaced, moved, restart_needed}` and
`POST /api/projects/orchestrator {"project","model","effort"}`; both need
the host's Attach ON through the hub.

What the tools do for orchestrators:

- `list_projects` rows carry `orchestrator` (ref, url, status, `reach`).
- `start_session` called by the **global** orchestrator for a project that
  has a running orchestrator starts nothing and returns `routed: true` with
  `project_orchestrator` — hand the request to it; `direct: true` starts the
  session anyway.
- `tell_session` also carries messages between the global orchestrator and
  a project's orchestrator (the latter names the former `"orchestrator"`),
  across machines too: the receiving collector checks the roles itself — its
  own rows, and the sending device's answer through the hub.
- `my_sessions` adds `project_sessions` for a project's orchestrator (the
  project's sessions it didn't start, grouped the same way) and
  `orchestrators` for the global one (per project orchestrator: its children
  counted per group, and the ones waiting on you).

#### The orchestrator tree, child sessions and "waiting on you" (portal)

The portal's session list keeps orchestrators in a fixed place and shows
children under their parent.

- **Global orchestrators** sit in a 統括 band at the very top — above pins,
  watch later, 新着 and the date bands — oldest first; activity never moves
  them. Their children that are in no project are indented under them; the
  arrow on the row folds them away (remembered per browser).
- **A project's orchestrator** is the first row of its project block, above
  the block's pins. Inside a block a session is indented under its parent
  when that parent is in the same block — two levels at most.
- A row whose parent is not right above it carries the parent's ref as a
  mark (e.g. A1). A child that is in a project stays in its project block,
  not under the global orchestrator.
- A row with children shows `子 12 判断待ち 4` (children, and how many wait
  on you). Press it — or 子セッションだけ表示 in the session menu — to narrow
  the list to that session and everything under it; すべてのセッションを表示
  above the list goes back. A project band shows `子 N` too.
- A row lifted into 新着 or put into あとで見る leaves the tree for that
  block (orchestrators never do). In 更新順 only the global orchestrators
  stay on top.
- With no roles and no parents the list is what it was before.

**The 子セッション pane.** Opening the chat of a session with a role (or
with children) shows 子セッション on the right — in the tasks pane's place,
covering it while open; on a phone a button in the chat bar opens it as a
full sheet. Sections, top to bottom: 判断待ち / 作業中 / 報告あり / 終わり,
filed the way `my_sessions` files them. 判断待ち holds every session under
this one that waits on you. A project's orchestrator also gets
このプロジェクトのほかのセッション (folded), a global one プロジェクトの統括
(each project orchestrator with its counts). Children on other machines are
included.

**判断待ち (`#/inbox`).** Every session that waits on you — an approval, a
question, a declared answer or action — from every machine, in one place
(the bottom tab on a phone, the header's 要対応 N chip on a desktop),
grouped by orchestrator and project, and answered on the card:

| Waiting for | The card shows |
|---|---|
| an approval | the tool, what it is for, the command; 許可 / 常に許可 / 拒否 |
| a question (AskUserQuestion), a choice on screen | the choices (the chat's own card) |
| a declared `action` whose reply has a `! command` | the end of the reply and コマンドを実行する (after a confirm it runs inside that session) |
| a declared `answer` / `action` | the note, the end of the reply, an input box |
| a dialog before the session exists (folder trust) | the choices on screen |

What you answer goes from the browser straight to that session; it never
passes through an orchestrator (an orchestrator can't carry an approval).
A card counts as read only once you press or type in it. At most six cards
are open at a time; the rest are rows, opened with さらに表示.

**依頼.** 依頼 in the session menu shows the brief ccdash kept when it
started the session — the caller's own text, and the notes ccdash appended,
apart — and the messages it received through `tell_session`, with who sent
them. It is absent for a session with neither, and on an older collector.

#### Notes for handed-over work (`ccdash brief`)

A session that hands work to others with `start_session` would otherwise
retype the same cautions in every prompt. You write them once and ccdash
appends them to each of those prompts, on the machine that runs the new
session:

- the **common note** of the machine, under `## 共通の注意`;
- the **project's note**, under `## このプロジェクトの注意`, when the session
  is started in a project.

A project can also have a **working directory** per machine: `start_session`
with just `project` then starts there (an explicit `cwd` still wins).

Edit them in the portal — a project band's **…** opens "プロジェクトの設定"
(working directory, note; with several hosts the dialog says which host it
edits and can switch), and "共通の注意…" there, or the same entry in a
device page's settings box, edits the common note — or from a shell:

```sh
ccdash brief                               # what is set
ccdash brief common --edit                 # $EDITOR; --set-file f (- = stdin), --clear
ccdash brief project web --dir ~/src/web   # the project's working directory ("" clears)
ccdash brief project web --edit            # the project's note
```

Only people can write them: no MCP tool does, a hub device token (peer
access) can't, and the portal needs the host's Attach setting ON. Nothing is
appended to an empty prompt, with `plain: true`, or to sessions started with
`n`, the portal's New session or `handoff-mcp`. The brief kept for the
session (`get_session`: `brief`) is the full text it received.

**Session setup command** (settings page, `session_setup_command`; empty =
off) is a shell command the collector runs — through your login shell, at
most 60 s — before every `start_session` spawns claude, e.g. to create the
project directory and commit the brief as a task file. It gets:

| Variable | |
|---|---|
| `CCDASH_BRIEF_FILE` | a temporary file (0600, removed afterwards) holding the full brief |
| `CCDASH_PROJECT` | the project, or empty |
| `CCDASH_TITLE` | the first line of the prompt (80 characters at most) |
| `CCDASH_CWD` | the directory that would be used (may be empty) |
| `CCDASH_PARENT` | the starting session's ref (or id) |

The last non-empty line it prints, if an existing absolute directory, becomes
the session's directory; printing nothing keeps `CCDASH_CWD`. A non-zero exit
or the timeout stops the start, and the caller gets the end of its stderr.
It runs on the machine that hosts the new session and can only be set on
that machine itself (the TUI settings page): not in remote mode (`-r`), not
from the hub, not by MCP.

```sh
#!/bin/sh
# ~/bin/ccdash-setup: keep each brief as a task file in the project
d="${CCDASH_CWD:?no directory}"
mkdir -p "$d/tasks" && f="$d/tasks/$(date +%Y-%m-%d-%H%M).md"
cp "$CCDASH_BRIEF_FILE" "$f" && git -C "$d" add "$f" >&2 && git -C "$d" commit -qm "task: $CCDASH_TITLE" >&2
echo "$d"
```

With machines joined to a hub, `list_hosts` names the reachable machines
and the other tools take `host` (see section 18). The to-do list has its own
tools (`list_tasks`, `add_task` …, see section 17).

It never answers approvals. Add
`--read-only` (`ccdash mcp --read-only`) to offer only the first four plus
`set_status`.

### Status declarations (`set_status`)

With many sessions running, a stopped one doesn't tell you whether it waits
for you or just reported. ccdash's MCP server gives every session that has it
a rule (in the MCP instructions — no CLAUDE.md edits): right before ending a
turn, call `set_status` once.

| status | shown as | meaning | effect |
|---|---|---|---|
| `answer` | 回答待ち | asked you a question / for a decision, can't go on without it | needs you (push "回答待ち") |
| `action` | 作業待ち | you must do something first (log in, merge, check a device…) | needs you (push "作業待ち") |
| `report` | 報告 | done; the last message is a report | unread done |
| `background` | 自動で続行 | waiting on CI / a build / a task, continues by itself | unread done |

The one-line `note` shows on the TUI's second line and in the portal's and
app's list, board and chat header. 回答待ち / 作業待ち stay "needs you" until
the next prompt (looking doesn't clear them). The next prompt clears the
declaration, and a turn that ends without one doesn't inherit the previous.
The calling session is found from the MCP server's parent process
(`~/.claude/sessions/<pid>.json`). Restart running claude sessions after
`ccdash setup` to pick the rule up.

When this machine is joined to a hub (`ccdash hub join`), every session
carries a `url` (`https://<hub>/s/<session id>`). Opening it in a browser or
the installed app lets the hub find the device that has the session and open
its chat (after a login if needed), so a managing Claude can hand you a list
of "sessions that need you" you can tap straight into. `--hub-url` sets or
overrides the hub origin. Tapped inside the portal's chat, such a link moves
to that chat in place (in the installed app too) instead of opening a tab.

## 17. Tasks (to-do list)

ccdash keeps a to-do list next to the sessions, per machine, in the
collector's database. The idea is that **Claude keeps it**: you say "add a
task for the release notes" or "#11: the review is done" in any session,
and Claude updates the list through the MCP tools (recording progress on the
task's timeline and linking the session to it). You mostly reorder, tick off
and plan.

- **Status**: todo / doing / waiting / done / cancelled. There is no delete;
  a done or cancelled task can be archived (and brought back).
- **Priority** (1 now … 4 someday) and **plan** (the day you'll work on it)
  are separate. The list is grouped by plan: today, tomorrow, this week,
  next week, later, unplanned.
- **Ids never change**: "#11" always means the same task. With several
  machines on a hub, a task's ref carries the host letter ("B#11").
- A task can belong to a ccdash project and carry links (URLs, a working
  directory, PRs, Jira keys, Slack threads).

**TUI**: the task pane is the right-hand column (`w` shows / hides it, `W`
focuses it, `b` folds the session list to make room). In the pane: `space`
done / back to todo, `s` cycles todo → doing → waiting, `c` cancels,
`1`–`4` plan today / tomorrow / this week / next week (`0` none), `[` / `]`
reorder, `enter` details and timeline, `y` copies "#N について、" to start a
prompt about it, `n` starts a session for the task, `x` archives. The
widths come from the **Pane widths** setting.

**Portal**: the task pane is docked on the right of every page and lists
every online machine's tasks (chips narrow to one host); the home page's
やること shows today and tomorrow.

**CLI** (handy in scripts):

```sh
ccdash tasks list                      # open tasks by plan (--all: finished and archived too)
ccdash tasks add "Write release notes" --plan tomorrow --priority 2 --project docs
ccdash tasks note 11 "Draft sent for review"
ccdash tasks status 11 done
ccdash tasks plan 12 week --why "waiting for the API"
ccdash tasks show 11                   # links and the whole timeline
ccdash tasks md                        # the list as Markdown
```

**MCP tools**: `list_tasks`, `get_task`, `add_task`, `update_task`,
`set_task_status`, `plan_task`, `add_task_note`, `link_task`, `order_tasks`,
`archive_task`. For an automated run (e.g. a scheduled `claude -p` that
reads Slack / Jira / GitHub through their own MCP servers), `task_context`
hands it the open tasks and their links and `sync_tasks` (or
`ccdash tasks sync` on stdin) merges its proposal — your own edits always
win: titles, priorities and notes are never overwritten, a plan you set
today is never moved, and a finished task is never reopened. ccdash itself
never writes to Slack, Jira or GitHub.

## 18. Several machines from one Claude (multi-host)

With machines joined to a hub, `ccdash mcp` on one of them can see and
drive the others' sessions — e.g. ask the Claude on your laptop "what is
waiting for me on the server?".

1. In the portal: 端末 (devices) → the machine that should reach the others
   → 管理 → 他の端末へのアクセス: オフ (off, default) / 閲覧 (read) /
   操作 (control).
2. On that machine, `ccdash mcp` (as registered by `ccdash setup`) now
   offers `list_hosts`, and the read tools cover every reachable machine;
   their rows carry `host`. Acting tools (`start_session`, `set_project` …)
   act on this machine unless you pass `host`.

Sessions are referred to by **ref**: the host's letter plus the number
("A231"); a bare "#231" is this machine's. Letters are assigned by the hub
in registration order and can be changed in the device menu. Peers never get
approvals, typing into a session, device management or settings — and each
target machine's own switches (Attach, Approval blocking) still apply. The one
thing a peer can say to a session is `tell_session`'s message from its parent
or child, which the receiving machine labels and delivers itself (section 16,
"Messages between sessions").

## 19. Letting another assistant or a bot hand work to Claude (`ccdash handoff-mcp`)

`ccdash handoff-mcp` is a small MCP server over HTTP for a caller outside
Claude Code: an assistant behind an MCP gateway (it was built for Grok), or
a bot of your own; any client that speaks Streamable HTTP MCP works. It can
**start** Claude sessions in one fixed directory and follow up on them, but
it can't touch your other sessions:

| Tool | What it does |
|---|---|
| `start_session` | a new claude in `--cwd` (auto permission mode), marked with `--origin`, in the `--project` project (default: none) |
| `list_sessions` | every session, but only ref / title / status / the one-line status note |
| `read_session` | the conversation (text only) of a session this server started |
| `send_message` | more text for a session this server started (resumes it when stopped) |

"Started by this server" means carrying its `--origin` (default `handoff`).
Run one instance per caller with its own origin, token and directory; they
never see each other's conversations. How far Claude goes (look only, a pull
request, a merge) is up to the directory's own `CLAUDE.md` and
`.claude/settings.json`. It never answers approvals or dialogs. Every request
needs `Authorization: Bearer <token>`.

```sh
openssl rand -hex 32 > ~/.config/ccdash/alerts.token && chmod 600 ~/.config/ccdash/alerts.token
ccdash handoff-mcp --cwd ~/src/inbox --origin alerts --project Alerts \
  --token-file ~/.config/ccdash/alerts.token \
  --listen 127.0.0.1:9131 --allow 100.64.0.0/10    # client addresses (default: any)
```

The MCP endpoint is `http://<listen>/mcp`. Keep `--listen` on loopback or
a private network and narrow `--allow`; a collector must be running on the
machine.

`ccdash grok-mcp` is the old name, kept as a hidden alias: the same server
with `--origin grok --project Grok` and advice to Grok on fixing
speech-recognition mistakes in the request.

## 20. Asking from the page you are looking at (quick request, `#/intake`)

When something on a page (your own app, say) should change, start a session from there with the page's URL, title, selected text and a screenshot — without going back to the portal first.

- **Desktop browser**: open `#/intake` in the portal → "Entry points and setup", and drag the "Ask ccdash" link to the bookmarks bar. Clicking it on any page opens the quick request form in a new tab. Add a screenshot with "Capture screen" (one shot of a tab or window) or by pasting / dropping an OS screenshot
- **Phone**: install the portal as an app and ccdash appears in the share sheet; sharing a screenshot or a URL opens the form (Chrome on Android; iOS has no share targets for web apps)
- **Routes**: per URL prefix (`match`), the host (device id or name), directory, project and permission mode to start in; the longest match wins, and with none the last host's default directory is used. Tick "Remember this as the target for …" when sending to add one
- **Footer**: text appended to every request (your standing rules)

The bookmarklet reads only the URL, title and selection. Images go to the target machine's upload dir (`$XDG_STATE_HOME/ccdash/uploads`, removed after 7 days) and the request carries their paths. Routes and footer are stored in the hub per login; keep secrets out of them.

### Requests from your own apps (`--intake-apps-file`)

Put the "ask" button (`contrib/intake-widget/ccdash-intake.js`) in your own web apps and a page's URL, screenshot, app state and console errors reach the hub: the app's server relays to `POST /api/intake/requests` with the app's own token (the browser never sees it). Where it runs (machine, directory, project, permission mode) and whether it starts at once or waits as a draft are set in the hub's `--intake-apps-file` next to the token's hash; the caller can't change them. Drafts are listed on the quick request page and announced by push. See `contrib/intake-widget/README.md`.
