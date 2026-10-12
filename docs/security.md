# Security

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
  [Remote mode](remote-mode.md).

### Explicitly out of scope

- Multi-user / shared-host deployments where operators don't trust each
  other (remote mode is for one operator's own collector, reachable from
  their own other machines — not a shared multi-tenant service)
- Public internet exposure of the collector, remote mode or not
- **No TLS in v1.** Remote mode's HTTP traffic (including the token
  header, prompts, and approval decisions) is unencrypted on the wire —
  intended for a trusted LAN or a VPN/Tailscale overlay, not an open or
  untrusted network
- Exposing the hub without its login (OIDC or `tailscale serve`); `--no-auth`
  is refused on a non-loopback listen
- Defense against the operator pasting their own secrets into prompts.
  The best ccdash can do is mask common token patterns before persisting.
  Claude itself already keeps a copy of every prompt in
  `~/.claude/projects/*.jsonl` regardless of ccdash.

### Mitigations in this codebase

- Loopback by default. The embedded/managed collector (plain `ccdash`,
  `-k`) is loopback-only, unconditionally — no flag changes that. A
  standalone `ccdash server` can opt into a non-loopback bind via
  `--listen`, which logs a clear warning and refuses to start without an
  existing auth token (see [Remote mode](remote-mode.md))
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
  immediately. The device-side allowlist (see [Hub mode](hub.md)) bounds what a
  compromised hub could do, and the **Hub connection** toggle (part of the
  secure preset) takes a device off the hub entirely. Optional read tokens
  (bearer, GET-only on five read routes) can read session lists and
  transcript tails — the raw transcript, unredacted — but can't write,
  drive a PTY or decide approvals; treat one like a portal login.
- DNS rebinding: a hub whose login a rebinding page could satisfy —
  `--no-auth`, `--tailscale-auth` (the page can set the login header
  itself) — answers 421 to any `Host` but `127.0.0.1` / `localhost` /
  `[::1]` and the public URL's host. The OIDC hub is behind its proxy and
  a rebinding page has no session cookie for it.
