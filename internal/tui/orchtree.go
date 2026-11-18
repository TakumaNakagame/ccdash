package tui

// The orchestrator tree in the session list (docs/decisions/
// 0009-orchestrator.md §8, §10). The order, the buckets and who hangs
// under whom come from internal/orchtree — the Go twin of the portal's
// orchtree block, pinned to the same fixture — so the TUI, the portal and
// the Android app lay the list out alike. This file is what the TUI adds:
// the tree gutter and the row marks, z (fold a global orchestrator's own
// children), Z (only this session and its children) and m (the children
// window).

import (
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/TakumaNakagame/ccdash/internal/i18n"
	mdl "github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/orchtree"
)

// bucketOrch heads the global orchestrators' block (a NUL can't collide
// with a date label; the header shows 統括).
const bucketOrch = "\x00orch"

var (
	// orchHeaderStyle: the 統括 band — the date headings' shape in the role
	// badge's colours, a step louder.
	orchHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("60"))
	// treeNoteStyle: the "only this session's children" line over the list.
	treeNoteStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("#38bdf8"))
	kidsStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
)

// viaMark leads the "parent is not the row above" mark: "↳A12". U+21B3 is
// East Asian Width Neutral — one cell in every locale, unlike ←↑→↓, which
// are Ambiguous (TestViaMarkWidth pins it) — so it needs no ASCII stand-in.
const viaMark = "↳"

// rowDeco is what the tree adds to a session row.
type rowDeco struct {
	via      string         // the parent's ref for the "↳ref" mark; "" = none
	kids     orchtree.Count // the subtree below the row
	foldable bool           // a global orchestrator with children of its own to fold
	folded   bool
}

// treeGutter is the indent in front of a row hanging under another, for
// its first line and for the rest. ASCII only: box-drawing characters are
// East Asian ambiguous width.
func treeGutter(depth int) (first, rest string) {
	if depth <= 0 {
		return "", ""
	}
	pad := strings.Repeat("  ", depth-1)
	return pad + "`- ", pad + "   "
}

// orderSessions lays the filtered rows out (orchtree.ListOrder) into
// m.sessions, with one m.sessMeta entry per row.
func (m *model) orderSessions(src []mdl.Session) {
	opt := orchtree.Options{Flat: m.settings.RecentOrder}
	m.foldApplied = !m.gridOpen
	if !m.gridOpen {
		// The grid shows whatever is on it: a fold never takes a tile away.
		opt.Collapsed = m.orchFolded
	}
	entries := orchtree.ListOrder(src, opt)
	if m.settings.NewestAtBottom {
		entries = reverseUnits(entries)
	}
	m.sessMeta = entries
	m.sessions = make([]mdl.Session, len(entries))
	for i, e := range entries {
		m.sessions[i] = e.S
	}
}

// reverseUnits is newest_at_bottom: the list upside down, except that a
// row and the rows hanging under it stay in reading order (parent first)
// — a tree drawn bottom-up can't be read. Without parents every unit is
// one row and this is a plain reversal, as before the tree.
func reverseUnits(es []orchtree.Entry) []orchtree.Entry {
	out := make([]orchtree.Entry, 0, len(es))
	end := len(es)
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Depth == 0 {
			out = append(out, es[i:end]...)
			end = i
		}
	}
	return append(out, es[:end]...) // rows above the first root (never, by ListOrder)
}

// metaAt is the layout entry of m.sessions[i]; ok is false when the list
// was set without one (tests that fill m.sessions by hand).
func (m *model) metaAt(i int) (orchtree.Entry, bool) {
	if i < 0 || i >= len(m.sessMeta) || len(m.sessMeta) != len(m.sessions) || m.sessMeta[i].S.SessionID != m.sessions[i].SessionID {
		return orchtree.Entry{}, false
	}
	return m.sessMeta[i], true
}

