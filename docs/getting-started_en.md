# Getting started with ccdash

A first-time walkthrough: install, check that it works, then pick the setup
that fits you. About 15 minutes. Every feature is described in the
[usage guide](usage_en.md).

> 日本語: [getting-started_jp.md](getting-started_jp.md)

## What it does

With many Claude Code sessions running at once it gets hard to tell which
one waits for an approval, which one asked a question, and which one is
done. ccdash watches every session through Claude Code's hooks and shows
them in one place, where you can also approve, reply, resume and start
sessions.

Three ways in:

| Way in | Command | When |
|---|---|---|
| Terminal (TUI) | `ccdash` | Start here. Every session in one list, the selected one on the right |
| Browser | `ccdash web` (this machine only) / `ccdash hub` (several machines + phone) | Reply and approve in a chat view, check from a phone |
| MCP | `ccdash mcp` (registered by `ccdash setup`) | Ask Claude itself "which sessions are waiting for me?" |

## Let an AI set it up

To have Claude Code do the setup, add this repository's Claude Code plugin
(it carries a setup skill). Inside Claude Code:

```text
/plugin marketplace add TakumaNakagame/ccdash
/plugin install ccdash@ccdash
```

Then ask "set up ccdash". The `ccdash-setup` skill checks your environment,
asks how you want to use it and follows the steps below, saying which
config files it will change before it changes them.

Without the plugin, this prompt does the same:

```text
Read https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/plugins/ccdash/skills/ccdash-setup/SKILL.md
and set up ccdash by following it.
```

The rest of this page is the manual route.

## Requirements

- **OS**: macOS or Linux (amd64 / arm64). On Windows, inside WSL2 (native
  Windows is not supported).
