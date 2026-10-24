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
`ccdash hub serve`; TLS terminates in front of it — a full walkthrough with
Compose, a reverse proxy and the login provider is in
[Deploying the hub](#deploying-the-hub-step-by-step)):

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
at least one allowed email. The hub is for one person: list only your own
addresses (several are fine, e.g. work and personal) — everyone who logs in
can prompt, approve and start sessions on every joined device. Proxies in front of it must pass WebSocket
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

**Read tokens (machine clients).** A bot or script of your own that can't
do a browser login can read the hub with `Authorization: Bearer <token>`. Configure the
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

**Peer access (multi-host MCP).** In 端末 → 管理 → 他の端末へのアクセス the
operator can let one device reach the others through the hub with its own
device token: オフ (default) / 閲覧 / 操作. `ccdash mcp` on that device then
lists every host's sessions and can start / stop / resume / organize them
on a named host (`host` argument, `list_hosts`). Peers reach only a narrow
route set (`internal/hub/peer.go`) — never approvals, typing into a PTY,
device management or settings — and the target device's own allowlist
still applies. See `docs/decisions/0007-multihost.md`.

## Deploying the hub (step by step)

A walkthrough for a first hub on one server. You need a machine that is
always on (a home server, a small VPS, a NAS that runs containers) and one
of two ways to log in:

- **OIDC** — any OpenID Connect provider (Cloudflare Access for SaaS,
  Google, a self-hosted Authelia / Pocket ID / Keycloak …) with the hub
  behind a reverse proxy that terminates TLS. Reachable from anywhere.
- **Tailscale** — no identity provider: `tailscale serve` puts the hub on
  your tailnet with HTTPS and tells it who you are. Reachable only from
  your own devices on the tailnet.

### 1. Register the hub with your login provider (OIDC)

Pick the hub's address first, e.g. `https://ccdash.example.net`, then
create an OIDC client ("web application", authorization-code flow) with:

- **Redirect URI**: `https://ccdash.example.net/auth/callback`
- **Scopes**: `openid email profile` (the hub matches the `email` claim
  against `--allowed-email`; an `email_verified: false` login is refused)

Note the **issuer URL**, **client ID** and **client secret**. Issuers:

| Provider | Where | Issuer |
|---|---|---|
| Cloudflare Access for SaaS | Zero Trust → Access → Applications → Add → SaaS → OIDC | `https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client-id>` |
| Google | Google Cloud console → APIs & Services → Credentials → OAuth client ID (Web application) | `https://accounts.google.com` |
| Self-hosted | the provider's client / application settings | its issuer (the URL that serves `/.well-known/openid-configuration`) |

The issuer must be reachable from the hub at start-up (the hub reads its
discovery document).

### 2. Run the container (Docker Compose)

The image is `ghcr.io/takumanakagame/ccdash` (`latest` = the newest
release; `vX.Y.Z` tags too). Its entrypoint is `ccdash hub serve`, so
`command:` only adds flags.

```sh
mkdir -p ccdash-hub/data && cd ccdash-hub
sudo chown 65532:65532 data            # the image runs as distroless "nonroot" (uid 65532)
printf '%s' '<client secret>' > oidc-secret && chmod 600 oidc-secret
```

```yaml
# compose.yaml
services:
  hub:
    image: ghcr.io/takumanakagame/ccdash:latest
    restart: unless-stopped
    environment:
      CCDASH_HUB_PUBLIC_URL: https://ccdash.example.net
      CCDASH_HUB_OIDC_ISSUER: https://accounts.google.com
      CCDASH_HUB_OIDC_CLIENT_ID: <client id>
      CCDASH_HUB_ALLOWED_EMAILS: you@example.com   # your own addresses only, comma-separated
    command: ["--oidc-client-secret-file", "/run/secrets/oidc"]
    secrets: [oidc]
    volumes:
      - ./data:/data                   # hub.sqlite: devices, cookie key, VAPID keys, prefs
    ports:
      - 127.0.0.1:8080:8080            # only the reverse proxy reaches it
secrets:
  oidc:
    file: ./oidc-secret
```

```sh
docker compose up -d
docker compose logs -f hub
curl -s http://127.0.0.1:8080/healthz
```

Without Docker, the same flags work on the release binary
(`ccdash hub serve --listen 127.0.0.1:8080 --data-dir … --public-url …`),
e.g. as a systemd service.

### 3. Put a reverse proxy in front (TLS + WebSocket)

The proxy must pass WebSocket upgrades (the device tunnel and the terminal
view) and keep long-lived connections open; it must also pass the original
`Host`. Caddy does all of that by default and gets the certificate itself:

```caddyfile
ccdash.example.net {
	reverse_proxy 127.0.0.1:8080
}
```

nginx needs it spelled out:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;   # map $http_upgrade → "upgrade" / "close"
    proxy_read_timeout 1h;
    proxy_buffering off;
}
```

Don't put a second login gate (e.g. a Cloudflare Access *application* on
the hostname) in front: devices dial the tunnel with their device token and
can't pass an interactive login. Use the provider as the hub's OIDC issuer
instead.

### Break glass (optional)

If the OIDC provider is down, nobody can log in. Setting
`CCDASH_HUB_BREAK_GLASS_PASSWORD` (environment only) adds a password login at
`https://<hub>/auth/break-glass`; failed OIDC logins link to it. Sessions
from it last one hour and end as soon as the variable is removed; 5 wrong
passwords within 10 minutes lock it for 10 minutes (per replica); every
attempt is logged as `hub: break-glass: …` with the sender's address, never
the password. Device tunnels, read tokens and peers are unaffected. Use it
only on a hub that isn't reachable from the internet (LAN / VPN), and keep
the password in your password manager.

