# Portal design rules (hub web / installed app)

The portal (`internal/hub/web`) is an operator's console for Claude Code
sessions, used for hours at a desk and in short bursts on a phone. Read this
before changing `app.css` or adding UI in `app.js` — it exists because UI
written by AI drifts to the same generic look (purple gradients, identical
rounded cards, emoji as icons, ALL-CAPS eyebrows, colour used as wallpaper)
unless the choices are written down and followed.

## Direction

A quiet tool, not a landing page. Neutral surfaces, text in two weights, one
warm accent (the brand orange) for "act here", status colours only for
status. Density closer to a terminal / issue tracker than a SaaS dashboard.
The one indulgent element is the **working beam** (the gradient comet round
a session that is mid-turn: its list row, board card, grid tile and the chat
header) — it carries real information, so it is allowed to be loud. Nothing
else is.

## Tokens (`:root` in app.css)

Use the variables; don't write raw values for these in new rules.

| Group | Tokens | Use |
|---|---|---|
| Surface | `--bg`, `--panel`, `--chip`, `--line` | page, cards/rows, quiet fills (hover, code, tags), hairlines |
| Text | `--text`, `--muted` | body / secondary. No third grey. |
| Accent | `--accent`, `--accent-text` | primary button, selection, links in chat, the one thing to press. The user picks it (表示 sheet → `/api/prefs`); never hard-code the orange — derive tints with `color-mix` from `--accent` |
| Status | `--ok`, `--warn`, `--bad` | done / needs you / error — never decoration |
| Type | `--fs-xs` 12, `--fs-sm` 13, `--fs-md` 14 (body), `--fs-lg` 16, `--fs-xl` 20 | nothing else; weights 400 / 600 / 700 |
| Radius | `--r-sm` 6 (buttons, inputs, tags), `--r-md` 8 (cards, rows groups, dialogs' inner), `--r-lg` 12 (dialogs, tiles) | `999px` only for real pills (tabs) |
| Fonts | `--sans` (Japanese faces first), `--mono` | mono only for code, paths, commands, numbers in tables |

Japanese body text gets `line-height: 1.6`; UI chrome (rows, buttons) 1.4.

## Rules

- **Icons are SVG from `icon()` in app.js** (stroke icons after Lucide, ISC).
  Never use emoji or arrow glyphs as icons (✨ 🔔 📌 ⏰ ▦ ↻ ↗ ⋯ …). An icon
  without visible text needs a `title` / `aria-label`.
- **Colour encodes one thing each.** Session colour = a small dot before the
  title. Project colour = a 3px gutter on its rows and the dot in its band.
  Status = `--ok/--warn/--bad`. No tinted row backgrounds, no full-colour
  bands, no two stripes side by side. The one exception is **unread (NEW)**:
  a session that moved since you last looked gets an accent-tinted ground and
  ring on its list row and grid tile (a 要対応 tile is the same in `--warn`),
  flashing once as it turns unread — the operator asked to see at a glance
  which sessions replied. On a phone the list also lifts unread rows into a
  新着 block at the top (「新着を上に」).
- **Status is words, in Japanese.** 作業中 / 待機中 / 停止 / 要対応 / 未確認 —
  not `active` / `idle` / `stopped` pills. A dot + muted text; only 要対応 and
  NEW may be filled, because they ask for action.
- **No eyebrows.** No small tracked uppercase labels above things ("PROJECT",
  section titles in caps). Headings are sentence-case Japanese in `--fs-sm`
  600 `--muted`.
- **One card style.** Bordered `--panel` on `--bg`, `--r-md`, no shadow.
  Shadows only on things that float (dialogs, a grid tile being dragged, toast).
- **Touch:** on `(pointer: coarse)` every control is at least 40px high
  (44px for icon-only buttons); keep 8px between neighbours. Respect
  `env(safe-area-inset-*)` at the top bar and the composer.
- **Focus is visible:** `:focus-visible` gets the accent outline; never
  remove outlines without a replacement.
- **Motion** only for state: the working beam, typing dots, title spinner.
  Everything animated stops under `prefers-reduced-motion` and with ◌ / ✨
  (the header beam toggle).
- **Copy:** verbs on buttons (送信 / 再開 / 中断), errors say what to do,
  empty states say what to do next. No "Submit", no apologies.

## App shell (phone / tablet, < 1024px)

Platform-neutral "modern app", not iOS- or Material-specific.

- **Nav bar**: top-level pages show the brand; detail pages (chat, terminal,
  diff) use their own bar as the nav bar — back chevron, title, actions — and
  hide the header. Bars are translucent (`backdrop-filter`) and sticky.
- **Bottom tab bar** (`#tabbar`: ホーム / セッション / グリッド) on top-level
  pages only; hidden in detail views. `paintShell` keeps it in sync.
- **Navigation** slides: push from the right, pop back (`navigate()`, View
  Transitions); swipe right from the edge goes back. Never both at once.
- **Dialogs are bottom sheets** with a grabber: drag down or tap outside to
  close.
- **Lists run edge to edge** with inset separators; pressed states instead
  of hover; a long press enters edit mode (row checkboxes), where taps
  toggle selection.
- **Haptics** (`haptic()`): a short tick for actions that change something —
  send, keys sent to claude, tab switch. Nothing for plain navigation.

Desktop (≥ 1024px) keeps the two-pane layout with breadcrumbs.

## Checking a change

Run a local hub (docs/hub.md: `ccdash hub serve --no-auth` + a device
collector in a temp `HOME`) and look at it at 390px and 1280px, light and
dark, before shipping. Screenshots, not imagination.
