package mcpserver

import (
	"context"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Orchestrator roles in ccdash mcp (docs/decisions/0009-orchestrator.md §7,
// §10). The server never gives or takes a role — only people do — it reads
// its own session's role and behaves accordingly:
//
//   - the instructions carry the role's rules (instructions.go);
//   - list_projects names each project's orchestrator;
//   - a global orchestrator's start_session for a project that has a
//     running orchestrator hands back that orchestrator instead of starting
//     (direct: true starts anyway);
//   - tell_session also carries messages between the global orchestrator
//     and a project's orchestrator;
//   - my_sessions adds the project's other sessions for a project
//     orchestrator, and a summary per project orchestrator for the global
//     one.

// envRole is the role the collector started this claude with: CCDASH_ROLE
// and, for a project's orchestrator, CCDASH_ROLE_PROJECT. Present from the
// very first start, when the session has no row to read a role from yet.
func envRole() (role, project string) {
	role = strings.TrimSpace(os.Getenv("CCDASH_ROLE"))
	if !model.IsOrchestrator(role) {
		return "", ""
	}
	if role == model.RoleProjectOrchestrator {
		project = strings.TrimSpace(os.Getenv("CCDASH_ROLE_PROJECT"))
	}
	return role, project
}

// myRole is the calling session's role and, for a project's orchestrator,
// its project: what the collector put in the environment, else the row.
func (s *Server) myRole(ctx context.Context) (role, project string) {
	if s.roleEnv != nil {
		if role, project = s.roleEnv(); role != "" {
			return role, project
		}
	}
	self := strings.TrimSpace(s.self())
	if self == "" {
		return "", ""
	}
	me, err := s.findSession(ctx, self)
	if err != nil || !model.IsOrchestrator(me.Role) {
		return "", ""
	}
	if me.Role == model.RoleProjectOrchestrator {
		return me.Role, me.Project
	}
	return me.Role, ""
}

// orchRef names an orchestrator for another session: who it is, where to
// read it and how to send it a message.
type orchRef struct {
	Ref     string `json:"ref,omitempty"`
	Project string `json:"project,omitempty"`
	Host    string `json:"host,omitempty"`
	Status  string `json:"status,omitempty"`
	URL     string `json:"url,omitempty"`
	Peer    string `json:"peer,omitempty"`
	// Reach is how to message it: "SendMessage:<peer>" while it runs on
	// this machine, else "tell_session" (which also wakes a stopped one).
	Reach     string `json:"reach,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

func (s *Server) orchRefOf(l located, peers map[string]string) *orchRef {
	o := &orchRef{Ref: l.ref(), Project: l.x.Project, Host: l.h.name, Status: string(l.x.Status), SessionID: l.x.SessionID, Peer: peerOf(peers, l)}
	o.Reach = reachOf(o.Peer)
	if s.hubURL != "" {
		o.URL = s.hubURL + "/s/" + url.PathEscape(l.x.SessionID)
	}
	return o
}

// byRecent orders sessions for "which one do I mean": running first, this
// machine before the others, then the most recently active.
func byRecent(ls []located) {
	sort.SliceStable(ls, func(i, j int) bool {
		a, b := ls[i], ls[j]
		if ra, rb := running(a.x), running(b.x); ra != rb {
			return ra
		}
		if a.h.self != b.h.self {
			return a.h.self
		}
		return changedAt(a.x).After(changedAt(b.x))
	})
}

// projectOrchestrators finds the sessions with the project-orchestrator
// role for project ("" = every project) on every reachable host, the best
// candidate first.
func (s *Server) projectOrchestrators(ctx context.Context, project string) []located {
	hs, _, err := s.hostsFor(ctx, "")
	if err != nil {
		return nil
	}
	ls, _, err := s.sessionsOn(ctx, hs, false)
	if err != nil {
		return nil
	}
	var out []located
	for _, l := range ls {
		if l.x.Role == model.RoleProjectOrchestrator && l.x.Project != "" && (project == "" || l.x.Project == project) {
			out = append(out, l)
		}
	}
	byRecent(out)
	return out
}

// globalOrchestrator finds the session a project's orchestrator means by
// "orchestrator": its own parent when that is one, else the best candidate
// among the reachable hosts' global orchestrators.
func (s *Server) globalOrchestrator(ctx context.Context, me model.Session) (located, bool) {
	hs, _, err := s.hostsFor(ctx, "")
	if err != nil {
		return located{}, false
	}
	ls, _, err := s.sessionsOn(ctx, hs, false)
	if err != nil {
		return located{}, false
	}
	var out []located
	for _, l := range ls {
		if l.x.Role != model.RoleOrchestrator || l.x.SessionID == me.SessionID {
			continue
		}
		if l.x.SessionID == me.Parent {
			return l, true
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return located{}, false
	}
	byRecent(out)
	return out[0], true
}

// routeToProject is what a global orchestrator's start_session answers
// instead of starting, when project has a running orchestrator of its own;
// nil when it should start as usual. A stopped one is only mentioned
// (stopped != nil): the caller starts the worker itself.
func (s *Server) routeToProject(ctx context.Context, project string) (routed map[string]any, stopped *orchRef) {
	if project == "" {
		return nil, nil
	}
	if role, _ := s.myRole(ctx); role != model.RoleOrchestrator {
		return nil, nil
	}
	self := strings.TrimSpace(s.self())
	peers := s.peers(ctx)
	for _, l := range s.projectOrchestrators(ctx, project) {
		if l.x.SessionID == self {
			continue
		}
		o := s.orchRefOf(l, peers)
		if !running(l.x) {
			if stopped == nil {
				stopped = o
			}
			continue
		}
		how := "tell_session(session: \"" + o.Ref + "\", …)"
		if o.Peer != "" {
			how = "Claude Code's SendMessage to " + o.Peer + " (or tell_session)"
		}
		return map[string]any{
			"routed":               true,
			"project":              project,
			"project_orchestrator": o,
			"note": "nothing was started: project " + project + " has its own orchestrator. Hand this request to it with " + how +
				" — it knows the project and starts the worker itself — and give the operator its ref and url. Pass direct: true to start the session yourself instead (when the operator asked for that)",
		}, nil
	}
	return nil, stopped
}

// orchestratorSummaries is what my_sessions adds for a global
// orchestrator: one row per project orchestrator with the count of its
// children by group and the ones that wait on the operator.
func (s *Server) orchestratorSummaries(ls []located, self string, peers map[string]string, shown, now time.Time) []map[string]any {
	var orchs []located
	for _, l := range ls {
		if l.x.Role == model.RoleProjectOrchestrator && l.x.SessionID != self {
			orchs = append(orchs, l)
		}
	}
	sort.SliceStable(orchs, func(i, j int) bool { return orchs[i].x.Project < orchs[j].x.Project })
	out := []map[string]any{}
	for _, o := range orchs {
		counts := map[string]int{groupNeeds: 0, groupWorking: 0, groupReported: 0, groupFinished: 0}
		needs := []*childRow{}
		for _, l := range ls {
			if l.x.Parent != o.x.SessionID {
				continue
			}
			g := childGroup(l.x, shown)
			counts[g]++
			if g == groupNeeds {
				needs = append(needs, s.childRowOf(l, peers, nil, now))
			}
		}
		row := map[string]any{"orchestrator": s.orchRefOf(o, peers), "project": o.x.Project, "children": counts, groupNeeds: needs}
		if g := childGroup(o.x, shown); g == groupNeeds {
			row["orchestrator_needs_operator"] = o.x.AttentionReason
		}
		out = append(out, row)
	}
	return out
}
