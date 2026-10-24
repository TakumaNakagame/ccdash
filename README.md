# ccdash

[![License: MIT](https://img.shields.io/badge/License-MIT-ff922b.svg)](./LICENSE)
[![Built with Claude Code](https://img.shields.io/badge/Built%20with-Claude%20Code-d18ee2)](https://claude.com/claude-code)

**A dashboard for running many Claude Code sessions at once** — in your
terminal, in a browser, and on your phone.

ccdash watches every Claude Code session on a machine through hooks: which
are working, which are waiting for you (an approval, a question, a menu on
screen), and what each one has been doing. You can answer, reply, start and
resume sessions from one place instead of hunting through terminal tabs.

![The web portal: session list grouped into projects, and a chat with the selected session](docs/images/portal-desktop.png)

> Hobby project, built for one person's machines. Read [Security](docs/security.md)
> before exposing anything beyond your own network.

## Who it's for

ccdash is for **one person managing their own Claude Code sessions** — from
any number of machines and phones they own. Run it for yourself and don't
give anyone else a way to operate it (hub login, collector token, device
token).

- Not supported: other people sending prompts to or approving your
  sessions, sharing one Claude account within a team or family, or running
  ccdash as a Claude service for others (shared, resold or multi-tenant).
- ccdash drives the official `claude` binary, which signs in through
  Anthropic's own flow. ccdash never obtains, stores or relays Claude
  credentials or OAuth tokens.
- ccdash is an independent project, not made or endorsed by Anthropic.
  Using it doesn't change what Anthropic's terms
  ([Consumer Terms](https://www.anthropic.com/legal/consumer-terms),
  [Claude Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance))
  require of you; check them for your own plan and use.

## Three ways in

| | |
| --- | --- |
| **Terminal (TUI)** | `ccdash` — every session in one list, the selected one live in the right pane (a real terminal for sessions ccdash hosts), approvals with one key. |
| **Web portal / app** | `ccdash hub` — a home-server portal for all your machines. Sessions read as chats; reply, answer questions and approvals, review diffs, watch several at once on a grid. Installable as an app on a phone or tablet, with push notifications. |
| **MCP server** | `ccdash mcp` — lets Claude itself see and organize your sessions ("which sessions need me?", "put these into a project"), with links straight into the portal. |

![The TUI: sessions grouped into projects on the left, the selected session's conversation on the right](docs/images/tui.png)

<p>
  <img src="docs/images/phone-home.png" width="250" alt="Phone: sessions that need you, across devices">
  <img src="docs/images/phone-list.png" width="250" alt="Phone: the session list">
  <img src="docs/images/phone-chat.png" width="250" alt="Phone: a session as a chat">
</p>

## Quick start

**New here? Follow [Getting started](docs/getting-started_en.md)
([日本語](docs/getting-started_jp.md))** — install, a check that it works,
and which setup fits you (terminal only, a browser on this machine, or
several machines and a phone).

```sh
# install (Linux / macOS, amd64 / arm64) — or: go install ./cmd/ccdash
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh

ccdash setup           # once: hooks (keeps yours) + MCP server + the status rule
ccdash                 # open the dashboard; claude sessions show up as you use them
```

`ccdash` runs the collector only while the TUI is open. To keep collecting in
the background, run `ccdash -k` (detached collector + TUI) or `ccdash server`
as a service. `ccdash update` upgrades in place; `ccdash uninstall-hooks`
undoes the setup.

Hands-on guides: [English](docs/usage_en.md) · [日本語](docs/usage_jp.md)

**Let Claude set it up.** This repository is also a Claude Code plugin
marketplace with a setup skill. In Claude Code:

```text
/plugin marketplace add TakumaNakagame/ccdash
/plugin install ccdash@ccdash
```

then ask "set up ccdash" — it checks your machine, asks which setup you
want, says what it will change, installs and verifies. The skill also
answers "how do I …" questions about ccdash from these docs.

**Requirements**: macOS or Linux (amd64 / arm64; Windows via WSL2), Claude
Code signed in, `curl`; `git` and `tmux` are optional. The TUI is in
English; the web portal is in Japanese only for now.

## What it does

**See what's going on**
- Every session on the machine, found automatically — working, idle, or
  stopped — with titles generated from the conversation.
- **Needs you**: approvals, Claude's questions, menus and confirmations on a
  session's screen, and finished turns you haven't looked at, flagged in the
  list and pushed to your phone.
- The conversation of any session, live; usage and an API-price estimate per
  session and per day.

**Act on it**
- Approve or deny tool use, answer questions (single / multi choice or free
  text), pick options on screen — without switching terminals.
- Reply to any session, attach images, resume a stopped one, start new ones
  in a directory (optionally with a first prompt), stop or restart claude.
- Run a command Claude suggests (`! cmd`) with one tap after a confirmation.
- Review a session's git diff, leave line comments, send them back as one prompt.

**Keep it organized**
- Projects (named, coloured groups of sessions), pins, "watch later",
  archive (automatic after N days), custom titles and colours.
- A grid of several sessions side by side — each tile is the full chat.

![The grid: several sessions side by side, each a full chat](docs/images/portal-grid.png)

**Outputs**
- Files Claude sends you show in the chat; Markdown and HTML reports Claude
  writes open in a preview — large, or in a sidebar beside the chat.

![A Markdown document Claude wrote, previewed in a sidebar](docs/images/portal-preview.png)

## Going further

- **[Hub mode — the web portal](docs/hub.md).** Run `ccdash hub serve` on a
  home server behind OIDC (or `tailscale serve`), then `ccdash hub join` on
  each machine. Machines dial out to the hub; nothing on them is exposed.
  What the hub may do on a machine is limited by an allowlist on that machine.
- **[Local portal](docs/hub.md#local-portal-ccdash-web).** `ccdash web`
  serves the same portal for this machine only on 127.0.0.1 — no hub, no
  inbound or outbound connections (bar Web Push if you enable it). For a
  work laptop nobody may reach from outside.
- **[Remote mode](docs/remote-mode.md).** Point a TUI at a collector on
  another machine over your LAN / VPN: `ccdash remote set …`, then `ccdash -r`.
- **Status declarations.** Every session ends its turn with `set_status`:
  回答待ち (waits for your answer), 作業待ち (waits for you to do something),
  報告 (just reporting) or 自動で続行 (continues by itself) — shown in the
  TUI, portal and app; the first two count as "needs you".
- **MCP.** `ccdash setup` (or `claude mcp add --scope user ccdash -- ccdash mcp`) gives Claude
  read tools (overview, projects, sessions, a session's recent conversation),
  organizing tools (projects, titles, archive, watch later) and start / stop /
  resume for sessions ccdash hosts (only while Attach is on; one session at a
  time, never its own). It never answers approvals; `--read-only` keeps only
  the read tools and `set_status`. On a hub-joined machine every session
  carries a link that opens it in the portal.
- **Tasks.** A per-machine to-do list beside the sessions that Claude keeps
  up to date through MCP ("#11: the review is done"), in the TUI's task
  pane, the portal and `ccdash tasks`. See the usage guide, section 17.
- **[TUI reference](docs/tui.md)** — every key (also `?` in the app).
- **[Security](docs/security.md)** — threat model, and the switches that turn
  each risky capability off (approval blocking, title generation, attach,
  settings rewrite, hub connection; "Apply secure preset" turns off all).

## Commands

```sh
ccdash                      # TUI (embeds the collector while it runs)
ccdash -k                   # detached collector + TUI
ccdash server               # collector only (e.g. a systemd user service)
ccdash -r                   # TUI against a remote collector (see Remote mode)
ccdash sessions | approvals # plain-text listings
ccdash trust <dir>          # folders where new sessions skip the folder-trust question
ccdash hub serve | join     # web portal / connect this machine to it
ccdash web                  # the portal for this machine only, on 127.0.0.1
ccdash mcp                  # MCP server on stdio
ccdash tasks                # the to-do list (list / add / note / status / plan …)
ccdash update               # self-update (ccdash update channel dev: follow rc builds)
```

## More

- [Code layout and state files](docs/architecture.md) ·
  [Design rules for the portal](docs/design.md) ·
  [Decision records](docs/decisions/)
- Contributing: see [CONTRIBUTING.md](CONTRIBUTING.md) — anything that makes
  CI green gets a fast read.
- License: [MIT](./LICENSE) © 2026 kameneko
- The docs mention an Android app; it is a separate, unpublished companion.
  The installable web app (PWA) covers the same ground.
- Built with help from [Claude Code](https://claude.com/claude-code); the
  `Co-Authored-By: Claude` lines in the log reflect that. Screenshots use
  made-up data.