### Alternative: Tailscale instead of OIDC

On a machine in your tailnet (no container needed):

```sh
ccdash hub serve --tailscale-auth --listen 127.0.0.1:8080 \
  --public-url https://<machine>.<tailnet>.ts.net \
  --allowed-email you@example.com      # your Tailscale login
tailscale serve --bg 8080
```

`--public-url` must be the name `tailscale serve` answers on (the hub
answers 421 to any other host). Run the first command as a service to keep
it up.

### 4. Log in and join the machines

1. Open the public URL and log in. The portal shows no devices yet.
2. **端末を追加** (Add device): name it; a one-time token appears.
3. On that machine: `ccdash hub join --url https://ccdash.example.net`,
   paste the token, and keep a collector running (`ccdash server` as a
   service, or `ccdash -k`). The device turns online within ~30 s.
4. On a phone: open the URL, log in, "Add to Home screen", then turn on 🔔
   for push notifications.

If a device stays offline, `ccdash hub status` on it shows whether it is
joined, whether **Hub connection** is on and whether a collector runs; the
collector log (`~/.local/state/ccdash/ccdash.log`, or the service's
journal) has the tunnel's errors; the usual causes are a proxy that drops WebSocket
upgrades and a public URL that doesn't match what the browser uses.

## Several replicas (Kubernetes)

On Kubernetes the hub can run as two or more replicas so a node drain or a
rollout doesn't take the portal down (docs/decisions/0008-hub-ha.md):

```
ccdash hub serve --listen :8080 --public-url https://ccdash.example.net \
  --state-secret ccdash-hub-state \
  --cluster-peers ccdash-hub-peers.<namespace>.svc.cluster.local --cluster-listen :8081
```

- `--state-secret` keeps the state in that Secret instead of hub.sqlite.
  The service account needs `create` on Secrets in the namespace and
  `get` / `update` on that one. If the Secret doesn't exist and
  `<data-dir>/hub.sqlite` does, the file is imported once, so devices,
  logins and push subscriptions carry over.
- `--cluster-peers` names a headless Service selecting the hub Pods. Each
  device keeps one tunnel to whichever replica it reached, and the others
  forward to it on the cluster port (keep that port off the ingress).
  `$POD_IP` (downward API) tells a replica which address is its own.

## Tracing (OpenTelemetry)

The hub and the collector (`ccdash server`, `ccdash web`) send OTLP/HTTP
traces when the standard environment names a receiver; without it they
create no TracerProvider and read or write no trace headers:

```
OTEL_EXPORTER_OTLP_ENDPOINT=http://alloy-otlp.tracing.svc:4318   # or …_TRACES_ENDPOINT (full URL, /v1/traces)
OTEL_SERVICE_NAME=ccdash-hub          # default: ccdash-hub / ccdash-collector
OTEL_TRACES_SAMPLER=parentbased_traceratio OTEL_TRACES_SAMPLER_ARG=0.2   # optional; default parent-based always-on
```

Only OTLP/HTTP is sent (`OTEL_EXPORTER_OTLP_PROTOCOL=grpc` is logged and
ignored); `OTEL_SDK_DISABLED=true` turns it off again. The hub joins an
incoming `traceparent` (from an ingress that traces) and passes it through
the tunnel, so browser → hub → device collector is one trace. Polling and
streams get no span — every GET except login, `/s/<id>`, git diff / file
and usage, plus statusline, per-tool hooks, `seen` and `resize`
(`internal/tracing.Traced`). Session starts (`pty.start`), the post-restart
resume, restarts and title generation have their own spans. The hosted
claudes inherit the collector's environment, OTEL variables included;
Claude Code only exports its own telemetry with
`CLAUDE_CODE_ENABLE_TELEMETRY=1`.

## Local portal (`ccdash web`)

The same portal for **this machine only**, with no hub server: one
process serves it on `127.0.0.1` and talks to this machine's collector
in-process. Meant for a machine nobody may reach from outside (a work
laptop) where you still want the web UI — chat view, approval cards,
grid, diff review, notifications. See
`docs/decisions/0005-local-portal.md`.

```sh
ccdash web              # starts http://127.0.0.1:9124 in the background, opens a one-time login link
ccdash web open         # log in again later (another browser, expired cookie, launchd)
ccdash web status       # running? pid, URL, which collector, log file
ccdash web restart      # restart; the claude sessions it hosts are resumed
ccdash web stop         # stop it, with the collector and claude sessions it runs
ccdash web stop --resume  # ... saving those sessions; the next `ccdash web` resumes them
ccdash web --foreground # stay in this terminal (Ctrl-C stops); for launchd / systemd
ccdash web --port 9200 --no-open
```

