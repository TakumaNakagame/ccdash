package mcpserver

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
	"github.com/TakumaNakagame/ccdash/internal/transcript"
)

// Orchestrating (docs/decisions/0009-orchestrator.md): a session that
// starts others with start_session is their parent (model.Session.Parent,
// a session id — unique across machines, so children are matched by it on
// every host). my_sessions lists the caller's children, adopt_session /
// release_session change who a session's parent is, and rows carry peer,
// the name Claude Code's SendMessage reaches a running session by.

// caller is the calling claude's session id; an error when it can't be
// found (these tools never act for another session).
func (s *Server) caller(tool string) (string, error) {
	if id := strings.TrimSpace(s.self()); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("can't tell which session is calling: %s only works for the calling session's own children (ccdash mcp must run under a claude session)", tool)
}

// peers maps this machine's running sessions to their SendMessage names;
// nil when they can't be read. Other hosts have none: SendMessage only
// reaches sessions on the caller's machine.
func (s *Server) peers(ctx context.Context) map[string]string {
	m, err := s.st.Peers(ctx)
	if err != nil {
		return nil
	}
	return m
}

func peerOf(peers map[string]string, l located) string {
	if !l.h.self || !running(l.x) {
		return ""
	}
	return peers[l.x.SessionID]
}

// parentHostFor is what a child on h records as its parent's machine: ""
// when h is this machine, else this machine's hub device id (stable; the
// letter can be reassigned), else its letter.
func (s *Server) parentHostFor(ctx context.Context, h host) string {
	if h.self {
		return ""
	}
	self := s.selfHost(ctx)
	if self.id != "" {
		return self.id
	}
	return self.letter
}

// spawnWait is how long start_session waits for a new spawn to turn into a
// session (or to stop at a dialog); spawnPoll how often it looks.
var (
	spawnWait = 10 * time.Second
	spawnPoll = 400 * time.Millisecond
)

// spawned is what became of a start_session spawn within spawnWait.
type spawned struct {
	sid    string         // the session id the PTY was aliased to; "" = not yet
	x      *model.Session // its row, once the collector has one
	dialog string         // the dialog it stopped at before having a session
	gone   bool           // the PTY is no longer listed: claude exited
}

// awaitSpawn watches PTY key on st until the spawn has a session id and a
// row, or stops at a dialog (the folder-trust question in a new folder), or
// spawnWait is over.
func (s *Server) awaitSpawn(ctx context.Context, st store.Store, key string) spawned {
	deadline := time.Now().Add(spawnWait)
	var sp spawned
	for {
		if rows, err := st.PTYs(ctx); err == nil {
			pid, found := 0, false
			for _, r := range rows {
				if r.Key == key {
					pid, found = r.PID, true
					sp.dialog = r.Prompt
				}
			}
			sp.gone = !found
			if found {
				for _, r := range rows {
					if r.PID == pid && r.Key != key && !strings.HasPrefix(r.Key, "pid-") {
						sp.sid, sp.dialog = r.Key, ""
					}
				}
			}
			switch {
			case sp.gone:
				return sp
			case sp.sid != "":
				for _, archived := range []bool{false, true} {
					ss, err := st.ListSessions(ctx, archived)
					if err != nil {
						break
					}
					if x, ok := findIn(ss, sp.sid); ok {
						sp.x = &x
						return sp
					}
				}
			case sp.dialog != "":
				return sp
			}
		}
		if !time.Now().Before(deadline) {
			return sp
		}
		select {
		case <-ctx.Done():
			return sp
		case <-time.After(spawnPoll):
		}
	}
}

