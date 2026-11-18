// Package orchtree holds the session list's order and the orchestrator
// tree as pure functions: who is fixed where, who hangs under whom, how an
// orchestrator's children are grouped, and which sessions wait on the
// operator. It is the Go twin of the block of internal/hub/web/app.js
// between "// >>> orchtree" and "// <<< orchtree" — TestFixture runs every
// case of internal/hub/testdata/orchtree.json through it, the same cases
// the portal's block passes under node (hub.TestOrchTree) — and the one
// place the grouping lives in Go: ccdash mcp's my_sessions and the TUI
// both use it.
//
// Rows are the collector's session rows. A session id is unique across
// hosts, so parents are matched by id alone.
package orchtree

import (
	"sort"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// The groups a parent's children are filed into, in the order they are
// shown. The values are the keys of my_sessions' result.
const (
	GroupNeeds    = "needs_operator"
	GroupWorking  = "working"
	GroupReported = "reported"
	GroupFinished = "finished"
)

// GroupOrder lists the groups in display order.
var GroupOrder = []string{GroupNeeds, GroupWorking, GroupReported, GroupFinished}

// ReportWindow is how far back a parent that never listed its children
// (no ChildrenSeenAt) looks for turns it hasn't been shown — the same span
// a just-read reply stays marked in the lists.
const ReportWindow = model.RecentReplyWindow

// MaxDepth is how far a row is indented under the row it hangs from;
// deeper descendants stay at this depth, with their parent marked.
const MaxDepth = 2

// The buckets of ListOrder (Entry.Bucket). A project's is ProjectBucket +
// its name; "" is the rest (the list groups those by pin / date).
const (
	BucketOrch    = "orch"
	BucketNew     = "new"
	BucketLater   = "later"
	ProjectBucket = "p:"
)

// NeedsOperator reports whether the session waits on the person: a
// pending approval, a question or dialog (attention needs_you), or a
// declared answer / action. The one rule behind every 判断待ち count.
func NeedsOperator(x model.Session) bool {
	return x.PendingCount > 0 || x.Attention == model.AttentionNeedsYou || model.WaitNeedsYou(x.Wait)
}

func running(x model.Session) bool {
	return x.Status == model.StatusActive || x.Status == model.StatusIdle
}

// ChildGroup files a child: waits on the operator (an approval, a
// question, a declared answer / action), is working (mid-turn, or running
// and waiting on something in the background), has reported, or is done
// with.
//
// "Reported" is a finished turn that is news to someone: the operator
// hasn't looked at it (attention done / Unseen), or its parent hasn't been
// shown it — it ended at or after shown, the parent's previous my_sessions
// call (ReportWindow ago when there was none; ChildShown). The second half
// matters because the operator's mark is cleared by any ccdash screen the
// row sat under for three seconds (the TUI cursor, an open chat): a child
// that finished and was "seen" that way before its parent asked used to
// land in finished, where a parent looking for reports never noticed it.
func ChildGroup(x model.Session, shown time.Time) string {
	switch {
	case NeedsOperator(x):
		return GroupNeeds
	case x.Status == model.StatusActive || running(x) && x.Wait == model.WaitBackground:
		return GroupWorking
	case x.Attention == model.AttentionDone || x.Unseen:
		return GroupReported
	}
	if end := TurnEnd(x); !end.IsZero() && !end.Before(shown) {
		return GroupReported
	}
	return GroupFinished
}

// TurnEnd is when a child that isn't working last finished something: its
// newest activity, the Stop that left the attention mark, or its declared
// status.
func TurnEnd(x model.Session) time.Time {
	t := x.LastActivity
	for _, o := range []time.Time{x.AttentionAt, x.WaitAt} {
		if o.After(t) {
			t = o
		}
	}
	return t
}

// ChildShown is the "shown" of ChildGroup for a parent row: when it last
// called my_sessions (ChildrenSeenAt), else ReportWindow before now.
func ChildShown(parent model.Session, now time.Time) time.Time {
	if !parent.ChildrenSeenAt.IsZero() {
		return parent.ChildrenSeenAt
	}
	return now.Add(-ReportWindow)
}

// ChangedAt is when a child last changed in a way a list of children
// shows; it orders the rows of a group, newest first.
func ChangedAt(x model.Session) time.Time {
	t := x.LastSeen
	for _, o := range []time.Time{x.LastActivity, x.AttentionAt, x.WaitAt, x.FirstSeen} {
		if o.After(t) {
			t = o
		}
	}
	return t
}

// ChildGroups files rows (a parent's children) into the four groups, each
// newest change first.
func ChildGroups(rows []model.Session, shown time.Time) map[string][]model.Session {
	g := map[string][]model.Session{}
	for _, x := range rows {
		k := ChildGroup(x, shown)
		g[k] = append(g[k], x)
	}
	for _, list := range g {
		sort.SliceStable(list, func(i, j int) bool { return ChangedAt(list[i]).After(ChangedAt(list[j])) })
	}
	return g
}

// IsTopOrch: a global orchestrator, fixed at the top of the list — unless
// archived (the archive view is flat).
func IsTopOrch(x model.Session) bool {
	return x.Role == model.RoleOrchestrator && !x.Archived
}

// IsProjectOrch: a project's orchestrator, fixed at the top of its block.
func IsProjectOrch(x model.Session) bool {
	return x.Role == model.RoleProjectOrchestrator && !x.Archived && x.Project != ""
}

// Count is what a session's subtree holds (itself excluded): N sessions,
// Needs of them waiting on the operator.
type Count struct{ N, Needs int }

// Index is the parent links of a set of rows, read once (O(n)).
type Index struct {
	byID   map[string]model.Session
	kids   map[string][]model.Session
	counts map[string]Count
}

// NewIndex reads the parent links of rows. A parent that isn't among rows
// (archived, a host that is offline) leaves its children as roots; a cycle
// of parents is cut where it closes.
func NewIndex(rows []model.Session) *Index {
	ix := &Index{byID: make(map[string]model.Session, len(rows)), kids: map[string][]model.Session{}, counts: map[string]Count{}}
	for _, s := range rows {
		ix.byID[s.SessionID] = s
	}
	for _, s := range rows {
		if s.Parent == "" || s.Parent == s.SessionID {
			continue
		}
		if _, ok := ix.byID[s.Parent]; !ok {
			continue
		}
		ix.kids[s.Parent] = append(ix.kids[s.Parent], s)
	}
	// Post-order over the kids, with an explicit stack: a chain of parents
	// can be as long as the list.
	state := map[string]int{} // 1 while being counted, 2 when done
	type frame struct {
		id   string
		next int
		c    Count
	}
	for _, root := range rows {
		if len(ix.kids[root.SessionID]) == 0 || state[root.SessionID] != 0 {
			continue
		}
		state[root.SessionID] = 1
		stack := []frame{{id: root.SessionID}}
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			kids := ix.kids[f.id]
			if f.next < len(kids) {
				k := kids[f.next]
				f.next++
				switch state[k.SessionID] {
				case 1: // a cycle
				case 2:
					kc := ix.counts[k.SessionID]
					f.c.N += 1 + kc.N
					f.c.Needs += b2i(NeedsOperator(k)) + kc.Needs
				default:
					state[k.SessionID] = 1
					stack = append(stack, frame{id: k.SessionID})
				}
				continue
			}
			done := *f
			stack = stack[:len(stack)-1]
			state[done.id] = 2
			if done.c.N > 0 {
				ix.counts[done.id] = done.c
			}
			if len(stack) > 0 {
				p := &stack[len(stack)-1]
				p.c.N += 1 + done.c.N
				p.c.Needs += b2i(NeedsOperator(ix.byID[done.id])) + done.c.Needs
			}
		}
	}
	return ix
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get returns the row with that session id.
func (ix *Index) Get(id string) (model.Session, bool) {
	s, ok := ix.byID[id]
	return s, ok
}

// Kids returns the direct children of id, in row order.
func (ix *Index) Kids(id string) []model.Session { return ix.kids[id] }

// Count returns what the subtree under id holds (zero without children).
func (ix *Index) Count(id string) Count { return ix.counts[id] }

// Counts returns the count of every session that has descendants.
func (ix *Index) Counts() map[string]Count { return ix.counts }

// parentOf is the row's parent among the rows, if any.
func (ix *Index) parentOf(s model.Session) (model.Session, bool) {
	if s.Parent == "" || s.Parent == s.SessionID {
		return model.Session{}, false
	}
	return ix.Get(s.Parent)
}

// Subtree returns root and every session below it, breadth of discovery
// order (root first).
func (ix *Index) Subtree(root string) []string {
	out := []string{root}
	in := map[string]bool{root: true}
	todo := []string{root}
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, k := range ix.kids[id] {
			if !in[k.SessionID] {
				in[k.SessionID] = true
				out = append(out, k.SessionID)
				todo = append(todo, k.SessionID)
			}
		}
	}
	return out
}

