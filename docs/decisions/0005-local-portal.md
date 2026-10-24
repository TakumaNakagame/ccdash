# 0005 — Local portal: the hub's web UI on 127.0.0.1 (`ccdash web`)

- Date: 2026-10-13
- Status: Accepted

## Context

The hub portal (0003) is the most comfortable way to use ccdash — chat
view, approvals as cards, grid, diff review, push notifications — but it
lives on a home server and reaches devices through a tunnel they dial
out to. On a work Mac that is not an option: connecting to the company
machine from outside is forbidden. Outbound traffic is monitored but not
blocked, so the bar is "nothing that looks odd", not "nothing at all".

What is wanted there is not remote control but the web UI itself, used
from the same machine.

Running the existing pieces locally (`ccdash hub serve --no-auth --listen
127.0.0.1:8080` + `ccdash hub join --url http://127.0.0.1:8080`) works, but:

- the hub checks no `Host` header, so a malicious page can DNS-rebind a
  name to 127.0.0.1 and become same-origin with the hub — `--no-auth`
  then hands it every hosted PTY (= code execution). `--tailscale-auth`
  has the same hole: after rebinding, the page can set
  `Tailscale-User-Login` itself;
- `--no-auth` is open to every local process;
- it is two processes, a device token and a join file — and it reads as
  "remote access machinery" to anyone reviewing the machine;
- the portal loads xterm.js, marked, DOMPurify and Google Fonts from
  CDNs, i.e. outbound requests on every page load.

## Decision

**`ccdash web`: one process that serves the hub SPA on 127.0.0.1 and
talks to this machine's collector over an in-process tunnel.**

- **Same hub code, a different device link.** The hub keeps its
  `deviceConn` (an `http.Transport` dialing `sess.Open()`, plus the raw
  `sess.Open()` used by `handlePTYSocket`'s pty-raw upgrade). In local
  mode the yamux session runs over `net.Pipe()` instead of a WebSocket:
  the hub end is `yamux.Client`, the collector end `yamux.Server`, served
  by the collector's existing tunnel handler — the same `hubAllowed`
  allowlist and per-feature gates (attach, approvals) as hub mode. Hooks,
  `/shutdown` and settings writes stay unreachable from the browser.
- **Independent of hub mode.** No `hubcfg` join file, no device token,
  no `hubLoop` involvement, and the `hub_enabled` toggle does not apply
  (it governs the outbound tunnel to an external hub; the local link
  never leaves the process). A joined machine can run both.
- **Collector: embed or attach, like the TUI.** If a collector already
  answers on 127.0.0.1:9123, `ccdash web` attaches to it instead of
  fighting for the port: it opens `GET /local/portal` on the collector
  with `X-Ccdash-Token` and an `Upgrade: ccdash-local` handshake, and runs
  the same yamux pair over that loopback connection. The route is behind
  the usual token check and serves only the tunnel allowlist, so it adds
  nothing a token holder couldn't already do. If no collector is running,
  `ccdash web` runs one in-process (hooks keep working, exactly like
  `ccdash server`) and links to it with `net.Pipe`. If an attached
  collector goes away, `ccdash web` starts its own; the portal shows the
  device offline only for that gap.
- **Loopback only.** The portal binds `127.0.0.1:<port>` (default 9124,
  `--port`). There is no `--listen`.
- **Login à la Jupyter.** A random login token lives in
  `$XDG_STATE_HOME/ccdash/web-login` (0600). `ccdash web` prints
  `http://127.0.0.1:9124/auth/local?token=…` and opens it with `open` /
  `xdg-open` (`--no-open` to skip). `/auth/local` checks the token in
  constant time, rotates it (one use per token), and sets a signed
  `ccdash_web` cookie: HttpOnly, SameSite=Strict, 30 days, keyed by the
  cookie secret in the local store so it survives restarts. A visit
  without the cookie gets a page saying to run `ccdash web open`, which
  reads the current token and opens a fresh login URL — so a launchd-run
  portal never needs its log read. The cookie name differs from the
  hub's because cookies ignore ports (a dev hub on 127.0.0.1:8080 would
  otherwise clobber it). No OIDC, no email allowlist; the user shown is
  the OS login name.
- **Host header check against DNS rebinding.** Every request's `Host`
  must be `127.0.0.1`, `localhost` or `[::1]` (any port), else 421. The
  same check guards `ccdash hub serve --no-auth` and `--tailscale-auth`,
  which also accept the public URL's host (`tailscale serve` forwards
  the original Host). The OIDC mode is unchanged: its cookie is useless
  to a rebinding page, and its proxies decide the Host.
