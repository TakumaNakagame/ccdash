package tui

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	mdl "github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/orchtree"
)

// --- the list's order before the orchestrator tree, kept verbatim ---

func legacyByActivity(src []mdl.Session) []mdl.Session {
	at := func(s mdl.Session) time.Time {
		if !s.LastActivity.IsZero() {
			return s.LastActivity
		}
		return s.LastSeen
	}
	out := append([]mdl.Session(nil), src...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := at(out[i]), at(out[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Num > out[j].Num
	})
	return out
}

func legacyLaterFirst(src []mdl.Session) []mdl.Session {
	var later, rest []mdl.Session
	for _, s := range src {
		if s.Later {
			later = append(later, s)
		} else {
			rest = append(rest, s)
		}
	}
	if len(later) == 0 {
		return src
	}
	sort.SliceStable(later, func(i, j int) bool { return later[i].LaterAt.After(later[j].LaterAt) })
	return append(later, rest...)
}

func legacyProjectsFirst(src []mdl.Session) []mdl.Session {
	order := map[string]int{}
	for _, s := range src {
		if s.Project != "" {
			order[s.Project] = s.ProjectOrder
		}
	}
	if len(order) == 0 {
		return src
	}
	var in, rest []mdl.Session
	for _, s := range src {
		if s.Project != "" {
			in = append(in, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Project != b.Project {
			if oa, ob := order[a.Project], order[b.Project]; oa != ob {
				return oa < ob
			}
			return a.Project < b.Project
		}
		if a.Favorite != b.Favorite {
			return a.Favorite
		}
		if a.Favorite && !a.PinnedAt.Equal(b.PinnedAt) {
			return a.PinnedAt.After(b.PinnedAt)
		}
		if !a.FirstSeen.Equal(b.FirstSeen) {
			return a.FirstSeen.After(b.FirstSeen)
		}
		return a.Num > b.Num
	})
	return append(in, rest...)
}

// legacyOrder is applyGroupFilter's ordering step as it was.
func legacyOrder(src []mdl.Session, recent, bottom bool) []mdl.Session {
	if recent {
		src = legacyByActivity(src)
	} else {
		src = legacyLaterFirst(legacyProjectsFirst(src))
	}
	if bottom {
		src = append([]mdl.Session(nil), src...)
		for i, j := 0, len(src)-1; i < j; i, j = i+1, j-1 {
			src[i], src[j] = src[j], src[i]
		}
	}
	return src
}

func ids(ss []mdl.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.SessionID
	}
	return out
}

// legacyRows are the shared fixture's rows without any role or parent
// (pins, projects, watch later, unread, dates), in the collector's order.
func legacyRows(t *testing.T) [][]mdl.Session {
	t.Helper()
	raw, err := os.ReadFile("../hub/testdata/orchtree.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Order []struct {
			Rows   []mdl.Session `json:"rows"`
			Legacy bool          `json:"legacy"`
		} `json:"order"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var out [][]mdl.Session
	for _, c := range fx.Order {
		if c.Legacy {
			out = append(out, orchtree.ListSort(c.Rows))
		}
	}
	if len(out) == 0 {
		t.Fatal("the fixture has no legacy case")
	}
	// One more, with ties and a pinned later project member.
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	out = append(out, orchtree.ListSort([]mdl.Session{
		{SessionID: "p1", Num: 1, Project: "b", ProjectOrder: 2, FirstSeen: at, LastSeen: at},
		{SessionID: "p2", Num: 2, Project: "b", ProjectOrder: 2, FirstSeen: at, LastSeen: at.Add(time.Hour), Favorite: true, PinnedAt: at},
		{SessionID: "p3", Num: 3, Project: "a", ProjectOrder: 2, FirstSeen: at.Add(time.Minute), LastSeen: at, Later: true, LaterAt: at},
		{SessionID: "p4", Num: 4, Project: "a", ProjectOrder: 2, FirstSeen: at, LastSeen: at.Add(-48 * time.Hour)},
		{SessionID: "n1", Num: 5, FirstSeen: at, LastSeen: at.Add(-72 * time.Hour), LastActivity: at.Add(2 * time.Hour)},
		{SessionID: "n2", Num: 6, FirstSeen: at, LastSeen: at, Favorite: true, PinnedAt: at.Add(time.Hour)},
		{SessionID: "n3", Num: 7, FirstSeen: at, LastSeen: at, Unseen: true},
	}))
	return out
}

// TestListOrderLegacy: with no role and no parent anywhere the list is
// what it was before the tree — the same rows in the same order under the
// same headings, never indented or marked — in every order setting.
func TestListOrderLegacy(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	for n, rows := range legacyRows(t) {
		for _, recent := range []bool{false, true} {
			for _, bottom := range []bool{false, true} {
				m := newModel(context.Background(), nil, RemoteInfo{})
				m.width, m.height = 120, 60
				m.settings.RecentOrder, m.settings.NewestAtBottom = recent, bottom
				m.allSessions = append([]mdl.Session(nil), rows...)
				m.applyGroupFilter()
				want := legacyOrder(append([]mdl.Session(nil), rows...), recent, bottom)
				if got, w := ids(m.sessions), ids(want); !reflect.DeepEqual(got, w) {
					t.Errorf("rows %d recent=%v bottom=%v\n  got  %v\n  want %v", n, recent, bottom, got, w)
					continue
				}
				for i, s := range m.sessions {
					e, ok := m.metaAt(i)
					if !ok || e.Depth != 0 || e.Via != "" {
						t.Errorf("rows %d: %s laid out as %+v", n, s.SessionID, e)
					}
					if got, w := m.rowBucket(i, now, recent), bucketFor(s, now, recent); got != w {
						t.Errorf("rows %d recent=%v: %s under %q, was %q", n, recent, s.SessionID, got, w)
					}
					if d := m.decoAt(i); d != (rowDeco{}) {
						t.Errorf("rows %d: %s decorated %+v", n, s.SessionID, d)
					}
				}
				if out := ansi.Strip(m.renderSessionsList(60, 60)); strings.Contains(out, "統括") || strings.Contains(out, "`-") || strings.Contains(out, viaMark) {
					t.Errorf("rows %d: a tree mark without a tree:\n%s", n, out)
				}
			}
		}
	}
}

