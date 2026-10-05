# 0003 — Hub: a web portal over outbound device tunnels

- Date: 2026-10-05
- Status: Accepted

## Context

The operator drives Claude Code on several machines and wants to do it from
a browser or phone. Claude's own Remote Control covered part of this, but a
dropped connection left the session unrecoverable from the remote side and
starting a new session there was awkward. Remote mode (`ccdash -r`) already
exposes the collector over HTTP, but it needs a terminal, a reachable
collector address, and a copy of the token on each client.

Constraints from the home lab:

- Apps are reached over Tailscale (subnet router → `*.g3.lab-dev.net`
  behind proxy02 / ingress-nginx on home-k8s); SSO is Cloudflare Access
  used as an OIDC provider ("Access for SaaS", the Grafana / ArgoCD
  pattern), not an edge gate.
- The operator does not want the portal to connect *into* each machine.

## Decision

Add a third role to the binary, `ccdash hub serve`, run on the home server:

- **Devices dial out.** A joined device's collector (`ccdash hub join`
  writes `$XDG_STATE_HOME/ccdash/hub.json`) opens a WebSocket to the hub and
  runs yamux on it with the roles inverted: the hub is the yamux client and
  opens one stream per request; the device serves those streams with an
  ordinary `http.Server` (Hijack works, so the existing `pty-raw` upgrade is
  reused untouched). No inbound port on any device.
- **The device decides what the hub may do.** Tunnel requests skip the
  loopback token check and instead pass an allowlist
  (`internal/server/hub.go`) gated by the device's own settings: reads,
  session metadata edits, PTY control (attach toggle), approval decisions
  (approve toggle). Hooks, `/shutdown` and settings writes are never
  reachable, so a compromised hub cannot re-enable the device's toggles. A
  new `hub_enabled` setting (in the secure preset) takes a device offline.
- **Login is OIDC at the app**, with an email allowlist, signed
  HttpOnly/SameSite cookies, PKCE + nonce, Go 1.25 cross-origin protection
  on mutating API calls and the WebSocket library's same-origin check on the
  terminal socket. Device tokens are random, shown once, stored hashed.
- **Sessions open as a chat, not a terminal.** The portal renders the
  transcript JSONL as messages and types into the ccdash-hosted PTY through
  a new non-exclusive `POST /pty/{key}/input` (multi-line text as a
  bracketed paste, then Enter). Stopped sessions resume with the message as
  the first prompt (`claude --resume <id> "<msg>"`). TUI-only prompts are
  surfaced from the server-side emulator via `GET /pty/{key}/text`: a
  `❯`-cursor menu on screen becomes a card with one-tap answers (digits for
  numbered dialogs, arrows + Enter otherwise). The xterm.js view of the same
  PTY (bridged by the hub from the browser WebSocket to `/pty/{key}/stream`)
  is one button away.
- The hub ships as a container (`Dockerfile`, `ghcr.io/<owner>/ccdash`) with
  its own SQLite (devices + cookie key); the SPA is embedded vanilla JS.

## Alternatives considered

- **Headless `claude -p --input-format stream-json` per session** would give
  token-level streaming and structured tool events — a "real" chat app — but
  permission prompts then depend on the SDK's undocumented control protocol
  (`--permission-prompt-tool stdio`), and the session could no longer be
  picked up in a terminal mid-flight. The PTY approach keeps one process that
  the TUI, fullscreen attach, the xterm view and the chat all share.
- **Hub dials devices over Tailscale** (reuse remote mode): rejected by the
  "don't connect into the machines" constraint, and every device would need a
  reachable listener plus the token on the hub.
- **Trust Cloudflare Access at the edge** (Tunnel + Access header): the home
  lab keeps app UIs on the LAN and uses Access only as an IdP; validating the
  OIDC login in-app works the same with or without a tunnel in front.

## Consequences

- A device is reachable only while a collector runs there (`ccdash server` as
  a user service, or `ccdash -k`). The embedded collector of a plain `ccdash`
  also connects, but only for the TUI's lifetime.
- Chat updates arrive at transcript granularity (whole messages, ~1.5 s
  polling), not token by token.
- The raw stream stays single-client: the xterm view gets a 409 while a TUI
  fullscreen attach holds the PTY (the chat view is unaffected — it uses the
  input endpoint).
- Sessions started outside ccdash are read-only in the portal until resumed
  there (which starts a second process if the original is still running; the
  portal asks first).
