# 0006 — Tasks: a to-do list kept by the collector, edited mostly by Claude

- Date: 2026-10-13
- Status: Accepted

## Context

The operator keeps a to-do list outside ccdash (a personal tool: a
`tasks.json` file, a Python CLI `task.py`, a merge script
`sync_tasks.py`, a context dump `timeline_context.py` and a small WebUI).
Claude adds and updates almost all of it: interactive sessions record
progress ("#11 について" → `task.py note 11 …`), and two launchd jobs run
`claude -p` skills that read Slack / Jira / GitHub through their MCP
servers and pipe a JSON proposal into `sync_tasks.py`. The person mostly
reorders, ticks off and re-plans.

The list and the sessions are two views of the same work, and ccdash
already runs on every machine where that work happens. What is wanted:

- tasks inside ccdash, edited by Claude through ccdash's MCP server;
- writes to Jira / Slack / GitHub stay with Claude and those services'
  MCP servers — ccdash holds links, never API clients;
- a task pane beside the sessions (list · main · tasks), a collapsible
  session list, and the task pane in the grid too;
- with several machines, one "to do" list on the portal's home page.

## Decision

### Where tasks live

**In the collector's SQLite, per machine** — like `tasks.json` lives on
the Mac today. The work Mac runs `ccdash web` (0005) and never joins the
home hub, so a hub-side store is not an option; per-device data also
keeps `ccdash mcp`, `-r` and the CLI working through `store.Store`
unchanged. The hub aggregates (below).

Tables (`db.migrate`, CREATE + ALTER only as usual):

- `tasks` — `id INTEGER PRIMARY KEY AUTOINCREMENT` (ids are never reused:
  "#11" must not come to mean something else), `key` (unique when set:
  Jira key, PR URL, `misc:…`; how automated runs recognise a task),
  `title`, `priority` 1–4, `status`, `plan` / `plan_by` / `plan_on`,
  `due`, `project`, `note`, `links` (JSON array), `source`,
  `created_on`, `done_on`, `ord`, `archived_at`, `updated_at`.
- `task_events` — the timeline: `task_id`, `at`, `source`, `text`,
  `link`, `ref`, unique on `(task_id, ref)` so the same Slack message is
  never appended twice.
- `task_sessions` — `(task_id, session_id)`: which Claude sessions worked
  on a task. Shown as `#11` on the session row and as links on the task.

A `tasks_rev` counter (settings table) is bumped in the same transaction
as every change, so the TUI and the portal can poll cheaply.

### Rules (package `internal/tasks`, pure functions with tests)

- **Status** is `todo` / `doing` / `waiting` / `done` / `cancelled`.
  There is no delete: a task can be **archived** only when it is `done` or
  `cancelled`, and unarchived again. No automatic archiving.
- **Priority** (how much it matters) and **plan** (the day to work on it)
  are separate. The list is bucketed by plan: today / tomorrow / this
  week / next week / later / unplanned; a past plan that isn't finished
  stays in today as "carried over from M/D". Tomorrow is the next weekday
  (no holidays); "this week" is this Friday, "next week" next Monday.
  Within a bucket: manual order (`ord`), plan date, priority, due, id.
  A `done` / `cancelled` task stays in today on the day it was finished,
  then moves to a "finished" section until archived.
- **Dates are the machine's local time.**
- **The person's decisions win** (the merge rules of `sync_tasks.py`,
  kept as is): an automated sync never changes title, priority or note;
  a plan set by the person today (`plan_by = manual`, `plan_on = today`)
  is not moved; a replan needs a reason and is logged in the timeline;
  a status other than done changes only when the same sync brings new
  timeline entries; a finished (done / cancelled) task is never reopened.
- Status and plan changes always leave a timeline entry.
- Buckets and headings are computed server-side and returned by the API,
  so the portal doesn't carry a JS copy of the date rules.

### Projects

