package tui

// ctrl+t: generate short titles with claude -p, for the selected session
// alone or for the "recent" batch in one claude call (see
// summarize.KickoffTitles). Generated titles sit between the operator's
// rename (t) and the first prompt in Session.DisplayTitle.

import (
	"context"
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/summarize"
)

// titleRecentWindow is how far back the "recent" batch looks.
const titleRecentWindow = 24 * time.Hour

// recentTitleCandidates is the "recent" batch: sessions in the current view
// (tab / search / account filter) active within titleRecentWindow that
// have no rename and either no generated title or activity since it was
// generated — newest first, at most summarize.MaxTitleBatch.
func (m *model) recentTitleCandidates(now time.Time) []mdl.Session {
	var out []mdl.Session
	for _, s := range m.sessions {
		if !titleable(s) || s.CustomTitle != "" {
			continue
		}
		if now.Sub(s.LastSeen) > titleRecentWindow {
			continue
		}
		if s.GenTitle != "" && !s.LastSeen.After(s.GenTitleAt) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if len(out) > summarize.MaxTitleBatch {
		out = out[:summarize.MaxTitleBatch]
	}
	return out
}

// titleable: a real row (not a spawn placeholder) with a transcript and no
// generation already in flight.
func titleable(s mdl.Session) bool {
	return s.Num > 0 && s.TranscriptPath != "" && s.TitleStatus != "running"
}

// startTitleGen opens the ctrl+t choice banner: y = selected, a = recent.
func (m *model) startTitleGen() {
	if !m.settings.SummaryEnabled {
		m.flash = "summarize is OFF (settings ',') — title generation uses it too"
		return
	}
	var sel *mdl.Session
	if len(m.sessions) > 0 && titleable(m.sessions[m.selSess]) {
		s := m.sessions[m.selSess]
		sel = &s
	}
	recent := m.recentTitleCandidates(time.Now())
	if sel == nil && len(recent) == 0 {
		m.flash = "nothing to title (no transcript yet, or already generating)"
		return
	}
	m.titleGenSel = ""
	if sel != nil {
		m.titleGenSel = sel.SessionID
	}
	m.titleGenRecent = m.titleGenRecent[:0]
	for _, s := range recent {
		m.titleGenRecent = append(m.titleGenRecent, s.SessionID)
	}
	msg := "generate titles with claude -p:"
	if sel != nil {
		msg += fmt.Sprintf("  y = %s only", sel.Ref())
		if sel.CustomTitle != "" {
			msg += " (renamed — the rename still wins)"
		}
	}
	if len(recent) > 0 {
		msg += fmt.Sprintf("  a = %d recent (24h, untitled or updated)", len(recent))
	}
	m.awaitTitleGenConfirm = true
	m.flash = msg
}

// handleKeyTitleGenConfirm resolves the banner. Any key other than an
// offered choice cancels.
func (m *model) handleKeyTitleGenConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.awaitTitleGenConfirm = false
	var ids []string
	switch msg.String() {
	case "y", "Y":
		if m.titleGenSel != "" {
			ids = []string{m.titleGenSel}
		}
	case "a", "A":
		ids = append(ids, m.titleGenRecent...)
	}
	if len(ids) == 0 {
		m.flash = "title generation cancelled"
		return m, nil
	}
	return m, m.generateTitles(ids)
}

// titleGenFooterHint is the key legend under the ctrl+t banner.
func (m *model) titleGenFooterHint() string {
	hint := ""
	if m.titleGenSel != "" {
		hint += "y selected · "
	}
	if len(m.titleGenRecent) > 0 {
		hint += fmt.Sprintf("a recent %d · ", len(m.titleGenRecent))
	}
	return hint + "any other key cancels"
}

// titlesKickedMsg: the store accepted a title batch.
type titlesKickedMsg struct{ sessionIDs []string }

func (m *model) generateTitles(ids []string) tea.Cmd {
	st := m.store
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := st.GenerateTitles(c, ids); err != nil {
			return attachDoneMsg{err: fmt.Errorf("generate titles: %w", err)}
		}
		return titlesKickedMsg{sessionIDs: ids}
	}
}

// watchTitles flashes once every session of a batch this TUI started has
// left "running" (the kickoff set it before titlesKickedMsg, so a done /
// error seen afterwards is this run's result).
func (m *model) watchTitles() {
	if len(m.titleWatch) == 0 {
		return
	}
	done, failed := 0, 0
	for _, s := range m.allSessions {
		if _, ok := m.titleWatch[s.SessionID]; !ok {
			continue
		}
		switch s.TitleStatus {
		case "done":
			done++
		case "error":
			failed++
		default:
			return // still running
		}
	}
	switch {
	case failed == 0:
		m.flash = fmt.Sprintf("titles updated (%d)", done)
	case done == 0:
		m.flash = "title generation failed (details in the collector log)"
	default:
		m.flash = fmt.Sprintf("titles updated (%d), %d failed", done, failed)
	}
	m.titleWatch = map[string]struct{}{}
}
