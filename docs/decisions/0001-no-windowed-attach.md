# 0001 — Attach stays fullscreen-only

- Date: 2026-05-01
- Status: Accepted (retired in v0.3.7)

## Context

Attach in v0.2.x suspended Bubble Tea via `tea.Exec` and handed the whole
terminal to `claude --resume`. The dashboard came back when the operator
hit Ctrl+D or claude exited. Simple and reliable, but it meant the
operator couldn't glance at the session list while talking to claude
without doing a full detach / re-attach cycle.

v0.3.0 introduced a **windowed attach** mode: ccdash kept Bubble Tea
running, spawned claude in a PTY it owned, fed the bytes to a
[`hinshun/vt10x`][vt10x] terminal emulator, and rendered the emulator's
cell grid into the right pane via lipgloss. Keystrokes were translated
back into bytes (`keyMsgToBytes`) and forwarded to the PTY; Ctrl+F
toggled to the v0.2.x fullscreen path and back. Demoed nicely.

It shipped as the default until v0.3.5 / v0.3.6 turned it into an opt-in
toggle. v0.3.7 removes the code path entirely.

## Why it's gone

Two problems showed up under real Japanese input that we couldn't get
out of in the existing architecture.

### 1. vt10x doesn't track display width

vt10x stores one rune per grid cell and advances its cursor by one
column on every put, regardless of whether the character is wide. claude
(Ink + React) advances its own cursor by `runewidth.RuneWidth(rune)`,
so for any CJK / emoji / box-drawing wide char its model and the
emulator's drift apart by one column. Subsequent claude writes that use
absolute positioning land in vt10x cells that don't match where claude
thinks they are; render-time runs through the cell grid in order and
gets the right characters at the wrong columns.

Symptom: `これって    どんな` with extra spaces between words because
the cells claude expected to overwrite were never reached.

A render-side fix that pads to visual width hides the auto-wrap symptom
but makes the column drift more visible — wide chars drift further the
more the operator types. We tried it in v0.3.1 and reverted in v0.3.5.

The **proper fix would require a vt10x fork** with width-aware advance
and continuation-cell marking. ~1 day of work, but the second problem
makes it not worth the effort:

### 2. Bubble Tea's render races the OS-level IME overlay

The IME (macOS / Windows / fcitx) composes text by drawing a pre-edit
overlay at the cursor position, outside the terminal application's
buffer. The operator types `korette`; the IME shows `これって` in an
underlined / floating overlay; on confirm the IME flushes the final
bytes to the terminal app.

In ccdash's windowed attach:

- Bubble Tea owns the alt-screen and re-renders the right pane on every
  `animTickMsg` (~150 ms) and every `attachOutputMsg`.
- Each re-render writes a fresh frame of the vt10x cell grid to stdout.
- That overwrites whatever the IME drew, then the IME re-draws, then
  ccdash re-renders, ad infinitum.

Symptom: phantom spaces and visual jitter while composing Japanese,
which clears up only after confirm.

This is **architectural**, not a vt10x bug. Any windowed embedding of a
terminal child runs into it: the OS draws the IME at the cursor; we
overwrite the cursor area to refresh our render. There's no Linux /
macOS API to ask "is the IME composing right now, please pause?".

## What we kept

Fullscreen attach (`tea.Exec` + raw-mode PTY pump + Ctrl+D detach)
handles both cases natively:

- Bubble Tea is suspended, so the host terminal — which understands its
  own width tables and IME — talks directly to claude.
- The persistent `attach.Session` map in the TUI still holds claude
  alive across detach / re-attach so Enter on the row resumes the same
  child (no divergent `claude --resume` fork).

The `Ctrl+L` nudge on re-attach (added in v0.3.6) stays — claude
otherwise sits idle on a freshly-restored alt-screen because Bubble Tea
released it on the prior detach.

## If you want to bring windowed attach back

The blocker isn't the implementation, it's the IME race. Realistic
paths:

1. **Don't re-render while the operator is typing.** Detect "operator
   was typing N ms ago" and pause the right-pane redraw window. Hairy:
   IME composition can take seconds, and we'd lose live claude updates
   in the meantime.
2. **Move the cursor outside the right pane while composing.** If the
   IME draws at the global cursor position, we could redirect the
   cursor to a "scratch" area while still rendering claude. Requires
   precise cursor-position management and doesn't fix the wide-char
   issue.
3. **Skip the in-terminal embed entirely** — attach as a separate
   window managed by tmux / WezTerm / iTerm2 split panes. ccdash would
   need to script the host terminal which is platform-specific.

Until one of those proves out, keep attach fullscreen.

[vt10x]: https://github.com/hinshun/vt10x
