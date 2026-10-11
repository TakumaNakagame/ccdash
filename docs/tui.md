# TUI reference

Every key is also listed in the app: press `?`.

## Sessions view (default)

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | move selection |
| `g` / `G` | jump to top / bottom |
| `tab` / `shift+tab` | cycle project / user-named groups |
| `R` | toggle "auto repo tabs" in the cycle |
| `T` | edit the user-named group for this session |
| `t` | rename the session (operator override of auto / generated title) |
| `f` | pin / unpin (pins stay on top — inside a project, on top of its block — latest pin first; activity never moves them) |
| `L` | watch later on / off: marked sessions gather in their own block at the very top until cleared |
| `c` / `C` | next color / re-roll for the session's bar (shared with the portal). Running sessions get a stored color that stands apart from the other running ones; `C` re-rolls by the same rule |
| `ctrl+r` | restart the session's ccdash-hosted claude (kill + `claude --resume`), after `y` |
| `!` | show only sessions that need you (`?`: approval, question, menu on screen) or finished a turn you haven't looked at (`✓`) |
| `p` | put the session in a project: existing projects as colored candidates (`↑` `↓` to pick), a new name creates one, `✕ remove from …` takes it out |
| `n` on a project member | the directory picker starts in the project's directory (its latest session's cwd) and the new session joins the project |
| `[` / `]` | move the selected session's project block up / down (activity never reorders projects) |
| `P` | re-roll the project's color (kept apart from other projects' colors) |
| `x` | archive / unarchive |
| `X` | toggle archive view (operator-archived sessions) |
| `o` | full-screen transcript viewer |
| `ctrl+t` | generate short titles with `claude -p` — `y` for the selected session, `a` for the recent batch in one call |
| `n` | start a new claude session (directory picker) |
| `S` | run a skill / slash command in a new session |
| `<` / `>` | shrink / grow the session list by 5% (10–90%, saved as "Session list size") |
| `enter` | attach to the session (tmux switch / `claude --resume`) |
| `a` / `A` / `d` | allow / keep-allow / deny the oldest pending approval |
| `Shift+J` `Shift+K` | scroll the right pane one line at a time |
| `pgup` / `pgdn` | half-page scroll on the right pane |
| `/` | search across session metadata |
| `,` | open the settings page |
| `r` | force refresh |
| `?` | help window listing every key (`j` / `k` scroll) |
| `q` / `ctrl+c` | quit |

## Transcript modal (`o`)

| Key | Action |
| --- | --- |
| `↑` `↓` | scroll one line |
| `pgup` `pgdn` / `ctrl+u` `ctrl+d` / space | half-page |
| `g` / `G` | top / bottom |
| `r` | reload from disk |
| `esc` / `q` / `tab` | back to sessions |

## Settings page (`,`)

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | navigate rows |
| `space` / `enter` | toggle bool, cycle enum, edit int, run action |
| `esc` / `q` / `,` | back to sessions |

## Attach behaviour (`enter`)

ccdash discovers running sessions from `~/.claude/sessions/<pid>.json` and
its on-disk transcripts. Pressing `enter` does:

| State | Action |
| --- | --- |
| Running, in a tmux pane | `tmux switch-client -t <pane>` |
| Running, no tmux pane detected | flash with PID + TTY so you can switch terminals manually |
| Stopped | `claude --resume <session_id>` from the session's cwd |

When a session has pending permission requests its row tints yellow and
the header shows a `⚠ pending: N` badge. The terminal bell rings once
when the total pending count crosses 0 → 1.