- **Background:** `ccdash web` starts `ccdash web --foreground` detached
  from the terminal (its own session, stdin `/dev/null`) and returns once
  it answers. Its output goes to `$XDG_STATE_HOME/ccdash/web.log` (rotated
  to `web.log.1` past 10 MB at start); `web.json` next to it records the
  PID, port and collector for `stop` / `restart` / `status`. If one is
  already running, `ccdash web` only opens its login link. The child keeps
  the environment of the shell you ran it from, so hosted claude sessions
  start exactly as from a foreground `ccdash web`; closing the terminal no
  longer stops the portal. Started by launchd (parent PID 1) or a systemd
  unit it stays in the foreground by itself.
- **Collector:** if one is already running (`ccdash server`, `ccdash -k`,
  a TUI's), `ccdash web` links to it; otherwise it runs the collector
  itself, exactly as `ccdash server` would (hooks keep working). If a
  linked collector stops, `ccdash web` starts its own.
- **Stop and restart mean one thing each.** A collector `ccdash web` runs
  lives and dies with it, and every claude it hosts ends with that
  collector, however it stops (the collector closes its PTYs on the way
  out). `ccdash web stop` (SIGTERM) and `POST /shutdown` to its collector
  — the TUI's **Restart ccdash** — stop the whole portal; it never starts
  a second collector in the same process. `ccdash web restart` and the
  portal's **↻ 再起動** use `POST /api/restart`: the hosted sessions are
  saved, their claudes closed, `ccdash web` re-execs itself (same PID and
  log) and resumes them. Linked to another collector, `stop` / `restart`
  only touch the portal; the sessions live in that collector. (The TUI's
  **Restart ccdash** on a collector run by `ccdash web` stops the portal
  too; the TUI then starts a `ccdash server`, which resumes the sessions —
  run `ccdash web` again for the portal.)
- **Loopback only.** It binds `127.0.0.1` (no `--listen`), answers 421 to
  any `Host` header but `127.0.0.1` / `localhost` / `[::1]` (DNS
  rebinding), and keeps the hub's cross-origin checks.
- **Login:** the link in `$XDG_STATE_HOME/ccdash/web-login` (0600) carries
  a one-time token; opening it sets an HttpOnly, SameSite=Strict cookie
  for 30 days (renewed while you use the portal). The token is replaced
  after every use; `ccdash web open` opens the current one. No OIDC, no
  logout, no device management — the only device is this machine.
- **What the portal may do** is the hub allowlist above, with the same
  **Attach** / **Approval blocking** gates. The **Hub connection** toggle
  doesn't apply: it governs the tunnel to an external hub, and the local
  link never leaves the process. A machine can be joined to a hub and run
  `ccdash web` at the same time.
- **No outbound traffic** from the portal: xterm.js, marked and DOMPurify
  are embedded in the binary (`internal/hub/web/vendor/`), and fonts are
  the system's. The one exception is notifications: turning on 🔔
  subscribes the browser to its push service (FCM for Chrome), and the
  portal sends encrypted Web Push messages there. Chrome shows them with
  the tab closed as long as Chrome itself runs; a click opens the session.
  If they don't arrive, 🔔 → **テスト通知** walks the chain and names the
  broken link (permission, the OS showing notifications, the
  subscription, hub → push service with its HTTP answer, push service →
  browser). Behind a proxy, give `ccdash web` (and a separate collector)
  `HTTPS_PROXY` — in the launchd plist's `EnvironmentVariables`.
- **State:** prefs, quick commands, VAPID keys and push subscriptions live
  in `$XDG_STATE_HOME/ccdash/web/hub.sqlite`, separate from a hub's.

**Keeping it running on macOS (launchd).** Save as
`~/Library/LaunchAgents/dev.ccdash.web.plist`, adjust the paths, then
`launchctl load ~/Library/LaunchAgents/dev.ccdash.web.plist` and
`ccdash web open` once:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.ccdash.web</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/.local/bin/ccdash</string>
    <string>web</string>
    <string>--foreground</string>
    <string>--no-open</string>
  </array>
  <!-- launchd starts with an empty environment: SHELL lets hosted
       claude sessions start through your login shell (and its PATH);
       PATH must reach claude, curl and git. -->
  <key>EnvironmentVariables</key>
  <dict>
    <key>SHELL</key><string>/bin/zsh</string>
    <key>PATH</key><string>/Users/you/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/you/Library/Logs/ccdash-web.log</string>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/ccdash-web.log</string>
</dict>
</plist>
```

launchd must keep the process it started, hence `--foreground` (a
portal started by launchd stays in the foreground even without it). With
`--no-open` the login link is never written to the log; use
`ccdash web open`. Stop it with `launchctl unload` on the same file —
`ccdash web stop` would only have launchd start it again (`KeepAlive`);
`ccdash web restart` works.
Running the TUI as well is fine: it finds this collector and uses it.
