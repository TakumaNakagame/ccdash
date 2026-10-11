# Code layout and state

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
internal/mcpserver/             `ccdash mcp`: MCP tools over the Store seam (stdio)
internal/screen/                wire protocol for the live right pane (server emulator → TUI)
internal/screenprompt/          spots dialogs / questions on a hosted claude's screen
internal/claudecfg/             marks folders trusted in ~/.claude.json (`ccdash trust`)
internal/usage/                 token / API-price totals from transcripts
Dockerfile                      hub container image (published by .github/workflows/image.yml)
docs/usage_en.md                hands-on usage guide (English)
docs/usage_jp.md                hands-on usage guide (Japanese)
docs/                           hub, remote mode, security, TUI reference, design rules, decision records
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
  [Remote mode](remote-mode.md). The embedded/managed collector (plain `ccdash`,
  `-k`) always stays loopback-only.
- ccdash hook entries are tagged with the `X-Ccdash-Managed: true`
  header (or point at `ccdash/hook.sh`) so install / uninstall are
  idempotent and don't collide with user hooks
