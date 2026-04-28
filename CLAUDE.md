# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working in this repository.

## Commands

Local dev:

```sh
go install ./cmd/ccdash      # places ~/go/bin/ccdash with Version="dev"
go build ./...               # any package
go test ./...                # transcript / wrapper / redact have unit tests
go vet ./...                 # CI runs this on every push
```

Run:

```sh
ccdash install-hooks         # one-time: writes HTTP hook entries into ~/.claude/settings.json
ccdash                       # opens the TUI; embeds the collector for the session
ccdash -k                    # spawns a detached collector, then opens the TUI
ccdash --group home-lab        # locks the TUI to a single project / user-named group
ccdash --version             # prints the build-time main.Version
ccdash update                # self-update from the latest GitHub release
```

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test`, `go build` on every push / PR. The `release.yml` workflow fires on `v*` tag pushes and produces 4 binaries (linux/darwin × amd64/arm64) plus sha256 sidecars. Tags can be cut from the GitHub UI via the manual `tag` workflow.

## Architecture

ccdash is a single Go binary that plays three roles depending on how it's invoked:

1. **Collector** — an HTTP server bound to `127.0.0.1:9123` that receives Claude Code's hook events, writes them to SQLite, and (optionally) blocks PermissionRequest hooks waiting for an operator decision.
2. **Discoverer** — a goroutine that polls `~/.claude/sessions/<pid>.json` and `~/.claude/projects/*/<id>.jsonl` every ~10 s to surface idle/stopped sessions even when no hook has fired.
3. **TUI** — a Bubble Tea application that reads the SQLite store, tails transcripts on disk, and dispatches per-session actions (attach, approve, summarize).

The TUI auto-spawns the collector when none is listening, and tears it down on quit. `-k` and `ccdash server` cover persistent-collector setups.

### Top-level layout

```
cmd/ccdash/main.go              entry; main.Version is injected via -ldflags at release time
internal/cli/                   cobra command tree (run, server, install-hooks, update, ...)
internal/server/                HTTP collector + rate limiter + auth middleware + discovery loop
internal/db/                    SQLite layer (sessions / events / approvals / settings)
internal/discovery/             scans ~/.claude/projects/*.jsonl, extracts cwd / first user prompt
internal/procmap/               PID ↔ session_id ↔ tmux pane via ~/.claude/sessions/<pid>.json
internal/transcript/            JSONL parser + LoadTail for fast right-pane previews
internal/summarize/             builds a digest and shells out to `claude -p`
internal/redact/                pattern-based secret masking (AWS / GH PAT / sk- / Bearer / URL creds)
internal/auth/                  loopback shared-token loader (32 B hex at $XDG_STATE_HOME/ccdash/token)
internal/hookcfg/               install-hooks merge logic for ~/.claude/settings.json
internal/settings/              persisted preferences (settings table) + KindBool/KindInt/KindEnum/KindAction spec
internal/selfupdate/            `ccdash update` self-replace via os.Rename onto the running path
internal/tui/                   Bubble Tea UI — keys, layout, transcript pane, settings page
internal/wrapper/               optional `ccdash claude` exec wrapper that adds tmux pane / pid metadata
internal/attach/                pty-based inline `claude --resume` runner with Ctrl+] mid-session detach
internal/paths/                 state dir / db / settings paths (XDG-aware)
internal/gitinfo/               `git -C cwd rev-parse` lookups for repo/branch/commit
internal/model/                 plain data types (Session / Event / Approval) and DisplayTitle
docs/usage_en.md                hands-on usage guide (English)
docs/usage_jp.md                hands-on usage guide (Japanese)
task.md                         original product spec; historical, not authoritative
```

### Data flow

1. Claude Code fires an HTTP hook to `http://127.0.0.1:9123/hooks/<event>`. The headers carry `X-Ccdash-Token` (auth), `X-Ccdash-Managed: true` (idempotency marker), and several `X-Ccdash-*` env-var interpolations.
2. `internal/server` runs the request through rate-limit → token check → handler. Handlers run `redact.JSON` / `redact.String` over the payload before persisting.
3. `internal/db` writes a row in `events`. SessionStart also `UpsertSession`; PermissionRequest also `InsertApproval` (status = pending).
4. The TUI polls SQLite on a ticker (default 1 s) via `internal/db.ListSessions` and `ListPendingApprovals`. The right pane separately tail-reads the selected session's transcript JSONL via `transcript.LoadTail` so updates land within the same tick.
5. PermissionRequest is the one handler that *blocks*. It opens a Go channel, registers it in `Server.pending[id]`, and selects on `<-ch | timeout(25 s) | r.Context().Done()`. The TUI's `decideApproval` POSTs to `/approvals/<id>/decide`, which routes through the channel back to the blocked goroutine, which then writes the appropriate `hookSpecificOutput` JSON so Claude obeys.

### Settings page (`,`)

`internal/settings/AllSpecs()` is the single source of truth for the list of preferences. Adding a setting means:

1. Add the storage field on `settings.Settings` and a default in `Defaults()`.
2. Pick a `Kind` (`KindBool` / `KindInt` / `KindEnum` / `KindAction`) and append a `Spec` to `AllSpecs()`.
3. Add the typed accessor branch in `Get` and `Set`.
4. If the value gates a TUI behavior, read it from `m.settings.Foo` rather than caching on the model — the settings page mutates `m.settings` in-place via `settings.Set`.

The TUI render layer dispatches on `Spec.Kind` automatically; you don't normally need to touch `internal/tui/tui.go` to add a new pref.

### Key invariants and behaviors

- **127.0.0.1 is hardcoded.** No flag, no env var, no opt-out. ccdash is single-host, single-user. `internal/paths.DefaultHost` is the only place that names the bind.
- **Hook entries belong to ccdash via the `X-Ccdash-Managed: true` marker.** `install-hooks` and `uninstall-hooks` use it to round-trip our entries without disturbing the operator's other hooks. Don't touch hook entries that lack the marker.
- **The auth token rotates only when the file is missing.** On first start `auth.LoadOrCreate` writes a fresh 32 B hex token to `$XDG_STATE_HOME/ccdash/token` (mode 0600). `server.syncInstalledHooks()` re-runs `install-hooks` on boot when the token in `settings.json` doesn't match — but only if `auto_install_sync` is on and there are existing hook entries to update.
- **The DB is never reset by code.** `migrate()` is `CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ADD COLUMN` only; ignore `duplicate column` errors. Operators can hand-delete `~/.local/state/ccdash/ccdash.sqlite` to start over.
- **`s.UserGroup` wins over `s.Repo` for grouping.** `groupOf(s)` consults `UserGroup` first, then `Repo`, then `filepath.Base(s.Cwd)`. The tab strip (the UI rendering of groups) dedupes auto-derived entries against user-named ones, so an operator naming a group the same as a repo just merges them.
- **`s.Title` is the auto-derived first user prompt; `s.CustomTitle` is the operator override.** `Session.DisplayTitle()` centralizes the precedence — never rebuild this logic in a render path. The transcript-derived title gets `redact.String` applied before persistence so a prompt containing an API key doesn't show up in the list.
- **`internal/transcript.LoadTail` exists for a reason.** The asmr-palyer transcript in this dev environment is ~30 MB; doing a full `Load` on every tab change made selection feel laggy. Use LoadTail for live-tail surfaces; reserve full `Load` for the modal viewer (`o`).
- **`claude -p` spawns are isolated.** `summarize.Run` invokes claude with `--setting-sources project` and cwd `/tmp` so the summarize subprocess does NOT inherit our hooks (otherwise it would loop back to our collector and pollute the session list). The summary prompt is also prefixed with `[ccdash:summary]` and `discovery.Scan` skips any transcript whose first user prompt starts with that marker.
- **Approvals fall back from `tool_use_id` to oldest-pending.** PermissionRequest payloads don't include a `tool_use_id`, so PostToolUse handlers call `ResolveOldestPendingForTool` after `ResolvePendingByToolUseID`. The discovery loop also sweeps anything pending > 45 s into `timeout`. Don't rely on tool_use_id alone.
- **Secure-mode toggles are explicit.** `approve_enabled`, `summary_enabled`, `attach_enabled`, `auto_install_sync` each turn off one risk-bearing capability; the "Apply secure preset" action flips all four. The TUI keys for these features check the flag and flash "<feature> is OFF" when disabled — don't bypass.
- **Mouse wheel zoning is layout-aware.** `mouseInRightPane` re-derives geometry per-event because vertical layout splits Y instead of X. Auto-vertical decides via `m.width < settings.VerticalAutoCols`.
- **`groupLocked` (from `--group`) hides the strip and disables h/l/Tab/Shift+Tab.** It also keeps `archiveCurrentGroup` from auto-advancing past the locked group. The deprecated `--tab` alias is still accepted (hidden) so existing scripts don't break.
- **Inline attach uses `internal/attach`, not `tea.ExecProcess`.** For stopped sessions, `attachCurrent` builds an `attach.Command` and hands it to `tea.Exec`. Bubble Tea releases the alt-screen and calls `Run`, which puts the operator's terminal in raw mode, opens a PTY for `claude --resume <id>`, and relays I/O. `Ctrl+]` (`attach.EscapeByte`) is intercepted in the stdin pump — on hit we send SIGTERM, drain output, and return `Result{Detached: true}` so the operator can leave a still-running claude session and bounce back to the dashboard. tmux-pane attach (running session) is unchanged and still uses `tea.ExecProcess` with `tmux switch-client`.

### Common gotchas

- **East Asian Width.** The terminal box-drawing `─` is "ambiguous" — runewidth says 2 cols on CJK locales, but many terminals draw it as 1. The vertical-layout body separator uses ASCII `-` for that reason; the header rule still uses `─` because it's followed by a known-width header bar. If you add a new full-width rule, mirror the ASCII-fallback pattern.
- **lipgloss column widths and styled strings.** `fmt.Sprintf("%-18s", styledStr)` doesn't work — `%-18s` counts ANSI escape bytes too. Pad first, style after; or use `runewidth.FillRight` against the unstyled version.
- **Bubble Tea + writes to stderr.** During alt-screen rendering, writes to stderr corrupt the buffer. The collector's `log.Printf` output is redirected via `cli.redirectLog()` to `~/.local/state/ccdash/ccdash.log` whenever the TUI embeds it. Don't add stray `fmt.Fprintln(os.Stderr, ...)` calls inside the TUI's lifetime.
- **ALTER TABLE migration ordering.** Append new ALTERs to the slice in `db.migrate`. Don't re-order the existing ones — every existing user has a partially-migrated DB already and the order matters for legacy fields like `layout_vertical` / `layout_auto` whose values are folded into `layout_mode` on Load.
- **macOS BSD sed.** `install.sh` runs on user shells; it must work on BSD sed (no `\+`, no `-i ''` quirks). Test with `sh install.sh` and ideally also against macOS BSD utils before shipping.
- **GitHub API rate limit (60/hr unauth).** `selfupdate` and `install.sh` annotate 403 responses with the rate-limit hint. The escape hatch is `CCDASH_VERSION=v0.1.x` for the install script.
- **Unsupported OS / arch.** Only linux/darwin × amd64/arm64 are released. Windows is not on the roadmap.