// treeModel: a global orchestrator with two own children (one waits on
// the operator) and a grandchild, a project with its orchestrator and two
// members, and two loose sessions.
func treeModel() *model {
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.Local)
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 160, 50
	m.allSessions = orchtree.ListSort([]mdl.Session{
		{SessionID: "G", Num: 1, HostLetter: "A", Title: "全体の統括", Role: mdl.RoleOrchestrator, FirstSeen: at, LastSeen: at, Status: mdl.StatusIdle},
		{SessionID: "k1", Num: 2, HostLetter: "A", Title: "ドキュメントの表記ゆれを直して回る作業（日本語の長い題名）", Parent: "G", FirstSeen: at, LastSeen: at.Add(time.Minute), Status: mdl.StatusActive},
		{SessionID: "k2", Num: 3, HostLetter: "A", Title: "needs an answer", Parent: "G", FirstSeen: at, LastSeen: at.Add(2 * time.Minute), Status: mdl.StatusIdle, Attention: mdl.AttentionNeedsYou, AttentionReason: "質問", AttentionAt: at},
		{SessionID: "g1", Num: 4, HostLetter: "A", Title: "孫セッション", Parent: "k1", FirstSeen: at, LastSeen: at.Add(3 * time.Minute), Status: mdl.StatusStopped},
		{SessionID: "g2", Num: 9, HostLetter: "A", Title: "ひ孫セッション", Parent: "g1", FirstSeen: at, LastSeen: at.Add(3 * time.Minute), Status: mdl.StatusStopped, PendingCount: 1},
		{SessionID: "PO", Num: 5, HostLetter: "A", Title: "web の統括", Role: mdl.RoleProjectOrchestrator, Project: "web", Parent: "G", FirstSeen: at, LastSeen: at, Status: mdl.StatusIdle},
		{SessionID: "w1", Num: 6, HostLetter: "A", Title: "ログイン画面を作り直す", Project: "web", Parent: "PO", FirstSeen: at, LastSeen: at.Add(time.Hour), Status: mdl.StatusIdle, Wait: mdl.WaitAnswer, WaitAt: at},
		{SessionID: "w2", Num: 7, HostLetter: "A", Title: "by hand", Project: "web", FirstSeen: at.Add(time.Minute), LastSeen: at, Status: mdl.StatusStopped},
		{SessionID: "x", Num: 8, HostLetter: "A", Title: "loose", FirstSeen: at, LastSeen: at.Add(-time.Hour), Status: mdl.StatusStopped},
	})
	m.applyGroupFilter()
	return m
}