// spawnResult writes what became of a spawn into start_session's result.
func (s *Server) spawnResult(ctx context.Context, h host, sp spawned, out map[string]any) {
	if sp.sid != "" {
		out["session_id"] = sp.sid
		if s.hubURL != "" {
			out["url"] = s.hubURL + "/s/" + url.PathEscape(sp.sid)
		}
	}
	switch {
	case sp.x != nil:
		l := located{*sp.x, h}
		out["ref"] = l.ref()
		out["note"] = "the session is running; the operator drives it in ccdash. my_sessions shows how it is doing"
		if p := peerOf(s.peers(ctx), l); p != "" {
			out["peer"] = p
			out["note"] = out["note"].(string) + "; peer is its name for Claude Code's SendMessage"
		}
	case sp.dialog != "":
		out["pending"] = true
		out["waiting_on"] = sp.dialog
		out["note"] = "claude stopped at a dialog before starting the session; the operator must answer it in ccdash (TUI row / portal 起動中). Folders under ccdash's 'Trusted folders' setting skip the trust question."
	case sp.gone:
		out["pending"] = true
		out["note"] = "the PTY is gone: claude exited right after starting (a wrong directory, or claude failed to launch). Nothing is running under this key"
	default:
		out["pending"] = true
		out["note"] = "claude has not registered its session yet; it appears in ccdash once it does. my_sessions lists it (as starting, then with its ref, url and peer)"
	}
}

// The my_sessions groups, in the order they are returned.
const (
	groupNeeds    = "needs_operator"
	groupWorking  = "working"
	groupReported = "reported"
	groupFinished = "finished"
)

// childGroup files a child for my_sessions: waits on the operator (an
// approval, a question, a declared answer / action), is working (mid-turn,
// or running and waiting on something in the background), has finished a
// turn the operator hasn't looked at, or is done with.
func childGroup(x model.Session) string {
	switch {
	case x.PendingCount > 0 || x.Attention == model.AttentionNeedsYou || model.WaitNeedsYou(x.Wait):
		return groupNeeds
	case x.Status == model.StatusActive || running(x) && x.Wait == model.WaitBackground:
		return groupWorking
	case x.Attention == model.AttentionDone || x.Unseen:
		return groupReported
	}
	return groupFinished
}

// changedAt is when a child last changed in a way my_sessions shows.
func changedAt(x model.Session) time.Time {
	t := x.LastSeen
	for _, o := range []time.Time{x.LastActivity, x.AttentionAt, x.WaitAt, x.FirstSeen} {
		if o.After(t) {
			t = o
		}
	}
	return t
}

// childRow is one child in my_sessions.
type childRow struct {
	Ref        string `json:"ref,omitempty"`
	Title      string `json:"title"`
	Project    string `json:"project,omitempty"`
	Host       string `json:"host,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"attention_reason,omitempty"`
	Pending    int    `json:"pending_approvals,omitempty"`
	Wait       string `json:"declared_status,omitempty"`
	WaitNote   string `json:"declared_note,omitempty"`
	Tool       string `json:"tool,omitempty"`        // the tool in flight (working)
	WorkingFor string `json:"working_for,omitempty"` // since the current turn began
	LastReply  string `json:"last_reply,omitempty"`  // the start of its latest reply
	URL        string `json:"url,omitempty"`
	Peer       string `json:"peer,omitempty"` // the name for Claude Code's SendMessage
	Age        string `json:"age,omitempty"`  // since it last did anything
	Children   int    `json:"children,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	// A spawn with no session yet.
	Key       string `json:"pty,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	WaitingOn string `json:"waiting_on,omitempty"`

	at time.Time
	l  located
}

// lastReplyLen is how much of a child's latest reply my_sessions quotes;
// finishedMax how many finished children it lists.
const (
	lastReplyLen = 300
	finishedMax  = 20
)

// lastReply is the start of the session's latest assistant text.
func lastReply(ctx context.Context, st store.Store, x model.Session) string {
	if x.TranscriptPath == "" {
		return ""
	}
	tail, err := st.TranscriptTail(ctx, x, 256<<10)
	if err != nil {
		return ""
	}
	for i := len(tail.Messages) - 1; i >= 0; i-- {
		m := tail.Messages[i]
		if m.Kind == transcript.KindAssistant && strings.TrimSpace(m.Text) != "" {
			return clip(m.Text, lastReplyLen)
		}
	}
	return ""
}