`tasks.project` is a ccdash project name. Setting one creates the project
like `SetProject` does; `RenameProject` (and its merge) updates tasks too;
`ListProjects` reports each project's open-task count and keeps listing
projects that only have tasks. A task's `n` / `start_session` opens in the
project's directory and puts the session into the project.

### Store, API, CLI

`store.Store` gains `ListTasks`, `GetTask`, `AddTask`, `UpdateTask`
(a patch: fields, status + why, plan + why, links, sessions, archived),
`AddTaskNote`, `OrderTasks`, `SyncTasks` and `ImportTasks`. Local writes
SQLite directly, so the launchd jobs work without a running collector.

Collector routes: `GET /api/tasks`, `GET /api/tasks/{id}`,
`POST /api/tasks`, `POST /api/tasks/{id}`, `POST /api/tasks/{id}/notes`,
`POST /api/tasks/order`, `POST /api/tasks/sync`, `POST /api/tasks/import`.
The hub allowlist takes the reads and the person-sized edits — not
`sync` or `import`.

`ccdash tasks show|list|find|note|status|link|plan|archive` replace
`task.py`; `ccdash tasks sync < proposal.json` replaces `sync_tasks.py`
(same input JSON); `ccdash tasks context` replaces `timeline_context.py`
(same output); `ccdash tasks import <tasks.json>` is the one-off
migration; `ccdash tasks export` writes a `tasks.json`-shaped file back
out; `ccdash tasks md` prints the old TASKS.md on demand (nothing reads
the auto-generated file today, so it isn't written any more).

### MCP

Read: `list_tasks`, `get_task`, `task_context`. Write (dropped by
`--read-only`): `add_task`, `update_task`, `set_task_status`,
`plan_task`, `add_task_note`, `link_task` (URL, path or session),
`order_tasks`, `archive_task`, `sync_tasks`. `get_session` /
`list_sessions` report the tasks a session is linked to.

### UI (later phases)

- **TUI**: horizontal layout becomes list · main · tasks. `b` collapses
  the session list (selection still moves with j/k), `w` shows / hides the
  task pane, `W` moves focus into it (space done, 1–4 / 0 plan, `[` `]`
  order, enter detail, `y` copy "#N について、", `n` new session for the
  task). The pane width is a setting. The grid (`V`) keeps the task pane
  when shown and re-tiles in the remaining width. In vertical layout the
  task pane takes the list's place. All column geometry goes through one
  function so the IME cursor stays on the drawn cell (0002).
- **Portal**: on wide screens list · main · tasks with a collapsible list
  and a header toggle for tasks (remembered per browser); the grid page
  gets the same pane; the preview sidebar covers the task pane while open;
  narrow screens get a "タスク" tab.
- **Hub home**: the device poll (`watchDevice`) refetches a device's tasks
  when its `tasks_rev` changes; the home page shows every device's open
  tasks bucketed together, each with a device badge ("mac #11"); edits go
  to the owning device.

## Alternatives considered

- **Keep `tasks.json` as the source of truth** and have ccdash read and
  write the file. Two writers, file watching, a whole-document rev lock
  and no way through the hub tunnel without new plumbing.
- **Store tasks on the hub.** One list for every machine, but the work
  Mac never talks to the hub, and MCP / CLI on a device would depend on a
  remote service.
- **Jira / Slack integrations in ccdash.** Claude already has those MCP
  servers, with the operator's own permissions and judgement in the loop;
  ccdash would need tokens and would duplicate them.
- **Hard delete.** A task that silently disappears breaks "#11" and the
  history; archive covers "get it out of the way".

## Consequences

- Task ids are per machine: "#11" is unambiguous inside a session, but
  the hub's combined list has to show the device next to the id.
- Callers of the old scripts move to `ccdash tasks …` / MCP: the
  operator's CLAUDE.md, four skills and two launchd scripts.
- Task text (titles, Slack-derived notes) becomes reachable through the
  hub like transcripts already are; a device that never joins a hub (the
  work Mac) is unaffected.
- The merge rules now live in Go; changing them means changing
  `internal/tasks` and its tests, not a script.