func layout(m *model) []string {
	out := make([]string, len(m.sessions))
	for i, s := range m.sessions {
		e, _ := m.metaAt(i)
		out[i] = s.SessionID + "|" + e.Bucket + "|" + string(rune('0'+e.Depth)) + "|" + e.Via
	}
	return out
}

func press(m *model, k string) {
	msg := tea.KeyPressMsg{Code: rune(k[0]), Text: k}
	switch k {
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	m.Update(msg)
}

func selectID(t *testing.T, m *model, id string) {
	t.Helper()
	for i, s := range m.sessions {
		if s.SessionID == id {
			m.selSess = i
			return
		}
	}
	t.Fatalf("%s is not listed: %v", id, ids(m.sessions))
}

// TestTreeLayout: the TUI lays the tree out as orchtree says, and
// newest_at_bottom turns the list over without turning a tree upside down.
func TestTreeLayout(t *testing.T) {
	m := treeModel()
	want := []string{
		"G|orch|0|", "k2|orch|1|", "k1|orch|1|", "g1|orch|2|", "g2|orch|2|g1",
		"PO|p:web|0|G", "w1|p:web|1|", "w2|p:web|0|",
		"x||0|",
	}
	if got := layout(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("layout\n  got  %v\n  want %v", got, want)
	}
	out := ansi.Strip(m.renderSessionsList(110, 40))
	for _, s := range []string{"統括", "`- ", "  `- ", viaMark + "A4", viaMark + "A1", "子 6 · 判断待ち 3", "子 1 · 判断待ち 1", "PROJECT  web"} {
		if !strings.Contains(out, s) {
			t.Errorf("the list lacks %q:\n%s", s, out)
		}
	}
	// The project header carries its orchestrator's counts.
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "PROJECT  web") && !strings.Contains(l, "子 1 · 判断待ち 1") {
			t.Errorf("project header: %q", l)
		}
	}

	m.settings.NewestAtBottom = true
	m.applyGroupFilter()
	want = []string{
		"x||0|",
		"w2|p:web|0|", "PO|p:web|0|G", "w1|p:web|1|",
		"G|orch|0|", "k2|orch|1|", "k1|orch|1|", "g1|orch|2|", "g2|orch|2|g1",
	}
	if got := layout(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("newest at bottom\n  got  %v\n  want %v", got, want)
	}
}

// TestTreeRowWidths: an indented row with a CJK title never overflows its
// slot or wraps — wide, narrow and vertical layouts, selected or not.
func TestTreeRowWidths(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{110, 40}, {60, 40}, {44, 40}, {30, 40}, {24, 30}} {
		m := treeModel()
		for sel := range m.sessions {
			m.selSess = sel
			out := m.renderSessionsList(tc.w, tc.h)
			lines := strings.Split(out, "\n")
			if len(lines) != tc.h {
				t.Fatalf("width %d sel %d: %d lines, want %d (a row wrapped)", tc.w, sel, len(lines), tc.h)
			}
			for _, l := range lines {
				if w := ansi.StringWidth(l); w > tc.w {
					t.Fatalf("width %d sel %d: a %d-cell line: %q", tc.w, sel, w, ansi.Strip(l))
				}
			}
		}
	}
	// The whole screen, in the vertical layout at a narrow size.
	m := treeModel()
	m.width, m.height = 46, 40
	m.settings.LayoutMode = "vertical"
	selectID(t, m, "g2")
	if !m.useVerticalLayout() {
		t.Fatal("not the vertical layout")
	}
	// (The body only: the header bar has its own, older, width rules.)
	lines := strings.Split(m.renderBody(30), "\n")
	if len(lines) > 30 {
		t.Fatalf("%d lines > height 30", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("a %d-cell line on a %d-cell screen: %q", w, m.width, ansi.Strip(l))
		}
	}
	// The role badge and the gutter share the row's budget.
	row := ansi.Strip(m.renderSessionRowDeco(m.allSessions[0], false, 30, rowDeco{via: "A12", kids: orchtree.Count{N: 3, Needs: 1}}))
	if !strings.Contains(row, viaMark+"A12") {
		t.Errorf("row: %q", row)
	}
}