// Descendants returns every session below root (root excluded).
func (ix *Index) Descendants(root string) []model.Session {
	ids := ix.Subtree(root)[1:]
	out := make([]model.Session, 0, len(ids))
	for _, id := range ids {
		out = append(out, ix.byID[id])
	}
	return out
}

// ActivityAt is when a session last moved: LastActivity (newest transcript
// entry or hook), else LastSeen.
func ActivityAt(s model.Session) time.Time {
	if !s.LastActivity.IsZero() {
		return s.LastActivity
	}
	return s.LastSeen
}

// ByActivity is recent order: a sorted copy, the session that moved last
// first, projects and pins ignored.
func ByActivity(src []model.Session) []model.Session {
	out := append([]model.Session(nil), src...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := ActivityAt(out[i]), ActivityAt(out[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Num > out[j].Num
	})
	return out
}

// ListSort is the list's base order, before any block is formed: pins
// first (latest pin first), then last seen. The collector's ListSessions
// already returns rows this way; the fixture's rows need it.
func ListSort(src []model.Session) []model.Session {
	out := append([]model.Session(nil), src...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Favorite != b.Favorite {
			return a.Favorite
		}
		if a.Favorite && !a.PinnedAt.Equal(b.PinnedAt) {
			return a.PinnedAt.After(b.PinnedAt)
		}
		return a.LastSeen.After(b.LastSeen)
	})
	return out
}

