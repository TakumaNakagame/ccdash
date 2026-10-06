// Package screenprompt recognizes a Claude Code dialog waiting for the
// operator on an emulated terminal screen (rows of plain text, as served by
// the collector's GET /pty/{key}/text). It is the Go twin of screenAsk /
// screenPrompt in internal/hub/web/app.js — change both together.
package screenprompt

import (
	"regexp"
	"strings"
)

var (
	numberedCursor = regexp.MustCompile(`^\s*❯\s*\d{1,2}[.)]\s`)
	askTabs        = regexp.MustCompile(`[☐☒]\s*\S`)
)

// Kind spots what the portal's chat shows as cards (see
// screenAsk / screenPrompt in web/app.js): Claude Code's AskUserQuestion
// dialog ("question") or a numbered confirmation menu — permission, plan
// approval, folder trust ("confirm"). text is the question line.
func Kind(rows []string) (kind, text string) {
	all := strings.Join(rows, "\n")
	if strings.Contains(all, "Enter to select") && strings.Contains(all, "Chat about this") {
		for i, r := range rows {
			if askTabs.MatchString(r) {
				for _, q := range rows[i+1:] {
					if t := strings.TrimSpace(q); t != "" {
						return "question", t
					}
				}
			}
		}
		return "question", ""
	}
	unbox := func(r string) string { return strings.TrimSpace(strings.Trim(strings.TrimSpace(r), "│|")) }
	for i := len(rows) - 1; i >= 0; i-- {
		if !numberedCursor.MatchString(unbox(rows[i])) {
			continue
		}
		for j := i - 1; j >= 0 && j >= i-12; j-- {
			t := unbox(rows[j])
			if strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") {
				return "confirm", t
			}
		}
		return "confirm", ""
	}
	// Unnumbered list (the folder-trust question, the theme picker): the
	// cursor row has siblings in the column after the "❯". A lone "❯" row
	// is claude's input prompt, not a menu.
	// Only the bottom-most "❯" row counts: higher ones are past prompts in
	// the scrollback, and an empty one is the input box (no dialog open).
	for i := len(rows) - 1; i >= 0; i-- {
		r := unbox(rows[i])
		col := strings.Index(r, "❯")
		if col < 0 {
			continue
		}
		if strings.TrimSpace(r[col+len("❯"):]) == "" {
			return "", ""
		}
		raw := rows[i]
		c := strings.Index(raw, "❯")
		sib := func(l string) bool {
			return len(l) > c+len("❯") && strings.TrimSpace(l[:c]) == "" && strings.TrimSpace(l[c:]) != "" && !strings.Contains(l, "─")
		}
		n := 0
		for j := i - 1; j >= 0 && sib(rows[j]); j-- {
			n++
		}
		for j := i + 1; j < len(rows) && sib(rows[j]); j++ {
			n++
		}
		if n == 0 {
			return "", ""
		}
		for j := i - 1; j >= 0 && j >= i-12; j-- {
			if t := unbox(rows[j]); strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") || strings.Contains(t, "?") {
				return "confirm", t
			}
		}
		return "confirm", ""
	}
	return "", ""
}