// rowBucket is the heading m.sessions[i] goes under.
func (m *model) rowBucket(i int, now time.Time, flat bool) string {
	s := m.sessions[i]
	e, ok := m.metaAt(i)
	if !ok {
		return bucketFor(s, now, flat)
	}
	switch {
	case e.Bucket == orchtree.BucketOrch:
		return bucketOrch
	case e.Bucket == orchtree.BucketLater:
		return bucketLater
	case strings.HasPrefix(e.Bucket, orchtree.ProjectBucket):
		return projectBucketPrefix + strings.TrimPrefix(e.Bucket, orchtree.ProjectBucket)
	}
	return dateBucket(s, now, flat)
}

// tree is the parent links of every session in view (whatever the tab or
// the search shows), read once per refresh.
func (m *model) tree() *orchtree.Index {
	if m.treeIdx == nil {
		m.treeIdx = orchtree.NewIndex(m.allSessions)
	}
	return m.treeIdx
}

// canFold reports whether s is a global orchestrator with children of its
// own (in no project) — the rows z folds away.
func (m *model) canFold(s mdl.Session) bool {
	if !orchtree.IsTopOrch(s) || m.settings.RecentOrder {
		return false
	}
	for _, c := range m.tree().Kids(s.SessionID) {
		if c.Project == "" {
			return true
		}
	}
	return false
}

// decoAt is the tree's marks for m.sessions[i].
func (m *model) decoAt(i int) rowDeco {
	s := m.sessions[i]
	d := rowDeco{kids: m.tree().Count(s.SessionID)}
	if e, ok := m.metaAt(i); ok && e.Via != "" {
		if p, ok := m.tree().Get(e.Via); ok {
			d.via = p.Ref()
		}
	}
	if m.canFold(s) {
		d.foldable, d.folded = true, m.orchFolded[s.SessionID]
	}
	return d
}

// kidsText is "子 3 · 判断待ち 1" for a subtree ("" without children).
func kidsText(c orchtree.Count) string {
	if c.N == 0 {
		return ""
	}
	out := i18n.Tf("子 %d", c.N)
	if c.Needs > 0 {
		out += " · " + i18n.Tf("判断待ち %d", c.Needs)
	}
	return out
}

// kidsPrefix is what leads a row's second line when it has children: the
// counts, "[+]" in front while they are folded away. Returns the styled
// text and its width.
func kidsPrefix(d rowDeco) (string, int) {
	if d.kids.N == 0 {
		return "", 0
	}
	plain, styled := "", ""
	if d.folded {
		plain, styled = "[+] ", pendingStyle.Render("[+]")+" "
	}
	n := i18n.Tf("子 %d", d.kids.N)
	plain += n
	styled += kidsStyle.Render(n)
	if d.kids.Needs > 0 {
		k := i18n.Tf("判断待ち %d", d.kids.Needs)
		plain += " · " + k
		styled += subtitleStyle.Render(" · ") + pendingStyle.Render(k)
	}
	plain += " · "
	styled += subtitleStyle.Render(" · ")
	return styled, runewidth.StringWidth(plain)
}

// keepSelection re-runs the filter and puts the cursor back on sid (the
// nearest row when it left the list).
func (m *model) keepSelection(sid string) {
	m.applyGroupFilter()
	for i, s := range m.sessions {
		if s.SessionID == sid {
			m.selSess = i
			return
		}
	}
	m.selSess = clamp(m.selSess, 0, max(len(m.sessions)-1, 0))
}

// toggleFold is z: fold / unfold the children of a global orchestrator —
// the selected row's, or the one the selected row hangs under. The fold
// lasts for this run of the TUI (like the search and the ! filter; the
// portal keeps its own per browser).
func (m *model) toggleFold() tea.Cmd {
	if len(m.sessions) == 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	if m.gridOpen {
		return nil // nothing folds on the grid
	}
	if m.settings.RecentOrder {
		m.flash = i18n.T("更新順（O）では畳めません")
		return nil
	}
	s := m.sessions[m.selSess]
	target, ok := s, m.canFold(s)
	if !ok {
		// A child in the 統括 block: fold the orchestrator above it.
		if e, has := m.metaAt(m.selSess); has && e.Bucket == orchtree.BucketOrch {
			seen := map[string]bool{}
			for p, found := m.tree().Get(s.Parent); found && !seen[p.SessionID]; p, found = m.tree().Get(p.Parent) {
				if m.canFold(p) {
					target, ok = p, true
					break
				}
				seen[p.SessionID] = true
			}
		}
	}
	if !ok {
		m.flash = i18n.T("畳めるのは統括の子セッションだけです")
		return nil
	}
	if m.orchFolded == nil {
		m.orchFolded = map[string]bool{}
	}
	if m.orchFolded[target.SessionID] {
		delete(m.orchFolded, target.SessionID)
		m.flash = i18n.Tf("%s の子セッションを開きました", target.Ref())
	} else {
		m.orchFolded[target.SessionID] = true
		m.flash = i18n.Tf("%s の子セッションを畳みました（z で開く）", target.Ref())
	}
	// The cursor goes to the orchestrator: its children may be gone.
	m.keepSelection(target.SessionID)
	return m.loadTailCmd()
}

