# ccdash

[![License: MIT](https://img.shields.io/badge/License-MIT-ff922b.svg)](./LICENSE)
[![Built with Claude Code](https://img.shields.io/badge/Built%20with-Claude%20Code-d18ee2)](https://claude.com/claude-code)

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
| **Per-session controls** | Rename, custom user-named group assignment, archive / unarchive, attach via `tmux switch-pane` or `claude --resume`. |
| **Approvals** | When enabled, pending permission requests appear in a yellow banner; press `a` / `A` (keep) / `d` to allow / keep-allow-for-session / deny without leaving the dashboard. |
| **Projects** | `p` puts sessions in an operator-named project: members move to the newest end of the list, one block per project under a full-width band in the project's color, each row with a color gutter. |
| **Generated titles** | Sessions are titled automatically by `claude -p` after their first exchange or two (`auto_title`); `ctrl+t` (re)generates on demand. Titles come from a redacted digest of the transcript. |
| **Auto-archive** | Once a day, sessions idle longer than N days (default 7) are archived — favorites, project members and running sessions are kept; resuming one brings it back. |
| **Tabs** | Browser-style strip across the top filters by repo or operator-named group. Slides on overflow. |
| **Search** | `/` filters the list by case-insensitive substring across title, tab, repo, project, session id. |
| **Settings page** | `,` opens a persisted preferences modal: layout (auto / vertical / horizontal), refresh rate, title-generation timeout, auto-archive days, secure-mode toggles, and an "observation only" preset. |
| **Self-update** | `ccdash update` pulls the latest GitHub release in place. |

Hands-on walkthroughs:
[English usage guide](./docs/usage_en.md) · [日本語 使い方](./docs/usage_jp.md)

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
```

Drops a binary in `$HOME/.local/bin` (override with `CCDASH_INSTALL_DIR=...`).
If you have it already, just run `ccdash update` — it talks to the GitHub
API and swaps the binary atomically.

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

`install-hooks` also puts a small relay in `statusLine`: it forwards the
status JSON Claude Code hands its status line (model, context window, cost,
lines changed, rate limits) to the collector — the portal draws its status
line from it — and then runs your own `statusLine` command unchanged, so
the terminal looks the same. `ccdash install-statusline` installs only that
relay; `ccdash uninstall-statusline` puts your original setting back.

To unwire ccdash from Claude (this restores your `statusLine` too):

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

#### Sessions view (default)

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | move selection |
| `g` / `G` | jump to top / bottom |
| `tab` / `shift+tab` | cycle project / user-named groups |
| `R` | toggle "auto repo tabs" in the cycle |
| `T` | edit the user-named group for this session |
| `t` | rename the session (operator override of auto / generated title) |
| `f` | toggle ★ favorite (favorites pin to the top) |
| `c` / `C` | next color / re-roll for the session's bar (shared with the portal). Running sessions get a stored color that stands apart from the other running ones; `C` re-rolls by the same rule |
| `ctrl+r` | restart the session's ccdash-hosted claude (kill + `claude --resume`), after `y` |
| `!` | show only sessions that need you (`?`: approval, question, menu on screen) or finished a turn you haven't looked at (`✓`) |
| `p` | put the session in a project: existing projects as colored candidates (`↑` `↓` to pick), a new name creates one, `✕ remove from …` takes it out |
| `P` | re-roll the project's color (kept apart from other projects' colors) |
| `x` | archive / unarchive |
| `X` | toggle archive view (operator-archived sessions) |
| `o` | full-screen transcript viewer |
| `ctrl+t` | generate short titles with `claude -p` — `y` for the selected session, `a` for the recent batch in one call |
| `n` | start a new claude session (directory picker) |
| `S` | run a skill / slash command in a new session |
| `<` / `>` | shrink / grow the session list by 5% (10–90%, saved as "Session list size") |
| `enter` | attach to the session (tmux switch / `claude --resume`) |
| `a` / `A` / `d` | allow / keep-allow / deny the oldest pending approval |
| `Shift+J` `Shift+K` | scroll the right pane one line at a time |
| `pgup` / `pgdn` | half-page scroll on the right pane |
| `/` | search across session metadata |
| `,` | open the settings page |
| `r` | force refresh |
| `?` | help window listing every key (`j` / `k` scroll) |
| `q` / `ctrl+c` | quit |

#### Transcript modal (`o`)

| Key | Action |
| --- | --- |
| `↑` `↓` | scroll one line |
| `pgup` `pgdn` / `ctrl+u` `ctrl+d` / space | half-page |
| `g` / `G` | top / bottom |
| `r` | reload from disk |
| `esc` / `q` / `tab` | back to sessions |

#### Settings page (`,`)

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

- **Approval blocking** — when off, ccdash never holds PermissionRequest
  hooks — Claude shows its own prompt as before, and the `a` / `A` / `d`
  shortcuts are disabled.
- **Title generation via `claude -p`** — when off, `ctrl+t` is disabled
  and no digest leaves the host.
- **Attach (enter)** — when off, `enter` only shows session info, never
  spawns `claude --resume` or runs `tmux switch-client`.
- **Auto-rewrite settings.json** — when off, server boot does *not*
  silently update `~/.claude/settings.json` even after a token rotation.
- **Hub connection** — when off, the collector does not dial the hub
  joined via `ccdash hub join` (and drops a live tunnel within ~10 s), so
  the machine is unreachable from the web portal.

The **"Apply secure preset"** action flips all five to off in one shot
for a fully observation-only deployment.

## Remote mode

By default the collector only binds `127.0.0.1` and the TUI reads its
SQLite DB / transcripts directly off the same disk — the two have to run
on the same host. Remote mode lifts that: run the collector on a dev
server, and point a TUI on any other machine at it over HTTP.

**1. Start a collector that listens beyond loopback**, on the host that
runs `claude` (call it `dev-box`):

```sh
ccdash server --listen 0.0.0.0:9123
# or a specific address if the host has more than one interface:
ccdash server --listen 192.168.20.132:9123
```

`--listen` is opt-in (without it the collector stays `127.0.0.1:9123`; the
older `--addr` flag still works but is deprecated in favor of `--listen`).
Binding non-loopback logs a clear warning, and ccdash refuses to start if
no auth token exists yet — run `ccdash server` (or `ccdash install-hooks`)
locally once first so `$XDG_STATE_HOME/ccdash/token` gets created, *then*
add `--listen`.

When `--listen` names a specific non-loopback IP, the collector **also**
keeps a listener on `127.0.0.1:<port>`: Claude Code's installed hooks are
wired to `127.0.0.1` and always arrive via loopback, and a local TUI on the
same host keeps working too — remote clients are the only ones using the
`--listen` address. (A wildcard bind like `0.0.0.0:9123` already accepts
loopback connections, so it stays a single listener.)

For a persistent setup, run it as a systemd user unit:

```ini
# ~/.config/systemd/user/ccdash.service
[Unit]
Description=ccdash collector

[Service]
ExecStart=%h/.local/bin/ccdash server --listen 0.0.0.0:9123
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now ccdash.service
```

**2. Copy the collector's token** to the machine that will run the TUI —
it's the same shared secret hook events already use, just scp'd over:

```sh
scp dev-box:~/.local/state/ccdash/token ~/.config/ccdash/collector.token
```

**3. Configure the remote once**, then use `-r` from anywhere:

```sh
ccdash remote set --url http://192.168.20.132:9123 \
  --token-file ~/.config/ccdash/collector.token \
  --ssh-target dev-box        # optional; defaults to the URL hostname
ccdash remote show            # prints the effective config + sources
ccdash -r                     # open the TUI against the configured remote
```

`ccdash remote set` writes `~/.config/ccdash/config.json`
(`$XDG_CONFIG_HOME/ccdash/config.json`, mode 0600):

```json
{
  "remote": {
    "url": "http://192.168.20.132:9123",
    "token_file": "/home/op/.config/ccdash/collector.token",
    "ssh_target": "dev-box"
  }
}
```

Per-invocation overrides: `--remote-url <url>` (implies `-r`, overrides the
configured url), `--token-file`, `--ssh-target`. Precedence for every
field: explicit flag → config file → (token only) `$CCDASH_TOKEN` env var →
a helpful error. `ccdash -r` without any configuration tells you to run
`ccdash remote set` first. In remote mode ccdash never opens a local DB and
never spawns/pings a local collector — every session list, approval
decision, and transcript read goes over HTTP to the remote origin,
authenticated the same way hook events are (the `X-Ccdash-Token` header).

**Attach and new-session in remote mode** go through `ssh -t` instead of a
local PTY (`internal/attach` only manages a claude process on the machine
running ccdash, which in remote mode isn't `dev-box`): pressing `enter` on
a running session with a tmux pane runs `tmux attach -t <pane>` over ssh;
on a stopped session it runs `cd <cwd> && exec claude --resume <id>`. The
remote command is executed under `bash -lc` — a **login** shell — so PATH
additions from `~/.profile` / `~/.bash_profile` (e.g. `~/.local/bin`, where
claude usually lives) apply even though ssh's remote-command path skips rc
files. `n` (new session) works the same way with an operator-typed
directory — there's no local filesystem to check or tab-complete against,
so the path is passed through as-is and a typo just fails inside the ssh
session. The ssh target defaults to the remote URL's hostname; set
`ssh_target` in the config (or pass `--ssh-target user@host`) when it
differs (e.g. a different SSH alias or username).

`sessions` and `approvals` (the plain-text CLI subcommands) also accept
`-r` / `--remote-url` / `--token-file` / `--ssh-target`; `events` stays
local-only and says so if you pass `-r`.

**Known limitation (v1):** toggling a value on the settings page (`,`) in
remote mode performs a synchronous HTTP write; if the collector just went
unreachable, the UI can stall for up to the 3-second mutation timeout on
that keypress. Reads (session list, transcript tail) are asynchronous and
don't block the UI.

## Hub mode (web portal)

Hub mode puts every machine's ccdash behind one web page: a portal lists
your devices, and picking one shows its sessions as chats you can read and
reply to from a browser or phone, start new sessions in, resume stopped
ones from, and answer approvals in.

```
 browser ──https──▶ ccdash hub (home server, OIDC login)
                        ▲          ▲
            WebSocket tunnel (dialed OUT by each device, device token)
                        │          │
                   device A     device B      ← no inbound ports
                   ccdash server / ccdash -k
```

- **Devices are never dialed.** Each device's collector connects out to the
  hub and keeps a WebSocket open (yamux-multiplexed); the hub forwards
  portal requests through it to the device's own collector API.
- **Sessions open as a chat.** The transcript is rendered as messages and
  tool calls; what you type is delivered to the claude that ccdash hosts in
  a PTY on the device (multi-line text as a bracketed paste). A stopped
  session is resumed with your message as its first prompt. Approvals show
  as cards (with ccdash's hooks installed), and TUI-only prompts — a
  permission dialog without hooks, a menu, the folder-trust question — show
  as a screen excerpt with one-tap answers. Claude's own multiple-choice
  questions (AskUserQuestion) become a form — single / multi select and
  free text — driven by the same keys you'd press in the terminal. Images
  (picked, pasted, or from a phone camera) are uploaded to the device and
  pasted into the prompt as attachments. Older history loads on demand. A
  **Terminal** button switches to a full xterm.js view of the same PTY.
- The session list mirrors the TUI: a newest-first view with date
  sections, plus one tab per group; rename / group / project / favorite /
  archive / title generation from a session's menu, with project bands and
  project-tinted rows; skills and a first
  message for new sessions; the device's settings read-only.
- **Needs-you board**: the portal's front page lists, across every device,
  sessions that need you (an approval, a question, a menu on screen),
  are working, or finished a turn you haven't looked at; the installed app's
  icon shows the count. The TUI shows the same state (`?` / `✓`, header
  counts, `!` to filter).
- **Diff review**: open a session's working-tree diff, tap lines to leave
  notes, and send them all to Claude as one prompt.
- **Usage**: tokens and an API-price estimate per session (subagents
  included) and per day — the TUI header shows today's total.
- **Composer**: quick commands (one-tap prompts shared across browsers),
  voice input, `/` completion of skills and slash commands, and a
  permission mode for new sessions.
- **Push notifications** (Web Push, VAPID keys kept in hub.sqlite): the hub
  polls each connected device through its tunnel and notifies subscribed
  browsers of new approvals, questions / confirmations on a hosted claude's
  screen, and finished turns. The portal is installable as a PWA.

**On the server** (the image is `ghcr.io/takumanakagame/ccdash`, entrypoint
`ccdash hub serve`; TLS terminates in front of it):

```sh
ccdash hub serve --listen :8080 --data-dir /data \
  --public-url https://ccdash.example.net \
  --oidc-issuer https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client-id> \
  --oidc-client-id <client-id> --oidc-client-secret-file /run/secrets/oidc \
  --allowed-email you@example.com
```

Every flag also reads an env var (`CCDASH_HUB_PUBLIC_URL`,
`CCDASH_HUB_OIDC_ISSUER`, `CCDASH_HUB_OIDC_CLIENT_ID`,
`CCDASH_HUB_OIDC_CLIENT_SECRET`, `CCDASH_HUB_ALLOWED_EMAILS`, …; see
`ccdash hub serve --help`). Without an OIDC provider, `--tailscale-auth`
(loopback `--listen` only) trusts the `Tailscale-User-Login` header that
`tailscale serve` adds, against the same `--allowed-email` list — handy for
running the hub on a workstation reached over your tailnet. Any OIDC provider works; the redirect URI to
register is `<public-url>/auth/callback`. The hub refuses to start without
at least one allowed email. Proxies in front of it must pass WebSocket
upgrades and allow long-lived connections (the tunnel pings every 20 s).

**On each device:** open the portal, **Add device**, then

```sh
ccdash hub join --url https://ccdash.example.net   # paste the token when asked
ccdash server                                      # or ccdash -k; keep a collector running
```

`ccdash hub status` shows the join state, `ccdash hub leave` removes it.
The collector re-reads the join file and the **Hub connection** setting on
every attempt, so no restart is needed.

**What the hub may do on a device** is fixed by an allowlist in the
device's collector (`internal/server/hub.go`): read sessions, approvals and
transcripts; rename/group/archive sessions and set projects / project
colors; start, type into, resize and stop ccdash-hosted PTYs (only while
**Attach** is on); decide approvals (only while **Approval blocking** is
on); list directories for the new-session picker (attach on). It can never
reach the hook endpoints, `/shutdown`, or settings writes — so a compromised
hub cannot switch the device's own safety toggles back on.

**Read tokens (machine clients).** A bot or script that can't do a browser
login can read the hub with `Authorization: Bearer <token>`. Configure the
tokens at start (each ≥ 32 characters, e.g. `openssl rand -hex 32`):
`CCDASH_HUB_READ_TOKENS` (comma-separated) and/or `--read-token-file`
(`CCDASH_HUB_READ_TOKEN_FILE`; one per line). A read token may only GET:

| Route | Returns |
|---|---|
| `/api/board` | `{"needs_you": [card], "working": [card], "done": [card]}` |
| `/api/active` | `[card]` — running (active / idle) or needs-you sessions |
| `/api/devices` | `[{id, name, created_at, last_seen, hostname, version, online, since, remote}]` |
| `/api/d/{id}/api/sessions` | the device's `[session]` (`?archived=1` for archived ones) |
| `/api/d/{id}/api/sessions/{sid}/transcript` | `{mtime, size, data}` — `data` is base64 of the transcript JSONL tail; `mode=tail` (default, `bytes=` ≤ 1 MiB, `lines=N` for the last N records) or `mode=stat` |

A card is `{device_id, device_name, session}`; a session has `session_id`,
`num`, `title` / `gen_title` / `custom_title`, `cwd`, `repo`, `branch`,
`status` (`active` / `idle` / `recent` / `stopped`), `attention`
(`needs_you` / `done` / empty), `attention_reason`, `attention_at`,
`last_seen`, `model`, … (`internal/model.Session`). Everything else —
writes, PTYs, approvals, push, settings — answers 403 to a read token; a
wrong token gets 401. The bearer header is never forwarded to devices. See
`docs/decisions/0004-hub-read-tokens.md`.

## Threat model

ccdash is built for a single user (or a small trusted team, via remote
mode) managing their own Claude Code sessions.

**Inside the trust boundary** (assumed honest):
- The local user account running ccdash and `claude`
- The `claude` binary and its on-disk state under `~/.claude/`
- Files the operator opens or edits via Claude
- In remote mode: whoever holds the collector's token, and the network
  path between the TUI and the collector (see "Explicitly out of scope"
  below for what that does *not* cover)

**Outside the trust boundary** (treated as adversarial):
- Other UNIX users on the same host
- Repositories the operator opens whose `.claude/settings.json` could try
  to inject hooks (Claude Code's concern; ccdash never writes into a
  project-scoped settings file, only `~/.claude/settings.json`)
- Network access of any kind by default — the embedded/managed collector
  binds `127.0.0.1` and will not accept connections from other interfaces.
  **Non-loopback binding requires an explicit opt-in** (`ccdash server
  --listen <addr>`) and a token always still gates every request; see
  "Remote mode" above.

#### Explicitly out of scope

- Multi-user / shared-host deployments where operators don't trust each
  other (remote mode is for one operator's own collector, reachable from
  their own other machines — not a shared multi-tenant service)
- Public internet exposure of the collector, remote mode or not
- **No TLS in v1.** Remote mode's HTTP traffic (including the token
  header, prompts, and approval decisions) is unencrypted on the wire —
  intended for a trusted LAN or a VPN/Tailscale overlay, not an open or
  untrusted network
- Web UI / browser access (there is none — TUI only)
- Defense against the operator pasting their own secrets into prompts.
  The best ccdash can do is mask common token patterns before persisting.
  Claude itself already keeps a copy of every prompt in
  `~/.claude/projects/*.jsonl` regardless of ccdash.

#### Mitigations in this codebase

- Loopback by default. The embedded/managed collector (plain `ccdash`,
  `-k`) is loopback-only, unconditionally — no flag changes that. A
  standalone `ccdash server` can opt into a non-loopback bind via
  `--listen`, which logs a clear warning and refuses to start without an
  existing auth token (see "Remote mode")
- DB file at `$XDG_STATE_HOME/ccdash/ccdash.sqlite` with `0600` permissions
- Hook entries in `~/.claude/settings.json` carry an `X-Ccdash-Managed`
  marker (or, for the forwarder entries, point at `ccdash/hook.sh`) so
  `install-hooks` and `uninstall-hooks` round-trip them idempotently
  without disturbing other user hooks
- `statusLine` points at `$XDG_STATE_HOME/ccdash/statusline.sh` (your
  original setting is saved in `statusline.orig.json` and run by the relay);
  the relay backgrounds its POST the same way
- Only PermissionRequest is a blocking HTTP hook. Every other event runs
  `$XDG_STATE_HOME/ccdash/hook.sh`, which backgrounds a `curl` POST and
  returns at once, so a stopped or hung ccdash never slows a session down
- Random shared token at `$XDG_STATE_HOME/ccdash/token` (mode `0600`),
  compared with a constant-time check. Required on every hook, decision,
  and remote-mode API request — whether loopback or not — so other UNIX
  users on the host, or other hosts on the network in remote mode, can't
  forge events, approve tools, or read session data without it. The
  server auto-rewrites the hook headers when it rotates.
- Token-bucket rate limit on every authenticated route (50 QPS / 100
  burst) bounds the impact of runaway loops or scripted floods
- Pattern-based masking on hook payloads / titles before
  they reach the DB (Bearer tokens, `KEY=VALUE` env, AWS / GitHub /
  OpenAI / Anthropic key formats, URL credentials, etc.). The masking
  matters mainly for title generation (which does send a digest
  over the network); on-disk Claude transcripts are unaffected.
- The remote-mode transcript API resolves the file path from the
  session's DB row only — never from anything the client sends — so a
  remote TUI can't ask the collector to read an arbitrary path.
- Hub mode: the hub is effectively a remote operator for every joined
  device, so it is gated by OIDC login + an email allowlist, signed
  HttpOnly/SameSite cookies, Go's cross-origin protection on state-changing
  API calls, and a same-origin check on the terminal WebSocket. Device
  tokens are stored only as SHA-256 hashes on the hub and 0600 on the
  device; rotating or deleting a device in the portal cuts its tunnel
  immediately. The device-side allowlist (see "Hub mode") bounds what a
  compromised hub could do, and the **Hub connection** toggle (part of the
  secure preset) takes a device off the hub entirely. Optional read tokens
  (bearer, GET-only on five read routes) can read session lists and
  transcript tails — the raw transcript, unredacted — but can't write,
  drive a PTY or decide approvals; treat one like a portal login.

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
internal/summarize/             claude -p driver for generated titles (ctrl+t)
internal/redact/                pattern-based secret masking
internal/auth/                  loopback shared-token loader
internal/settings/              persisted preferences (settings table + spec)
internal/selfupdate/            `ccdash update` self-replace logic
internal/hookcfg/               install-hooks settings.json merge
internal/wrapper/               `ccdash claude` exec wrapper
internal/paths/                 state dir / db / settings paths
internal/gitinfo/               git repo / branch / commit lookup
internal/store/                 Store seam (Local: *db.DB + files; Remote: HTTP client for -r remote mode)
internal/clientcfg/             client-side remote config (~/.config/ccdash/config.json) for -r
internal/hub/                   `ccdash hub serve`: web portal, OIDC login, device registry, tunnel endpoint, embedded SPA (web/)
internal/hubcfg/                device-side hub join state ($XDG_STATE_HOME/ccdash/hub.json)
internal/tunnel/                device ⇄ hub WebSocket + yamux tunnel
Dockerfile                      hub container image (published by .github/workflows/image.yml)
docs/usage_en.md                hands-on usage guide (English)
docs/usage_jp.md                hands-on usage guide (Japanese)
install.sh                      curl-installable shell installer
.github/workflows/              CI + release pipelines
```

## State

- DB: `$XDG_STATE_HOME/ccdash/ccdash.sqlite` (default `~/.local/state/ccdash/`), `0600`
- Token: `$XDG_STATE_HOME/ccdash/token`, `0600`
- Hub join state (device token): `$XDG_STATE_HOME/ccdash/hub.json`, `0600`
- Server bind: `127.0.0.1:9123` by default (loopback only). `ccdash server
  --listen <addr>` opts a standalone collector into a non-default bind for
  remote mode — a non-loopback `--listen` keeps a second listener on
  `127.0.0.1:<port>` so hooks (always wired to loopback) keep landing; see
  "Remote mode" above. The embedded/managed collector (plain `ccdash`,
  `-k`) always stays loopback-only.
- ccdash hook entries are tagged with the `X-Ccdash-Managed: true`
  header (or point at `ccdash/hook.sh`) so install / uninstall are
  idempotent and don't collide with user hooks

## Contributing

Bug reports, feature requests, and pull requests are welcome. See
[CONTRIBUTING.md](./CONTRIBUTING.md) for the full guide. Anything that
makes CI green gets a fast read.

## License

[MIT](./LICENSE) © 2026 kameneko

## AI-assisted development

This project is built with help from [Claude Code](https://claude.com/claude-code)
(Anthropic). The `Co-Authored-By: Claude` lines in the commit log
reflect that. AI-generated code is still owned by the maintainer once
it's reviewed and merged.
</content>