- **Cross-origin protection unchanged.** Mutating `/api/` calls keep
  `http.CrossOriginProtection`; the terminal WebSocket keeps
  coder/websocket's same-origin check.
- **No CDN.** xterm.js, its fit / web-links addons, xterm.css, marked and
  DOMPurify are vendored under `internal/hub/web/vendor/` (versions,
  sources and licenses in `vendor/README.md`) and served from the binary
  — for the hub too. Google Fonts goes: the stack already falls back to
  Hiragino / Yu Gothic / Noto Sans JP when installed.
- **Notifications: the existing Web Push.** 127.0.0.1 is a secure
  context, so the service worker, `PushManager.subscribe` and the
  installed-app badge work; the payload is encrypted end to end and
  Chrome delivers it with the tab closed (Chrome itself running). The
  only outbound traffic is the push service request (FCM for Chrome),
  which is what any web app with notifications does. The VAPID `sub`
  claim needs a contact: the public URL when it is https, otherwise the
  project URL `https://github.com/TakumaNakagame/ccdash` (a mailto: of
  a loopback URL is rejected by some push services). Notification clicks
  open `/#/d/<device>/s/<session>` as before.
- **One fixed device.** The local store holds a single device row
  (`id = "local"`, named after the hostname). Device add / rename /
  rotate / delete answer 404 and the tunnel endpoint is not registered;
  the SPA hides the device management UI, the join hint and the logout
  link (`/api/me` reports `local: true`).
- **State next to the collector's.** The hub store (prefs, quick
  commands, VAPID keys, push subscriptions, cookie key) is
  `$XDG_STATE_HOME/ccdash/web/hub.sqlite`, separate from a hub's.
- **Read tokens are off** in local mode (no machine clients on a laptop).
- **Runs in the background** (added later). `ccdash web` starts a detached
  `ccdash web --foreground` (Setsid, stdin `/dev/null`, output to
  `web.log`) and returns; `web.json` (PID, port, collector address and
  whether the collector is embedded or linked) backs `ccdash web
  stop|restart|status`. The child inherits the caller's environment, so
  hosted claude sessions start as before. launchd (parent PID 1) and
  systemd (`INVOCATION_ID`) starts stay in the foreground, so existing
  agents keep working.
- **One meaning for stop and for restart.** A first version restarted the
  embedded collector inside the same process whenever it returned; after
  `POST /shutdown?resume=1` that left the old claudes running (only a
  restart closed them) while the new collector resumed the same sessions —
  two claudes per session. Now the collector closes every hosted PTY
  whenever `ListenAndServe` returns, and `ccdash web` exits when its
  collector is shut down (`errCollectorStopped`); only `POST /api/restart`
  (`ccdash web restart`, the portal's ↻) brings it back, by re-exec.
  `ccdash web stop --resume` is `POST /shutdown?resume=1`: saved, stopped,
  resumed by the next start.

## Alternatives considered

- **Call the collector's handler directly (no yamux).** Simpler for plain
  requests, but the hub's PTY socket and `deviceConn` are built around
  `net.Conn` streams (Hijack for pty-raw). `net.Pipe` + yamux reuses all
  of that unchanged, and one code path is tested for both modes.
- **Serve the SPA from the collector's own port (9123).** Hooks and the
  TUI use 9123 with the shared token; mixing cookie-authenticated browser
  traffic into that listener would mean the collector's whole mux has to
  reason about browsers (CSRF, Host, cookies). A separate listener keeps
  the collector's contract as is.
- **Browser Notification API instead of Web Push.** Needs an open tab and
  a poller; Web Push already exists, works with the tab closed and keeps
  one notification path.
- **A token only on the command line (true Jupyter).** A launchd-started
  portal would need its log read to log in again; the 0600 token file
  plus `ccdash web open` covers that with the same trust boundary as the
  collector token.

## Consequences

- A company Mac can run `ccdash web` (e.g. as a launchd agent) and use the
  portal UI with no inbound exposure and no outbound traffic besides push.
- Anything running as the same OS user can read the login token file —
  the same boundary as the collector token, which already grants more.
- The rebinding fix also hardens `--no-auth` / `--tailscale-auth` hubs.
- MCP session links (`url`) still point only at a joined hub.
