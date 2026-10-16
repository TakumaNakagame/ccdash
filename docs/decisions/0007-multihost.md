# 0007 — Multi-host: sessions across collectors, host picked per session

- Date: 2026-10-15
- Status: Accepted

## Context

The hub (0003) already reaches every joined machine, but both of its
clients were single-host in shape. The portal made you pick a device
first and then worked inside it (`#/d/<dev>`: list, new session); only the
board and the grid looked across devices. `ccdash mcp` talked to one
`store.Store` — this machine's collector — so Claude could start or
organize sessions only where it ran itself.

We want the host to be an attribute of a session: one list of every
host's sessions, the host chosen when a session is started (host, then a
directory on it), and the same through MCP (`start_session` on host X,
list / stop / resume / retitle a session on host Y).

## Decision

- **Route through the hub, not machine-to-machine.** Collectors bind
  loopback and dial out to the hub; nothing else can reach them. A tool
  call for another host goes `store.NewHubPeer(<hub>/api/d/<id>, token)`
  → the hub's device proxy → the device's own hub allowlist
  (`hubAllowed`, with its `attach_enabled` gate). No collector change, no
  new port, and the device-side boundary stays the one that already
  protects it from the portal.
- **Credential: the caller's own device token, behind a per-device
  switch.** `devices.peer_access` in hub.sqlite: "" (off, the default),
  `read`, `control`; set by a logged-in operator in the portal (端末 → 管理
  → 他の端末へのアクセス, `POST /api/devices/{id}/peer`). A bearer token
  that starts with `ccdh_` is a device token (`apiAuth` → `servePeer`). No
  second token to hand out; turning the switch off cuts it at the next
  request. The trade-off is explicit in the UI: a device with `control`
  that gets compromised can start claude on the others.
- **Peers get a narrow surface** (`internal/hub/peer.go`,
  `peerDeviceRoutes`): `GET /api/devices`, `GET /api/peer/self` (who am I —
  devices don't otherwise know their hub id), and on a device: sessions,
  a transcript tail, approvals (read), projects, usage, restart state,
  `/pty` list, `/hub/info`, `/hub/dirs`; with `control` also the session
  edits, `/pty/start`, `DELETE /pty/{key}` and the (scheduled) restart.
  Never device management, push, prefs, the raw terminal, typing into a
  PTY, deciding approvals, settings. `read` is GET-only.
- **Hosts are named by the hub device name** (case-insensitive; the id
  works too; `local` / `self` = this machine). Unknown → error listing the
  names; offline → error with last seen.
- **Defaults keep old calls working.** Read tools (`list_sessions`,
  `overview`, `list_projects`) without `host` cover this machine plus
  every online peer, rows carry `host`, offline / failing hosts are listed
  instead of failing the call. Acting tools (`start_session`,
  `schedule_restart`, the project tools, `archive_finished`) default to
  this machine. Session numbers are per host: a bare `#N` stays this
  machine's; another host's is `<host>#N` (that is what `ref` shows), a
  session id is searched everywhere. `set_status` stays local; tasks
  joined later (addendum below).
  Without a hub, with peer access off, or under `-r`, nothing changes.
- **Portal:** `#/s` is one list of every online host's sessions (host chip
  on each row; host filter chips above; `#/d/<dev>` = the same list
  filtered, which also shows that host's status / usage / settings /
  restart). Selections, project bands and approvals are per host inside
  it. 新規セッション asks for the host first (default: the project's host,
  else the last used) and then lists directories on it.
- **TUI stays single-host.** It is the machine's own console.

## Consequences

- Only the hub needs updating for the portal and the peer route; devices
  need the new binary only where `ccdash mcp` should reach other hosts
  (MCP servers are spawned per claude session, so no collector restart).
- The Android app got the cross-host list and host picker in its own
  repo (ccdash-android #6), and the task list later (addendum below).

## Addendum (2026-10-15): host letters

Session numbers are per collector, so "#22" alone is ambiguous once lists
mix hosts. Each hub device now has a letter (A, B, … in registration
order; editable in the portal's device menu) and a session's ref is
letter + number — "A231", "B22" — everywhere: TUI, portal, MCP, push
notifications. The hub sends the letter on the tunnel dial; the collector
keeps it in hub.json and stamps it on listed rows. MCP accepts "B22"
(the host is found by letter), still takes "#N" / N for this machine and
"<host>#N". A machine not joined to a hub keeps "#N".

## Addendum (2026-10-16): tasks across hosts

Each machine still keeps its own list (0006), but sessions moved between
machines (the PC → LXC move) and kept working on tasks that live on the
first one. Now:

- **Peers reach tasks.** `peerDeviceRoutes` adds `GET /api/tasks[/{id}]`
  (read) and, with `control`, `POST /api/tasks`, `/api/tasks/order`,
  `/api/tasks/<digits>` and `/api/tasks/<digits>/notes` — never `sync` or
  `import`, and writes only by numeric id.
- **A task's ref is `<letter>#<id>`** ("B#11"): the host letter, then the
  task's usual "#11", so it never collides with a session ref ("B11").
  MCP also takes `<host>#11` and a `host` argument.
- **MCP:** `list_tasks` without `host` merges every reachable host's list
  (bucket by bucket; rows carry `ref` and `host`). A bare "#11" or key is
  this machine's, or — when this machine has none — the one other host
  that has it (several → an error listing their refs). Edits go to the
  task's host; the calling session is linked whichever host it runs on
  (links are session ids), and session rows list their tasks as refs from
  every host. `add_task` takes `host` (default this machine);
  `task_context` and `sync_tasks` stay this machine's.
- **Portal:** rows show the ref with the letter and "について、" copies it.
  The side pane lists every online device's tasks on every page — inside
  a device's page it used to show only that device's, which hid the tasks
  a session there worked on elsewhere; host chips (kept per browser)
  narrow it instead. The Android app
  gets the same list (ccdash-android).
- **The TUI stays single-host** and keeps "#11".

## Addendum (2026-10-16): projects merge, hosts move out of the way

The list still read as "per host": project bands split by host ("web ·
hostA", "web · hostB"), the grid grouped the same way, and host chips sat
above the list. Now:

- **A project name is one group across hosts** in the portal's list and
  grid. Storage does not change — each host keeps its own `projects` row
  and members — the portal merges by name: the block's order is the lowest
  of the hosts' orders, ▲▼ moves it on every host that has it, and
  the color shown is the one of the host with the first letter (`unifyProjectColors`); changing the
  color or joining another host's project copies the color to the hosts
  that have it (`shareProjectColor`, never creating the project elsewhere).
  The project picker lists every online host's projects, and a selection
  across hosts can go into one project. A band's ＋ starts on the host its
  most recent session used; the new-session dialog keeps the project when
  another host is picked.
- **The host filter is a setting** (⚙ 表示 → セッション一覧に出すホスト),
  not chips on the list; `#/d/<dev>` narrows only that page and no longer
  changes the saved choice. A chat's back button returns to the list it
  came from instead of the host's page.
- The TUI and MCP stay per host.