// LaterFirst puts "watch later" sessions at the very newest end, in their
// own block, the most recently marked first — ahead of projects, whose
// members they temporarily leave. Returns src when none is marked.
func LaterFirst(src []model.Session) []model.Session {
	var later, rest []model.Session
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

// ProjectsFirst moves the sessions that belong to a project to the newest
// end of the list, one block per project. The order is stable — activity
// never moves anything: projects by their operator-set order (lowest
// first; with several hosts a block takes the lowest of its rows'); inside
// a block pinned sessions come first (most recently pinned first), then
// the rest newest-started first. Everything else keeps its order (pins,
// then by date). Returns src when no row has a project.
func ProjectsFirst(src []model.Session) []model.Session {
	order := map[string]int{}
	for _, s := range src {
		if s.Project == "" {
			continue
		}
		if o, ok := order[s.Project]; !ok || s.ProjectOrder < o {
			order[s.Project] = s.ProjectOrder
		}
	}
	if len(order) == 0 {
		return src
	}
	var in, rest []model.Session
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
		// Pins lead their block, the most recently pinned first.
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

// Options are ListOrder's switches.
type Options struct {
	// Flat is recent order: no blocks, one run by latest activity.
	Flat bool
	// Lift puts unread rows in a block of their own at the top (the
	// portal's 新着を上に; the TUI has no such switch and leaves it off).
	Lift bool
	// Collapsed holds the global orchestrators whose own (no-project)
	// children are folded away.
	Collapsed map[string]bool
}

// Entry is one shown row of the list.
type Entry struct {
	S model.Session
	// Bucket is BucketOrch, BucketNew, BucketLater, ProjectBucket+name, or
	// "" (a date / pin band).
	Bucket string
	// Depth (0–MaxDepth) is how far the row is indented under the row it
	// hangs from.
	Depth int
	// Via is the row's parent (its session id) when that parent is not
	// the row directly above its level — the list shows a "↳ <parent
	// ref>" mark — else "".
	Via string
}

// ListOrder lays the session list out. rows come in the base order
// (ListSort — what the collector returns); the result is one entry per
// shown row, in order.
//
//  1. BucketOrch: every global orchestrator, oldest first (FirstSeen, Num)
//     — never moved by activity, a pin, watch later or NEW. Under each
//     (unless collapsed) its descendants that are in no project, depth 1
//     and 2.
//  2. BucketNew (Lift only): unread rows, last moved first.
//  3. BucketLater: watch-later rows, last marked first.
//  4. one block per project in project order: its project orchestrators
//     (oldest first), each followed by its tree; then the other members
//     in the base order of ProjectsFirst, each root followed by its tree.
//     A member hangs under its parent when that parent is in the same
//     block.
//  5. the rest in base order (the list groups them by pin / date).
//
// Orchestrators never go to 2 or 3. A row taken by 2 or 3 leaves the tree;
// its own children stay where it was, one level up, with Via set. Below
// MaxDepth rows stay at MaxDepth with Via set. Without any role or parent
// this is exactly LaterFirst(ProjectsFirst(rows)) (unread rows first with
// Lift), and ByActivity(rows) when Flat.
func ListOrder(rows []model.Session, opt Options) []Entry {
	out := make([]Entry, 0, len(rows))
	oldest := func(list []model.Session) {
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if !a.FirstSeen.Equal(b.FirstSeen) {
				return a.FirstSeen.Before(b.FirstSeen)
			}
			return a.Num < b.Num
		})
	}
	var tops []model.Session
	for _, s := range rows {
		if IsTopOrch(s) {
			tops = append(tops, s)
		}
	}
	oldest(tops)
	ix := NewIndex(rows)
	put := func(s model.Session, bucket string, depth int, marked bool) {
		e := Entry{S: s, Bucket: bucket, Depth: depth}
		if marked {
			if p, ok := ix.parentOf(s); ok {
				e.Via = p.SessionID
			}
		}
		out = append(out, e)
	}
	if opt.Flat {
		for _, s := range tops {
			put(s, BucketOrch, 0, true)
		}
		for _, s := range ByActivity(rows) {
			if !IsTopOrch(s) {
				put(s, "", 0, true)
			}
		}
		return out
	}
	fixed := func(s model.Session) bool { return IsTopOrch(s) || IsProjectOrch(s) }
	lifted := func(s model.Session) bool { return opt.Lift && s.Unseen && !fixed(s) }
	away := func(s model.Session) bool { return !fixed(s) && (lifted(s) || s.Later) }
	base := ProjectsFirst(rows)
	done := map[string]bool{} // rows placed (or folded away) by a tree walk
	// walk places the children of p that belong to this tree (ok), depth
	// first in row order. under is the depth of the row they hang from.
	var walk func(p model.Session, under int, bucket string, ok func(model.Session) bool, show, loose bool)
	walk = func(p model.Session, under int, bucket string, ok func(model.Session) bool, show, loose bool) {
		for _, c := range ix.kids[p.SessionID] {
			if !ok(c) || done[c.SessionID] {
				continue
			}
			done[c.SessionID] = true
			if away(c) {
				walk(c, under, bucket, ok, show, true)
				continue
			}
			depth := min(under+1, MaxDepth)
			if show {
				put(c, bucket, depth, loose || depth != under+1)
			}
			walk(c, depth, bucket, ok, show, false)
		}
	}
	noProject := func(c model.Session) bool { return c.Project == "" && !fixed(c) }
	for _, o := range tops {
		done[o.SessionID] = true
		put(o, BucketOrch, 0, true)
		walk(o, 0, BucketOrch, noProject, !opt.Collapsed[o.SessionID], false)
	}
	// A walk marks an "away" row done only to pass through it; it is still
	// shown, in its own block.
	sorted := LaterFirst(base)
	if opt.Lift {
		var fresh []model.Session
		for _, s := range sorted {
			if lifted(s) {
				fresh = append(fresh, s)
			}
		}
		sort.SliceStable(fresh, func(i, j int) bool { return ActivityAt(fresh[i]).After(ActivityAt(fresh[j])) })
		for _, s := range fresh {
			put(s, BucketNew, 0, true)
		}
	}
	for _, s := range sorted {
		if s.Later && !lifted(s) && !fixed(s) {
			put(s, BucketLater, 0, true)
		}
	}
	at := 0
	for at < len(base) && base[at].Project != "" {
		name := base[at].Project
		bucket := ProjectBucket + name
		end := at
		for end < len(base) && base[end].Project == name {
			end++
		}
		var members []model.Session
		for _, s := range base[at:end] {
			if !IsTopOrch(s) {
				members = append(members, s)
			}
		}
		at = end
		mine := func(c model.Session) bool { return c.Project == name && !IsTopOrch(c) }
		// root places a member that hangs from nothing in this block.
		root := func(s model.Session) {
			if done[s.SessionID] {
				return
			}
			done[s.SessionID] = true
			if away(s) {
				walk(s, -1, bucket, mine, true, true)
				return
			}
			put(s, bucket, 0, true)
			walk(s, 0, bucket, mine, true, false)
		}
		var orchs []model.Session
		for _, s := range members {
			if IsProjectOrch(s) {
				orchs = append(orchs, s)
			}
		}
		oldest(orchs)
		for _, s := range orchs {
			root(s)
		}
		for _, s := range members {
			if p, ok := ix.parentOf(s); !ok || !mine(p) {
				root(s)
			}
		}
		for _, s := range members { // what a cycle of parents left over
			root(s)
		}
	}
	for ; at < len(base); at++ {
		s := base[at]
		if IsTopOrch(s) || away(s) || done[s.SessionID] {
			continue
		}
		put(s, "", 0, true)
	}
	return out
}