// mySessions is the my_sessions tool: the caller's children on every
// reachable host, grouped. It stamps the caller's row with the time of the
// call (model.Session.ChildrenSeenAt) — the caller's own bookmark, which is
// why --read-only still offers the tool with since_last, as it does
// set_status.
func (s *Server) mySessions(ctx context.Context, sinceLast bool) (any, error) {
	self, err := s.caller("my_sessions")
	if err != nil {
		return nil, err
	}
	hs, offline, err := s.hostsFor(ctx, "")
	if err != nil {
		return nil, err
	}
	ls, errs, err := s.sessionsOn(ctx, hs, false)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var since time.Time
	me, meErr := s.findSession(ctx, self)
	if meErr == nil {
		since = me.ChildrenSeenAt
	}
	filter := sinceLast && !since.IsZero()
	// LastSeen has whole seconds: a change in the second of the last call
	// is reported again rather than lost.
	cut := since.Truncate(time.Second)

	grand := map[string]int{}
	for _, l := range ls {
		if l.x.Parent != "" {
			grand[l.x.Parent]++
		}
	}
	peers := s.peers(ctx)
	groups := map[string][]*childRow{}
	total, unchanged := 0, 0
	for _, l := range ls {
		x := l.x
		if x.Parent != self {
			continue
		}
		total++
		at := changedAt(x)
		if filter && at.Before(cut) {
			unchanged++
			continue
		}
		r := &childRow{
			Ref: l.ref(), Title: x.DisplayTitle(), Project: x.Project, Host: l.h.name, Status: string(x.Status),
			Reason: x.AttentionReason, Pending: x.PendingCount, Wait: x.Wait, WaitNote: x.WaitNote,
			Peer: peerOf(peers, l), Children: grand[x.SessionID], SessionID: x.SessionID, at: at, l: l,
		}
		if r.Title == "" {
			r.Title = "(no prompt yet)"
		}
		if s.hubURL != "" {
			r.URL = s.hubURL + "/s/" + url.PathEscape(x.SessionID)
		}
		if !at.IsZero() {
			r.Age = ago(now.Sub(at))
		}
		g := childGroup(x)
		if g == groupWorking {
			r.Tool = x.Tool
			if !x.TurnStartedAt.IsZero() {
				r.WorkingFor = now.Sub(x.TurnStartedAt).Round(time.Second).String()
			}
		}
		groups[g] = append(groups[g], r)
	}
	for _, rows := range groups {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })
	}
	finishedMore := 0
	if f := groups[groupFinished]; len(f) > finishedMax {
		finishedMore = len(f) - finishedMax
		groups[groupFinished] = f[:finishedMax]
	}
	// The replies come from transcript tails: fetch them together.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, rows := range groups {
		for _, r := range rows {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				r.LastReply = lastReply(ctx, r.l.h.st, r.l.x)
			}()
		}
	}
	wg.Wait()

	// Spawns of the caller that have no session yet: stuck on a dialog
	// (the operator must answer it) or still starting.
	for _, h := range hs {
		rows, err := h.st.PTYs(ctx)
		if err != nil {
			continue
		}
		named := map[int]bool{}
		for _, r := range rows {
			if !strings.HasPrefix(r.Key, "pid-") {
				named[r.PID] = true
			}
		}
		for _, r := range rows {
			if !r.Alive || r.Parent != self || !strings.HasPrefix(r.Key, "pid-") || named[r.PID] {
				continue
			}
			total++
			row := &childRow{Title: "(starting)", Host: h.name, Status: "starting", Key: r.Key, Cwd: r.Cwd, WaitingOn: r.Prompt}
			if r.Prompt != "" {
				groups[groupNeeds] = append(groups[groupNeeds], row)
			} else {
				groups[groupWorking] = append(groups[groupWorking], row)
			}
		}
	}

	out := map[string]any{"children": total}
	counts := map[string]int{}
	for _, g := range []string{groupNeeds, groupWorking, groupReported, groupFinished} {
		rows := groups[g]
		if rows == nil {
			rows = []*childRow{}
		}
		out[g] = rows
		counts[g] = len(rows)
	}
	counts[groupFinished] += finishedMore
	out["counts"] = counts
	if finishedMore > 0 {
		out["finished_note"] = fmt.Sprintf("the %d most recent of %d finished children; archive_finished with children_only tidies them away", finishedMax, finishedMax+finishedMore)
	}
	if meErr == nil {
		if r := (located{x: me, h: s.selfHost(ctx)}).ref(); r != "" {
			out["parent"] = r
		}
	}
	switch {
	case filter:
		out["since"] = since.Local().Format(time.RFC3339)
		out["unchanged"] = unchanged
	case sinceLast:
		out["since_note"] = "no earlier my_sessions call is on record for this session, so every child is listed"
	}
	if total == 0 {
		out["note"] = "this session has no children: start_session makes one, adopt_session takes over an existing session"
	}
	if len(hs) > 1 {
		out["hosts"] = hostNames(hs)
	}
	noteHosts(out, offline, errs)
	if meErr == nil {
		if err := s.st.SetChildrenSeen(ctx, self, now); err != nil && sinceLast {
			out["since_note"] = "couldn't record this call, so the next since_last may repeat rows: " + err.Error()
		}
	}
	return out, nil
}