// toggleSubtree is Z: show only the selected session and the sessions
// below it; Z again (or esc) shows everything.
func (m *model) toggleSubtree() tea.Cmd {
	cur := m.currentSessionID()
	if m.subtreeRoot != "" {
		m.subtreeRoot = ""
		m.flash = i18n.T("すべてのセッションを表示")
		m.keepSelection(cur)
		return m.loadTailCmd()
	}
	if cur == "" {
		return nil
	}
	if m.tree().Count(cur).N == 0 {
		m.flash = i18n.T("このセッションに子セッションはありません")
		return nil
	}
	m.subtreeRoot = cur
	m.keepSelection(cur)
	return m.loadTailCmd()
}

// subtreeNote is the line over the list while Z narrows it ("" otherwise).
func (m *model) subtreeNote(width int) string {
	if m.subtreeRoot == "" {
		return ""
	}
	root, ok := m.tree().Get(m.subtreeRoot)
	if !ok {
		return ""
	}
	hint := i18n.T("Z / esc で全表示")
	// The way out always fits; the title goes first when the pane is narrow.
	text := i18n.Tf("%s とその子だけ表示中", strings.TrimSpace(root.Ref()+" "+root.DisplayTitle()))
	room := width - runewidth.StringWidth(hint) - 4
	if runewidth.StringWidth(text) > room {
		text = i18n.Tf("%s とその子だけ表示中", root.Ref())
	}
	if runewidth.StringWidth(text) > room {
		text = i18n.Tf("%s の子だけ", root.Ref())
	}
	line := " " + runewidth.Truncate(text, max(room, 1), "…") + "  " + hint
	return treeNoteStyle.Render(padRight(runewidth.Truncate(line, width, ""), width))
}

// --- the children window (m) ---

// childrenEntry is one line of the children window: a group heading
// (session == nil) or a session.
type childrenEntry struct {
	head    string
	n       int
	needs   bool
	session *mdl.Session
}

// childrenEntries builds the window for root: 判断待ち over its whole
// subtree (the longest wait first), then its direct children that are
// working, have reported, or are done — the rule of the portal's children
// pane and of my_sessions (orchtree.ChildGroups).
func (m *model) childrenEntries(root mdl.Session, now time.Time) []childrenEntry {
	ix := m.tree()
	var needs []mdl.Session
	for _, s := range ix.Descendants(root.SessionID) {
		if orchtree.NeedsOperator(s) {
			needs = append(needs, s)
		}
	}
	waitAt := func(s mdl.Session) time.Time {
		t := s.AttentionAt
		if s.WaitAt.After(t) {
			t = s.WaitAt
		}
		if t.IsZero() {
			t = orchtree.ChangedAt(s)
		}
		return t
	}
	sort.SliceStable(needs, func(i, j int) bool { return waitAt(needs[i]).Before(waitAt(needs[j])) })
	g := orchtree.ChildGroups(ix.Kids(root.SessionID), orchtree.ChildShown(root, now))
	var out []childrenEntry
	add := func(head string, list []mdl.Session, isNeeds bool) {
		if len(list) == 0 {
			return
		}
		out = append(out, childrenEntry{head: head, n: len(list), needs: isNeeds})
		for i := range list {
			out = append(out, childrenEntry{session: &list[i], needs: isNeeds})
		}
	}
	add(i18n.T("判断待ち"), needs, true)
	add(i18n.T("作業中"), g[orchtree.GroupWorking], false)
	add(i18n.T("報告あり"), g[orchtree.GroupReported], false)
	add(i18n.T("終わり"), g[orchtree.GroupFinished], false)
	return out
}