// InboxGroup is one group of the sessions that wait on the operator.
type InboxGroup struct {
	// Orch is the orchestrator that watches the rows (nil when none).
	Orch    *model.Session
	Project string
	Rows    []model.Session
}

// InboxGroups files the sessions that wait on the operator (NeedsOperator)
// under the orchestrator that watches them — the nearest one up the parent
// chain, else their project's orchestrator — else under their project,
// else under "". Groups: orchestrators (global first, then oldest first),
// then projects by name, then the rest; rows oldest wait first.
func InboxGroups(rows []model.Session) []InboxGroup {
	ix := NewIndex(rows)
	projOrch := map[string]model.Session{}
	for _, s := range rows {
		if IsProjectOrch(s) {
			if _, ok := projOrch[s.Project]; !ok {
				projOrch[s.Project] = s
			}
		}
	}
	watcher := func(s model.Session) (model.Session, bool) {
		seen := map[string]bool{s.SessionID: true}
		for p, ok := ix.Get(s.Parent); ok && !seen[p.SessionID]; p, ok = ix.Get(p.Parent) {
			if IsTopOrch(p) || IsProjectOrch(p) {
				return p, true
			}
			seen[p.SessionID] = true
		}
		if IsTopOrch(s) || IsProjectOrch(s) {
			return s, true
		}
		if s.Project != "" {
			o, ok := projOrch[s.Project]
			return o, ok
		}
		return model.Session{}, false
	}
	var out []*InboxGroup
	groups := map[string]*InboxGroup{}
	for _, s := range rows {
		if !NeedsOperator(s) || s.Archived {
			continue
		}
		o, watched := watcher(s)
		key := ""
		switch {
		case watched:
			key = "o:" + o.SessionID
		case s.Project != "":
			key = "p:" + s.Project
		}
		g := groups[key]
		if g == nil {
			g = &InboxGroup{Project: s.Project}
			if watched {
				g.Orch, g.Project = &o, o.Project
			}
			groups[key] = g
			out = append(out, g)
		}
		g.Rows = append(g.Rows, s)
	}
	since := func(s model.Session) time.Time {
		t := s.AttentionAt
		if s.WaitAt.After(t) {
			t = s.WaitAt
		}
		if t.IsZero() {
			t = s.LastActivity
		}
		if t.IsZero() {
			t = s.LastSeen
		}
		return t
	}
	rank := func(g *InboxGroup) int {
		switch {
		case g.Orch != nil && IsTopOrch(*g.Orch):
			return 0
		case g.Orch != nil:
			return 1
		case g.Project != "":
			return 2
		}
		return 3
	}
	for _, g := range out {
		sort.SliceStable(g.Rows, func(i, j int) bool { return since(g.Rows[i]).Before(since(g.Rows[j])) })
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if a.Orch != nil && b.Orch != nil {
			if !a.Orch.FirstSeen.Equal(b.Orch.FirstSeen) {
				return a.Orch.FirstSeen.Before(b.Orch.FirstSeen)
			}
			return a.Orch.Num < b.Orch.Num
		}
		return a.Project < b.Project
	})
	res := make([]InboxGroup, len(out))
	for i, g := range out {
		res[i] = *g
	}
	return res
}
