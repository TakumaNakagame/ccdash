# Hub mode (web portal)

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
  sections, plus one tab per group; rename / group / project / pin /
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
- **Grid**: several sessions side by side, each tile the full chat (cards,
  approvals, images, stop…); a part-filled last row stretches across, and on
  a tablet typing into a tile maximises it.
- **Files and documents**: images and other files Claude sends you
  (SendUserFile) show in the chat; Markdown and HTML reports Claude writes
  open in a preview — a large viewer or a sidebar beside the chat.
- **Look**: light / dark / system theme and an accent colour, saved per
  account (shared by the browser and the installed app).
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