- **Claude Code**: the `claude` command, signed in.
- **curl**: the hooks use it to post events.
- Nice to have: `git` (branch display, diff review), `tmux` (jump to a
  session's pane from the TUI).
- **Language**: English or Japanese. The portal follows your browser
  (change it under ⚙ Display → Language); the TUI's and CLI's own text
  follows `$LANG` (change it under settings `,` → Language).

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
ccdash --version
```

It downloads the latest GitHub release into a directory on your `PATH`
(`$(brew --prefix)/bin` on macOS with Homebrew, `~/.local/bin` on Linux).
To read the script first, download it with `curl -fsSLO …/install.sh` and
run `sh install.sh`. `CCDASH_INSTALL_DIR=/path` picks another directory.

`failed to resolve latest release tag` means GitHub's anonymous API rate
limit. If you are logged in with `gh`, its token is used automatically;
otherwise pin a version, e.g. `CCDASH_VERSION=v1.0.0`.

## 2. Connect it to Claude Code

```sh
ccdash setup
```

Safe to re-run. It changes three things and removes nothing of yours:

| File | What is added |
|---|---|
| `~/.claude/settings.json` | ccdash's hooks (yours stay) and a status-line relay (your own statusLine command still runs) |
| `~/.claude.json` | the `ccdash` MCP server (user scope) |
| `permissions.allow` in `~/.claude/settings.json` | `mcp__ccdash__set_status`, so the per-turn status call never prompts |

It also creates `~/.local/state/ccdash/` (database, token, hook forwarder).
Hooks post to `127.0.0.1` only; nothing leaves the machine.

**Restart any claude session that is already running**: hooks and MCP
servers are read at start.

## 3. Check that it works

```sh
ccdash            # terminal 1: the dashboard
claude            # terminal 2: use it as usual
```

Ask something in terminal 2; within seconds the session shows up in
terminal 1. If it doesn't:

- Not in `ccdash sessions` either: the hooks don't arrive. Re-run
  `ccdash setup` and restart claude.
- `curl -s http://127.0.0.1:9123/healthz` doesn't answer: no collector is
  running (see "Keep it running" below).
- The log is `~/.local/state/ccdash/ccdash.log` (collector embedded in the TUI).

TUI basics: `j`/`k` to move, `enter` to open, `n` for a new session, `?`
for every key, `q` to quit.

## 4. Pick a setup

| You want | Use | Steps |
|---|---|---|
| The terminal is enough | TUI + a collector that keeps running | [A](#a-terminal-only) |
| This machine in a browser too (a work laptop nobody may reach) | local portal `ccdash web` | [B](#b-in-a-browser-this-machine-only) |
| Several machines in one place, on your phone too | hub | [C](#c-several-machines--a-phone-hub) |
| A TUI here for a collector on another machine | remote mode | [D](#d-a-collector-on-another-machine-remote-mode) |

### A. Terminal only

`ccdash` runs the collector only while the TUI is open. To record what
happens while it is closed, keep a collector running:

```sh
ccdash -k          # start a detached collector, then the TUI (the collector outlives it)
```

To start it at login on Linux / WSL, use a systemd user unit:

```ini
# ~/.config/systemd/user/ccdash.service
[Unit]
Description=ccdash collector

[Service]
ExecStart=%h/.local/bin/ccdash server
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now ccdash.service
```

On macOS, use launchd: the [plist in hub.md](hub.md#local-portal-ccdash-web)
works with `web --foreground --no-open` replaced by `server`.

### B. In a browser (this machine only)

```sh
ccdash web         # starts http://127.0.0.1:9124 in the background and opens a one-time login link
```

Chat view, approval cards, grid, diff review and notifications, served on
`127.0.0.1` only, with no server elsewhere. If no collector runs,
`ccdash web` runs one itself.

- `ccdash web open` logs in again (another browser, an expired cookie).
- `ccdash web status` / `restart` / `stop` manage it.
- To start it at login: on macOS, the [launchd example](hub.md#local-portal-ccdash-web);
  on Linux, unit A with `ExecStart=%h/.local/bin/ccdash web --foreground --no-open`.

### C. Several machines + a phone (hub)

Run one hub (the web portal) on a home server and connect each machine to
it. Machines only dial out; no ports are opened on them.

1. Set up the hub: [Deploying the hub](hub.md#deploying-the-hub-step-by-step)
   (Docker Compose + a reverse proxy + OIDC, or Tailscale).
2. In the portal, **端末を追加** (Add device) shows a token. On each machine:
   ```sh
   ccdash hub join --url https://ccdash.example.net   # paste the token
   ccdash server                                      # keep it running, as in A
   ```
3. On a phone, "Add to Home screen" installs the portal as an app with
   notifications.

The hub is **for one person**: list only your own addresses in
`--allowed-email`. Anyone who can log in can prompt, approve and start
sessions on every joined machine.

### D. A collector on another machine (remote mode)

A TUI here for another machine's collector over your LAN / VPN; see
[remote-mode.md](remote-mode.md). If a browser is enough, C is simpler.

## Good to know from day one

- **A permission prompt in the terminal can take up to 25 s to appear.**
  While a collector runs, the PermissionRequest hook waits up to 25 s for an
  allow / deny in ccdash; without one, Claude's usual dialog follows. If
  that bothers you, turn **Approval blocking** off in the settings (`,` in
  the TUI).
- **You can type into sessions that ccdash started.** Those started with
  `n` in the TUI, the portal's new-session button, or a resume can be typed
  into from the right pane and the browser. A claude started in a plain
  terminal can be watched and approved from ccdash, but not typed into.
  Replying to a stopped session from the portal resumes it inside ccdash.
- **Titles are generated automatically** by one `claude -p` call per batch
  (likely secrets are masked first), which uses a little of your quota.
  Turn off **Auto-generate titles** (automatic only) or **Title generation
  via claude -p** (never run `claude -p`) to stop it.
- **Status declarations.** After `ccdash setup`, each session declares at
  the end of a turn whether it waits for your answer, waits for you to do
  something, reports, or carries on by itself. The first two count as
  "needs you".
- **Sessions untouched for 7 days are archived** (not pinned ones, project
  members or running ones). `X` shows the archive; `auto_archive_days`
  changes the period (0 = off).
- **Skip the "trust this folder?" question** for folders you use often:
  `ccdash trust ~/src`.

## On a work machine

- Outbound traffic: `ccdash update` and the TUI's update check (GitHub),
  title generation (through your own `claude`), and Web Push once you turn
  on portal notifications (through the browser's push service, encrypted).
  Without a hub, session content never leaves the machine.
- **Apply secure preset** in the settings (`,`) turns off approval holding,
  title generation, attach, settings.json rewrites and the hub connection
  in one go.
- Behind a proxy, give `ccdash web` / `ccdash server` `HTTPS_PROXY`.
- ccdash is not an Anthropic product. Check your company's rules for
  Claude and [Anthropic's terms](https://code.claude.com/docs/en/legal-and-compliance).
  ccdash never reads or stores Claude credentials.

## Undo

```sh
ccdash uninstall-hooks                 # remove the hooks and the status-line relay (yours stay)
claude mcp remove --scope user ccdash  # unregister the MCP server
rm -rf ~/.local/state/ccdash ~/.config/ccdash   # database, token, logs, client config
rm "$(command -v ccdash)"              # the binary
```

The `mcp__ccdash__set_status` entry in `permissions.allow` is harmless;
delete it from `~/.claude/settings.json` by hand if you like.

## Next

- [Usage guide](usage_en.md): every feature of the TUI, portal, tasks and MCP
- [Hub](hub.md): deploying the web portal, and what it can and can't do
- [Security](security.md): the threat model and which switch turns what off
- Stuck: [Troubleshooting](usage_en.md#14-troubleshooting), or open an
  [issue](https://github.com/TakumaNakagame/ccdash/issues)