// adoptSession makes the caller the parent of one existing session, or —
// with from — of every child of another session (handing an orchestrator's
// children over to a new one). Unlike a spawn's parent, this overwrites.
func (s *Server) adoptSession(ctx context.Context, ref, from, hostArg string) (any, error) {
	self, err := s.caller("adopt_session")
	if err != nil {
		return nil, err
	}
	ref, from = strings.TrimSpace(ref), strings.TrimSpace(from)
	if (ref == "") == (from == "") {
		return nil, fmt.Errorf("pass either session (one session to adopt) or from (adopt every child of that session)")
	}
	var targets []located
	out := map[string]any{}
	if ref != "" {
		l, err := s.locate(ctx, ref, hostArg)
		if err != nil {
			return nil, err
		}
		targets = []located{l}
	} else {
		old, err := s.locate(ctx, from, hostArg)
		if err != nil {
			return nil, err
		}
		if old.x.SessionID == self {
			return nil, fmt.Errorf("%s is your own session; its children are already yours", old.ref())
		}
		hs, _, err := s.hostsFor(ctx, "")
		if err != nil {
			return nil, err
		}
		ls, _, err := s.sessionsOn(ctx, hs, true)
		if err != nil {
			return nil, err
		}
		for _, l := range ls {
			if l.x.Parent == old.x.SessionID && l.x.SessionID != self {
				targets = append(targets, l)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("%s has no children to adopt", old.ref())
		}
		out["from"] = old.ref()
	}
	me, meErr := s.findSession(ctx, self)
	for _, l := range targets {
		switch {
		case l.x.SessionID == self:
			return nil, fmt.Errorf("%s is your own session; a session can't be its own parent", l.ref())
		case meErr == nil && me.Parent == l.x.SessionID:
			return nil, fmt.Errorf("%s is your parent; adopting it would make a loop", l.ref())
		}
	}
	for _, l := range targets {
		if err := l.h.st.SetParent(ctx, l.x.SessionID, self, s.parentHostFor(ctx, l.h)); err != nil {
			return nil, l.h.explain(err)
		}
	}
	out["adopted"] = refsOfLoc(targets)
	out["note"] = "they are your children now: my_sessions lists them"
	return out, nil
}

// releaseSession clears the parent of one of the caller's children.
func (s *Server) releaseSession(ctx context.Context, ref, hostArg string) (any, error) {
	self, err := s.caller("release_session")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("session is required")
	}
	l, err := s.locate(ctx, ref, hostArg)
	if err != nil {
		return nil, err
	}
	if l.x.Parent != self {
		if l.x.Parent == "" {
			return nil, fmt.Errorf("%s has no parent; nothing to release", l.ref())
		}
		return nil, fmt.Errorf("%s is not your child (another session is its parent); only its parent can release it", l.ref())
	}
	if err := l.h.st.SetParent(ctx, l.x.SessionID, "", ""); err != nil {
		return nil, l.h.explain(err)
	}
	return map[string]any{"released": l.ref()}, nil
}

// briefInto adds what the session was asked to do at its start to a
// get_session result (hosts that don't serve it are skipped silently).
func briefInto(ctx context.Context, st store.Store, sessionID string, out map[string]any) {
	bs, err := st.Briefs(ctx, sessionID)
	if err != nil || len(bs) == 0 {
		return
	}
	const max = 2000
	text := strings.TrimSpace(bs[0].Text)
	if r := []rune(text); len(r) > max {
		text = string(r[:max])
		out["brief_truncated"] = true
	}
	out["brief"] = text
	if !bs[0].CreatedAt.IsZero() {
		out["brief_at"] = bs[0].CreatedAt.Local().Format(time.RFC3339)
	}
}