// openChildren is m: the children of the selected session, grouped. Only
// for a session with a role or with children.
func (m *model) openChildren() {
	if len(m.sessions) == 0 || m.selSess >= len(m.sessions) {
		return
	}
	s := m.sessions[m.selSess]
	if s.Role == "" && len(m.tree().Kids(s.SessionID)) == 0 {
		m.flash = i18n.T("このセッションに子セッションはありません")
		return
	}
	m.childrenOpen = true
	m.childrenRoot = s.SessionID
	m.childrenSel = -1
	m.childrenScroll = 0
	m.childrenStep(1)
}

func (m *model) closeChildren() {
	m.childrenOpen = false
	m.childrenRoot = ""
}

// childrenList is the open window's lines (nil when its session is gone).
func (m *model) childrenList() (mdl.Session, []childrenEntry, bool) {
	root, ok := m.tree().Get(m.childrenRoot)
	if !ok {
		return mdl.Session{}, nil, false
	}
	return root, m.childrenEntries(root, time.Now()), true
}

// childrenStep moves the window's cursor to the next session line in
// direction dir, skipping headings; it stays put at either end.
func (m *model) childrenStep(dir int) {
	_, es, ok := m.childrenList()
	if !ok {
		return
	}
	for i := m.childrenSel + dir; i >= 0 && i < len(es); i += dir {
		if es[i].session != nil {
			m.childrenSel = i
			return
		}
	}
	if m.childrenSel < 0 || m.childrenSel >= len(es) || es[m.childrenSel].session == nil {
		m.childrenSel = -1
		for i, e := range es {
			if e.session != nil {
				m.childrenSel = i
				break
			}
		}
	}
}

// handleKeyChildren: j/k move, enter jumps to that session in the list
// (where the usual keys — a / A / d, enter — act on it), esc / q / m close.
// Nothing here answers anything on a session's behalf.
func (m *model) handleKeyChildren(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "j", "down", "tab":
		m.childrenStep(1)
	case "k", "up", "shift+tab":
		m.childrenStep(-1)
	case "g", "home":
		m.childrenSel = -1
		m.childrenStep(1)
	case "G", "end":
		_, es, _ := m.childrenList()
		m.childrenSel = len(es)
		m.childrenStep(-1)
	case "enter":
		_, es, ok := m.childrenList()
		if !ok || m.childrenSel < 0 || m.childrenSel >= len(es) || es[m.childrenSel].session == nil {
			m.closeChildren()
			return m, nil
		}
		sid := es[m.childrenSel].session.SessionID
		ref := es[m.childrenSel].session.Ref()
		m.closeChildren()
		return m, m.jumpToSession(sid, ref)
	case "esc", "q", "m":
		m.closeChildren()
	}
	return m, nil
}

// jumpToSession puts the list's cursor on sid. A row that a fold, the Z
// filter, the search or the ! filter hides is brought back by dropping
// what hides it; one in another tab or the other archive view is named.
func (m *model) jumpToSession(sid, ref string) tea.Cmd {
	var cmds []tea.Cmd
	if m.gridOpen {
		// "In the list": the grid may not hold that session.
		cmds = append(cmds, m.toggleGrid())
		m.applyGroupFilter()
	}
	find := func() int {
		for i, s := range m.sessions {
			if s.SessionID == sid {
				return i
			}
		}
		return -1
	}
	i := find()
	if i < 0 {
		m.subtreeRoot, m.searchQuery, m.attentionOnly, m.accountFilter = "", "", false, ""
		for id := range m.orchFolded {
			delete(m.orchFolded, id)
		}
		m.applyGroupFilter()
		i = find()
	}
	if i < 0 && !m.groupLocked && m.groupFilter != "" {
		m.groupFilter = ""
		m.applyGroupFilter()
		i = find()
	}
	if i < 0 {
		m.selSess = clamp(m.selSess, 0, max(len(m.sessions)-1, 0))
		m.flash = i18n.Tf("%s はこの一覧にありません", ref)
		return tea.Batch(cmds...)
	}
	return tea.Batch(append(cmds, m.jumpTo(i))...)
}

