# Local portal vs hub: why they looked different (2026-10-22)

Report: the local portal (`ccdash web`) "lacks" hub features — e.g. the
unread sessions lifted to the top of the list.

## Findings

- **The code is already one source.** `ccdash web` runs `internal/hub`
  with `Config.Local` and serves the same embedded SPA (`web/app.js`,
  `index.html`, `app.css`) as `ccdash hub serve`. Go-side branches on
  `cfg.Local` cover login, device management, the tunnel endpoint and read
  tokens only; the SPA's `me.local` hides device management / logout.
  Device data goes through the same `tunnelHandler` allowlist. No feature
  is implemented twice or gated off for local mode (ADR 0005 holds).
- **Cause 1 — per-browser settings.** 「新着を上に」 (`newFirst`, the 新着
  block) was a localStorage toggle defaulting to on only for a coarse
  pointer (phones). localStorage is per origin, so a choice made on the
  hub never reached `http://127.0.0.1:9124`, and a desktop browser started
  with it off. The feature was there; the setting wasn't.
- **Cause 2 — version gaps.** The local portal serves the SPA embedded in
  the binary that runs it. The machine in question ran v0.9.60 while the
  hub ran a v0.9.61 rc (projects merged across hosts, `/` completion
  anywhere, links from the restart band, …). Also, a `ccdash web` linked
  to a separate collector keeps serving its old SPA after `ccdash update`
  + `ccdash restart` (only the collector restarts), and nothing said so.

Other per-browser (localStorage) state, still per origin: list / task
pane widths, task pane open / host, list fold, preview mode, grid mode /
snap, work-light animation, host filter, new-session host. These are
screen-size or device choices, kept per browser on purpose.

## Options

1. Patch the local portal's defaults only (e.g. turn newFirst on for
   `me.local`). Fixes the symptom; the next per-browser setting diverges
   again, and hub and local behave differently by design.
2. **Keep one SPA and move list-behavior settings to the per-user prefs
   (`/api/prefs`)** that both modes already serve from their own store.
   Every browser of one portal agrees; the default is the same in both
   modes; nothing local-specific is added.

Chosen: 2. Done in this change:

- `prefs.new_first` ("" = on, "on", "off"); `PUT /api/prefs` merges onto
  the stored value so an older cached page can't wipe it. The default is
  on everywhere. A browser's old `ccdash.newFirst` is carried up once.
- `ccdash update` names a running linked `ccdash web` and says to run
  `ccdash web restart`.

Not shared, by design: the hub's and the local portal's stores are
separate (a work machine never talks to the hub — ADR 0005), so a pref set
on one is not seen by the other; both start from the same default.
Keeping the binary current (`ccdash update`, then `ccdash restart` /
`ccdash web restart`) is what keeps the local portal level with the hub.