// TestViaMarkWidth: the arrow of the parent mark is one cell whatever the
// locale says about ambiguous-width characters.
func TestViaMarkWidth(t *testing.T) {
	for _, ea := range []bool{false, true} {
		c := runewidth.NewCondition()
		c.EastAsianWidth = ea
		if w := c.StringWidth(viaMark); w != 1 {
			t.Errorf("east asian %v: %q is %d cells", ea, viaMark, w)
		}
		first, rest := treeGutter(2)
		if c.StringWidth(first) != 5 || c.StringWidth(rest) != 5 || len(first) != 5 {
			t.Errorf("gutter %q / %q is not 5 ASCII cells", first, rest)
		}
	}
}

// TestFoldKey: z folds a global orchestrator's own children (from the
// orchestrator or from one of them), z again brings them back; the grid's
// tiles are not touched by a fold.
func TestFoldKey(t *testing.T) {
	m := treeModel()
	selectID(t, m, "g1")
	press(m, "z")
	if got, want := ids(m.sessions), []string{"G", "PO", "w1", "w2", "x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("folded: %v, want %v", got, want)
	}
	if m.sessions[m.selSess].SessionID != "G" {
		t.Fatalf("the cursor is on %s, want the orchestrator", m.sessions[m.selSess].SessionID)
	}
	if out := ansi.Strip(m.renderSessionsList(110, 40)); !strings.Contains(out, "[+] 子 6") {
		t.Errorf("no fold mark:\n%s", out)
	}
	// The grid still has the working child as a tile.
	press(m, "V")
	if !m.gridOpen {
		t.Fatalf("grid did not open: %q", m.flash)
	}
	var tiles []string
	for _, i := range m.gridSessions() {
		tiles = append(tiles, m.sessions[i].SessionID)
	}
	if !strings.Contains(strings.Join(tiles, " "), "k1") || !strings.Contains(strings.Join(tiles, " "), "k2") {
		t.Fatalf("grid tiles %v lack the folded children", tiles)
	}
	press(m, "V")
	if m.gridOpen || len(m.sessions) != 5 {
		t.Fatalf("after the grid: open=%v rows=%v", m.gridOpen, ids(m.sessions))
	}
	selectID(t, m, "G")
	press(m, "z")
	if len(m.sessions) != 9 {
		t.Fatalf("unfolded: %v", ids(m.sessions))
	}
	// Not an orchestrator's child: nothing folds, and it says so.
	selectID(t, m, "w1")
	press(m, "z")
	if len(m.sessions) != 9 || m.flash == "" {
		t.Fatalf("z on a project member: rows=%d flash=%q", len(m.sessions), m.flash)
	}
	// Recent order has no tree to fold.
	m.settings.RecentOrder = true
	m.applyGroupFilter()
	selectID(t, m, "G")
	press(m, "z")
	if len(m.orchFolded) != 0 {
		t.Fatal("z folded in recent order")
	}
}