// childrenBox renders the children window.
func (m *model) childrenBox() string {
	w := min(max(m.width-8, 30), 100)
	inner := w - 4
	root, es, ok := m.childrenList()
	if !ok {
		return ""
	}
	c := m.tree().Count(root.SessionID)
	head := strings.TrimSpace(root.Ref() + " " + root.DisplayTitle())
	lines := []string{
		titleStyle.Render(runewidth.Truncate(head, inner, "…")),
		subtitleStyle.Render(runewidth.Truncate(firstNonEmpty(kidsText(c), i18n.T("子セッションはまだありません")), inner, "…")),
		"",
	}
	selLine := 0
	for i, e := range es {
		if e.session == nil {
			h := " " + e.head + " " + itoa(e.n) + " "
			st := groupHeaderStyle
			if e.needs {
				st = laterHeaderStyle
			}
			lines = append(lines, st.Render(padRight(runewidth.Truncate(h, inner, ""), inner)))
			continue
		}
		s := *e.session
		marker := "  "
		if i == m.childrenSel {
			marker = "> "
			selLine = len(lines)
		}
		why := ""
		switch {
		case s.PendingCount > 0:
			why = i18n.T("承認待ち")
		case s.Attention == mdl.AttentionNeedsYou:
			why = firstNonEmpty(i18n.Reason(s.AttentionReason), i18n.T("要対応"))
		default:
			why = waitText(s)
		}
		tail := m.sessionTime(orchtree.ChangedAt(s))
		if k := kidsText(m.tree().Count(s.SessionID)); k != "" {
			tail = k + " · " + tail
		}
		if s.Project != "" {
			tail = runewidth.Truncate(s.Project, 16, "…") + " · " + tail
		}
		left := marker + s.Ref() + " "
		room := inner - runewidth.StringWidth(left) - runewidth.StringWidth(tail) - 2
		title := s.DisplayTitle()
		if title == "" {
			title = "(no prompt yet)"
		}
		if why != "" {
			title += " — " + why
		}
		if room < 6 {
			// Too narrow for the tail: the title wins.
			tail, room = "", inner-runewidth.StringWidth(left)
		}
		text := left + runewidth.Truncate(title, max(room, 1), "…")
		row := padRight(text, inner-runewidth.StringWidth(tail)) + tail
		row = runewidth.Truncate(row, inner, "")
		switch {
		case i == m.childrenSel:
			row = selectedRow.Render(padRight(row, inner))
		case e.needs:
			row = pendingRowStyle.Render(row)
		}
		lines = append(lines, row)
	}
	// Clip to the terminal, keeping the cursor in view.
	room := max(m.height-8, 4)
	const fixed = 3 // the heading lines stay
	if body := lines[fixed:]; len(body) > room {
		sel := selLine - fixed
		if sel < m.childrenScroll {
			m.childrenScroll = max(sel-1, 0)
		}
		if sel >= m.childrenScroll+room {
			m.childrenScroll = sel - room + 1
		}
		m.childrenScroll = clamp(m.childrenScroll, 0, len(body)-room)
		lines = append(lines[:fixed:fixed], body[m.childrenScroll:m.childrenScroll+room]...)
	} else {
		m.childrenScroll = 0
	}
	lines = append(lines, "", subtitleStyle.Render(runewidth.Truncate(i18n.T("j/k 選ぶ · enter 一覧でそのセッションへ · esc 閉じる"), inner, "…")))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(0, 1).
		Width(w - 2).
		Render(strings.Join(lines, "\n"))
	title := " " + i18n.T("子セッション") + " "
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > runewidth.StringWidth(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+runewidth.StringWidth(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func itoa(n int) string { return strconv.Itoa(n) }
