---
name: ccdash-setup
description: Install, configure, verify, troubleshoot or uninstall ccdash (the dashboard for many Claude Code sessions — TUI, local web portal `ccdash web`, multi-machine hub `ccdash hub`, MCP server) on this machine, and answer "how do I … in ccdash" questions from its docs. Use when the user asks to set up / install / try ccdash, connect a machine to a ccdash hub, deploy a hub, fix ccdash not showing sessions, or remove it.
---

# ccdash setup

You are setting up ccdash for the person in front of you, on their machine.
Speak their language. They may be new to it: explain each step in a line or
two, and before you change any of their config files, say which file and
what goes in.

Facts you can rely on (verify with `ccdash <command> --help` when unsure —
the binary's help is authoritative):

- Repo: https://github.com/TakumaNakagame/ccdash — docs are readable as raw
  files under `https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/`:
  `docs/getting-started_en.md` (or `_jp`), `docs/usage_en.md` (or `_jp`),
  `docs/hub.md`, `docs/remote-mode.md`, `docs/security.md`, `README.md`.
  Read the relevant one before answering a question you are not sure of.
- Releases: linux / darwin × amd64 / arm64. Windows only inside WSL2.
- State lives in `$XDG_STATE_HOME/ccdash` (default `~/.local/state/ccdash`):
  `ccdash.sqlite`, `token` (secret), `hook.sh`, `ccdash.log`, `hub.json`
  (secret), `web.log`.
- The collector listens on `127.0.0.1:9123`; `GET /healthz` needs no token.

## Rules

- **Never print secrets**: the contents of `~/.local/state/ccdash/token`,
  `hub.json`, device tokens, OIDC client secrets, read tokens. Check that a
  file exists or compare lengths; don't `cat` it.
- **Hub device tokens are typed by the person, not by you.** Ask them to run
  `ccdash hub join --url <hub>` in their own terminal (it prompts for the
  token, keeping it out of shell history and out of this conversation).
- Ask before writing a service file (systemd unit / launchd plist) and
  before running anything with `sudo`.
- ccdash is for **one person's own sessions**. If the request is to let
  other people operate someone's sessions, or to share one Claude account,
  say that ccdash doesn't support that use (README "Who it's for").
- Interactive programs (`ccdash` the TUI, `ccdash hub join` without
  `--token`) can't run inside your Bash tool. Ask the person to run them in
  their terminal (or with `! <command>` in Claude Code when no input is
  needed).

## Step 1 — Look at the machine

Run these and summarize what you find (don't dump raw output):

```sh
uname -sm; grep -qi microsoft /proc/version 2>/dev/null && echo WSL
for c in claude curl git tmux ccdash; do printf '%-7s ' "$c"; command -v "$c" || echo '-'; done
ccdash --version 2>/dev/null
curl -s -m 2 http://127.0.0.1:9123/healthz && echo ' <- collector running'
grep -c 'ccdash/hook.sh' ~/.claude/settings.json 2>/dev/null   # >0 = hooks already installed
ccdash hub status 2>/dev/null
```

- No `claude`: stop; ccdash needs Claude Code installed and signed in.
- No `curl`: the hooks need it; ask them to install it.
- Native Windows (not WSL): unsupported; suggest WSL2.
- Already installed: skip to what is missing; offer `ccdash update`.

## Step 2 — Ask what they want

Use AskUserQuestion (one question, these options; recommend A or B for a
first try):

| Option | Meaning |
|---|---|
| A. Terminal only | TUI (`ccdash`), collector kept running in the background |
| B. Browser, this machine only | `ccdash web` on 127.0.0.1 — good for a work laptop |
| C. Several machines + phone | a hub (web portal on a server) + `ccdash hub join` on each machine |
| D. TUI for another machine's collector | remote mode |

Also ask whether this is a **work machine**; if so, mention the secure
preset and outbound traffic in Step 6.

## Step 3 — Install the binary

Skip if `ccdash` exists. Download the script, glance at it, run it:

```sh
tmp=$(mktemp -d) && curl -fsSL -o "$tmp/install.sh" https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh
sh "$tmp/install.sh"
```

It installs into a `PATH` directory (`$(brew --prefix)/bin` with Homebrew,
else `/usr/local/bin` if writable on macOS, else `~/.local/bin`). If that
directory isn't on `PATH`, say how to add it to their shell rc.
`failed to resolve latest release tag` = GitHub's anonymous rate limit: the
script uses `GITHUB_TOKEN` / `GH_TOKEN` / `gh auth token` when present, or
pin a version from https://github.com/TakumaNakagame/ccdash/releases with
`CCDASH_VERSION=vX.Y.Z sh "$tmp/install.sh"`.

## Step 4 — Connect it to Claude Code

Explain first, then run `ccdash setup`. It is idempotent and keeps the
person's own settings:

- `~/.claude/settings.json`: ccdash hook entries (theirs untouched) and a
  status-line relay that still runs their own statusLine command
- `~/.claude.json`: MCP server `ccdash` at user scope
- `permissions.allow`: `mcp__ccdash__set_status`

Variants: `--no-mcp` (hooks only), `--read-only-mcp` (MCP without the
organizing / start / stop tools).

Tell them: **running claude sessions — including this one — pick up the
hooks and the MCP server only after a restart.**

## Step 5 — Keep it running, per option

**A (and the device side of C / D).** A collector must run for events to be
recorded while the TUI is closed.

- Quick: `ccdash -k` (the person runs it; it opens the TUI).
- At login, Linux / WSL with systemd (check `systemctl --user status`
  works; on WSL systemd must be enabled in `/etc/wsl.conf`):

  ```ini
  # ~/.config/systemd/user/ccdash.service
  [Unit]
  Description=ccdash collector

  [Service]
  ExecStart=<absolute path of ccdash> server
  Restart=on-failure

  [Install]
  WantedBy=default.target
  ```

  then `systemctl --user daemon-reload && systemctl --user enable --now ccdash.service`.
  For the collector to keep running after logout on a server:
  `loginctl enable-linger "$USER"` (ask first).
- At login, macOS: a LaunchAgent `~/Library/LaunchAgents/dev.ccdash.server.plist`
  — use the plist in `docs/hub.md` ("Local portal") with ProgramArguments
  `<ccdash path> server`, Label `dev.ccdash.server`, and `PATH` containing
  the directories of `claude`, `ccdash`, `curl` and `git`
  (`dirname "$(command -v claude)"` etc.), `SHELL` = their login shell. Load
  with `launchctl load <plist>`.

**B.** `ccdash web` (starts in the background, opens a one-time login link;
it runs a collector itself when none is running). `ccdash web open` prints /
opens the login link again; `ccdash web status` shows where it runs. At
login: the same service as A with `web --foreground --no-open` instead of
`server` (launchd Label `dev.ccdash.web`); after it starts,
`ccdash web open` once. Don't install both a `server` and a `web` service —
`web` already includes a collector.

**C.** Two parts.

1. *The hub server* (once). Read `docs/hub.md`, section "Deploying the hub
   (step by step)", and walk them through it: pick OIDC (any provider;
   redirect URI `<public-url>/auth/callback`; scopes `openid email profile`)
   or Tailscale (`--tailscale-auth` + `tailscale serve`), then Docker
   Compose (`ghcr.io/takumanakagame/ccdash`, data dir owned by uid 65532)
   and a reverse proxy that passes WebSocket upgrades and the `Host`
   header. `--allowed-email` must list only their own addresses. They create
   the OIDC client themselves; they put the client secret in a file — never
   paste it here.
2. *Each machine*: Steps 3–4, a running collector (A), then the person
   runs `ccdash hub join --url https://<hub>` in their terminal and pastes
   the token from the portal's 端末を追加 (Add device). Check with
   `ccdash hub status` and the portal (online within ~30 s).

**D.** Follow `docs/remote-mode.md`: on the host with the sessions,
`ccdash server --listen <ip>:9123` (after one plain start so the token
exists); copy its token to this machine with `scp` (the person runs it);
`ccdash remote set --url http://<ip>:9123 --token-file <path>`; then
`ccdash -r`.

## Step 6 — Verify and hand over

```sh
curl -s -m 2 http://127.0.0.1:9123/healthz   # collector answers (B: ccdash web status)
grep -c 'ccdash/hook.sh' ~/.claude/settings.json
ccdash sessions | head                       # sessions are listed
```

Then ask the person to open `ccdash` (or the portal) in one terminal and
start a new `claude` in another: the new session should appear as working
within seconds. If it appears only as idle / stopped and never active,
hooks aren't arriving (see Troubleshooting).

Finish with a short "good to know" list:

- While a collector runs, a permission prompt in the terminal can wait up
  to 25 s for an answer in ccdash before Claude's own dialog shows. Turn
  **Approval blocking** off in the settings (`,` in the TUI) if that bothers
  them.
- Typing into a session from the TUI's right pane or the portal works for
  sessions ccdash started (TUI `n`, portal new session, resume); a claude
  started in a plain terminal can be watched and approved.
- Titles are generated with `claude -p` (a little quota). Settings:
  **Auto-generate titles**, **Title generation via claude -p**.
- Sessions untouched for 7 days are archived (`X` shows them;
  `auto_archive_days`).
- `ccdash trust <dir>` stops new sessions under `<dir>` from stopping at
  the folder-trust question.
- Work machine: outbound traffic is only the update check / `ccdash update`
  (GitHub), title generation (through their own `claude`) and Web Push if
  they turn on portal notifications. **Apply secure preset** in the
  settings turns off approval holding, title generation, attach,
  settings.json rewrites and the hub connection at once. Behind a proxy,
  give the services `HTTPS_PROXY`.
- `?` in the TUI lists every key; the usage guide covers the rest.

## Troubleshooting

| Symptom | Check / fix |
|---|---|
| No sessions at all | `ccdash setup` again; restart claude; is `curl` on the PATH that claude sees? |
| Sessions show but never "working" | hooks not arriving: collector running (`/healthz`)? `grep -c ccdash/hook.sh ~/.claude/settings.json`; restart claude |
| Hook calls get 401 in `ccdash.log` | token changed: `ccdash install-hooks` rewrites it (or restart the collector with Auto-rewrite settings.json on) |
| `address already in use` on 9123 | another collector runs: `ccdash web status`, `systemctl --user status ccdash`, `pgrep -fl 'ccdash server'` |
| Portal login link lost / expired | `ccdash web open` |
| Device stays offline on the hub | `ccdash hub status`; collector log lines starting `hub:`; proxy must pass WebSocket upgrades; `--public-url` must match the browser's URL |
| Sessions started from ccdash hang at "trust this folder?" | answer it in the portal / TUI, or `ccdash trust <parent dir>` |
| launchd service can't start claude | `PATH` / `SHELL` missing in the plist's EnvironmentVariables |

Logs: `~/.local/state/ccdash/ccdash.log` (collector embedded in the TUI or
started by `ccdash server` in a terminal: stderr), `web.log` for
`ccdash web`, `/tmp/ccdash-server.log` for `ccdash -k`,
`journalctl --user -u ccdash` for the systemd unit.

## Uninstall

Explain, then (with their OK):

```sh
ccdash web stop 2>/dev/null; systemctl --user disable --now ccdash.service 2>/dev/null   # or launchctl unload <plist>
ccdash hub leave 2>/dev/null
ccdash uninstall-hooks                  # their own hooks and statusLine are restored / kept
claude mcp remove --scope user ccdash
rm -rf ~/.local/state/ccdash ~/.config/ccdash
rm "$(command -v ccdash)"
```

Remove the service file they had you write, and, if they want,
`mcp__ccdash__set_status` from `permissions.allow` in
`~/.claude/settings.json`.