// TestSubtreeKey: Z narrows the list to the session and what is below it,
// says so, and Z or esc shows everything again.
func TestSubtreeKey(t *testing.T) {
	m := treeModel()
	selectID(t, m, "PO")
	press(m, "Z")
	if got, want := ids(m.sessions), []string{"PO", "w1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtree: %v, want %v", got, want)
	}
	if m.sessions[m.selSess].SessionID != "PO" {
		t.Fatal("the cursor left the root")
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "A5 の子だけ") || !strings.Contains(view, "Z / esc で全表示") {
		t.Errorf("no indicator:\n%s", view)
	}
	// The note is a line of its own: a click on it selects nothing.
	if len(m.listLineSess) == 0 || m.listLineSess[0] != -1 {
		t.Errorf("list lines %v", m.listLineSess)
	}
	press(m, "esc")
	if m.subtreeRoot != "" || len(m.sessions) != 9 {
		t.Fatalf("esc: root=%q rows=%d", m.subtreeRoot, len(m.sessions))
	}
	selectID(t, m, "G")
	press(m, "Z")
	if len(m.sessions) != 7 {
		t.Fatalf("the global subtree: %v", ids(m.sessions))
	}
	press(m, "Z")
	if m.subtreeRoot != "" || len(m.sessions) != 9 || m.sessions[m.selSess].SessionID != "G" {
		t.Fatalf("Z again: root=%q rows=%d", m.subtreeRoot, len(m.sessions))
	}
	// No children: nothing to narrow to.
	selectID(t, m, "x")
	press(m, "Z")
	if m.subtreeRoot != "" || m.flash == "" {
		t.Fatalf("Z on a childless session: root=%q flash=%q", m.subtreeRoot, m.flash)
	}
	// The root leaving the list (archived) drops the filter.
	selectID(t, m, "PO")
	press(m, "Z")
	var rest []mdl.Session
	for _, s := range m.allSessions {
		if s.SessionID != "PO" {
			rest = append(rest, s)
		}
	}
	m.allSessions = rest
	m.applyGroupFilter()
	if m.subtreeRoot != "" || len(m.sessions) != 8 {
		t.Fatalf("root gone: root=%q rows=%d", m.subtreeRoot, len(m.sessions))
	}
}

// TestChildrenWindow: m lists the children by group — 判断待ち over the
// whole subtree, the rest direct children — and enter jumps to the row,
// through a fold if need be. It has no key that answers anything.
func TestChildrenWindow(t *testing.T) {
	m := treeModel()
	selectID(t, m, "x")
	press(m, "m")
	if m.childrenOpen {
		t.Fatal("the window opened for a session without role or children")
	}
	selectID(t, m, "G")
	press(m, "m")
	if !m.childrenOpen {
		t.Fatal("m did not open the window")
	}
	root, es, _ := m.childrenList()
	var got []string
	for _, e := range es {
		if e.session == nil {
			got = append(got, e.head)
		} else {
			got = append(got, e.session.SessionID)
		}
	}
	// k2 (a question), w1 (a declared answer) and g2 (an approval) wait
	// anywhere below G; of its direct children k1 works and PO has nothing new.
	want := []string{"判断待ち", "k2", "w1", "g2", "作業中", "k1", "終わり", "PO"}
	if root.SessionID != "G" || !reflect.DeepEqual(got, want) {
		t.Fatalf("window\n  got  %v\n  want %v", got, want)
	}
	box := ansi.Strip(m.View().Content)
	for _, s := range []string{"子セッション", "判断待ち 3", "作業中 1", "A3 needs an answer"} {
		if !strings.Contains(box, s) {
			t.Errorf("the window lacks %q:\n%s", s, box)
		}
	}
	for _, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("a %d-cell line", w)
		}
	}
	// a / d / y do nothing here: answering stays with the list's own keys.
	m.flash = ""
	for _, k := range []string{"a", "A", "d", "y"} {
		press(m, k)
	}
	if !m.childrenOpen || m.flash != "" {
		t.Fatalf("a key acted inside the window: open=%v flash=%q", m.childrenOpen, m.flash)
	}
	press(m, "j")
	press(m, "j")
	press(m, "enter")
	if m.childrenOpen || m.sessions[m.selSess].SessionID != "g2" {
		t.Fatalf("enter: open=%v on %s", m.childrenOpen, m.sessions[m.selSess].SessionID)
	}
	// Folded away: the jump unfolds.
	selectID(t, m, "G")
	press(m, "z")
	press(m, "m")
	press(m, "enter")
	if m.sessions[m.selSess].SessionID != "k2" || len(m.orchFolded) != 0 {
		t.Fatalf("jump through a fold: on %s, folded %v", m.sessions[m.selSess].SessionID, m.orchFolded)
	}
	press(m, "m") // k2 has neither role nor children
	if m.childrenOpen {
		t.Fatal("the window opened for a leaf")
	}
	selectID(t, m, "G")
	press(m, "m")
	press(m, "esc")
	if m.childrenOpen {
		t.Fatal("esc did not close the window")
	}
}
