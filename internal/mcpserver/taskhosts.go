package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Tasks across hosts. Each machine keeps its own list (0006), so a task is
// named by its host too: "B#11" is host B's task 11 (the letter, then the
// task's "#N" — a session is "B11", without the "#"). A bare "#11" is this
// machine's #11, or — when this machine has no #11 — the one other host
// that has it. Writes go to the task's own host through the hub (the
// peer's "control" access and that device's hub allowlist), always by
// numeric id.

// hostTask is a task and the host it lives on.
type hostTask struct {
	t model.Task
	h host
}

func (ht hostTask) ref() string { return taskRefOn(ht.h, ht.t.ID) }

// id is the task's numeric id as the routes take it.
func (ht hostTask) id() string { return strconv.FormatInt(ht.t.ID, 10) }

// taskRefOn names task id on h: "B#11" with a letter, "#11" on this
// machine without one, "<host>#11" elsewhere.
func taskRefOn(h host, id int64) string {
	n := "#" + strconv.FormatInt(id, 10)
	switch {
	case h.letter != "":
		return h.letter + n
	case h.self || h.name == "":
		return n
	}
	return h.name + n
}

func sameHost(a, b host) bool { return a.self && b.self || !a.self && !b.self && a.id == b.id }

func isNumRef(ident string) bool {
	_, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(ident), "#"), 10, 64)
	return err == nil
}

func allLetters(s string) bool {
	if s == "" || len(s) > 2 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}

// taskOn reads one task of h by id or key. Another host's task is looked
// up by key through its list: a key can be a URL, which a hub path
// segment can't carry.
func (s *Server) taskOn(ctx context.Context, h host, ident string) (hostTask, error) {
	ident = strings.TrimSpace(ident)
	if !h.self && !isNumRef(ident) {
		l, err := h.st.ListTasks(ctx, true)
		if err != nil {
			return hostTask{}, h.explain(err)
		}
		for _, t := range l.Tasks {
			if t.Key == ident {
				ident = strconv.FormatInt(t.ID, 10)
				break
			}
		}
		if !isNumRef(ident) {
			return hostTask{}, fmt.Errorf("no task %q on host %s", ident, hostLabel(h))
		}
	}
	t, err := h.st.GetTask(ctx, ident)
	if err != nil {
		return hostTask{}, h.explain(err)
	}
	return hostTask{t, h}, nil
}

// locateTask resolves "B#11", "#11", "11" or a key; hostArg (the tool's
// host argument) picks the host for the ones that don't name it.
func (s *Server) locateTask(ctx context.Context, ref, hostArg string) (hostTask, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return hostTask{}, fmt.Errorf("task is required")
	}
	if prefix, rest := splitRef(ref); prefix != "" {
		h, err := s.hostNamed(ctx, prefix)
		if err == nil {
			if hostArg != "" {
				if want, err := s.hostNamed(ctx, hostArg); err != nil {
					return hostTask{}, err
				} else if !sameHost(h, want) {
					return hostTask{}, fmt.Errorf("%q is a task of host %s, but host is %q", ref, hostLabel(h), hostArg)
				}
			}
			return s.taskOn(ctx, h, rest)
		}
		if allLetters(prefix) {
			return hostTask{}, err
		}
		// Not a host: a key that happens to end in "#N".
	}
	if hostArg != "" {
		h, err := s.hostNamed(ctx, hostArg)
		if err != nil {
			return hostTask{}, err
		}
		return s.taskOn(ctx, h, ref)
	}
	self := s.selfHost(ctx)
	ht, err := s.taskOn(ctx, self, ref)
	if err == nil {
		return ht, nil
	}
	hs, _, herr := s.hostsFor(ctx, "")
	if herr != nil || len(hs) < 2 {
		return hostTask{}, err
	}
	var (
		mu    sync.Mutex
		found []hostTask
		wg    sync.WaitGroup
	)
	for _, h := range hs[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ht, err := s.taskOn(ctx, h, ref); err == nil {
				mu.Lock()
				found = append(found, ht)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return hostTask{}, fmt.Errorf("%w (not on the other hosts either; list_tasks shows refs like B#11)", err)
	}
	refs := make([]string, len(found))
	for i, f := range found {
		refs[i] = f.ref()
	}
	return hostTask{}, fmt.Errorf("this machine has no task %q and several hosts do (%s): pass one of those refs", ref, strings.Join(refs, ", "))
}

// taskListsOn reads the task lists of hs at once; a host that fails is
// reported in errs rather than failing the call (unless it is the only one).
func (s *Server) taskListsOn(ctx context.Context, hs []host, archived bool) ([]model.TaskList, map[string]string, error) {
	type res struct {
		l   model.TaskList
		err error
	}
	got := make([]res, len(hs))
	var wg sync.WaitGroup
	for i, h := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := h.st.ListTasks(ctx, archived)
			got[i] = res{l, err}
		}()
	}
	wg.Wait()
	out := make([]model.TaskList, len(hs))
	errs := map[string]string{}
	for i, r := range got {
		if r.err != nil {
			if len(hs) == 1 {
				return nil, nil, hs[i].explain(r.err)
			}
			errs[hostLabel(hs[i])] = hs[i].explain(r.err).Error()
			continue
		}
		out[i] = r.l
	}
	return out, errs, nil
}

// sessionIndex maps session ids to their rows on every reachable host, so
// a task names the sessions that worked on it wherever they ran.
func (s *Server) sessionIndex(ctx context.Context) map[string]located {
	out := map[string]located{}
	hs, _, err := s.hostsFor(ctx, "")
	if err != nil {
		return out
	}
	ls, _, err := s.sessionsOn(ctx, hs, true)
	if err != nil {
		return out
	}
	for _, l := range ls {
		out[l.x.SessionID] = l
	}
	return out
}

// tasksBySession maps session ids to the refs of the open tasks linked to
// them, on every reachable host, for the session rows.
func (s *Server) tasksBySession(ctx context.Context) map[string][]string {
	hs, _, err := s.hostsFor(ctx, "")
	if err != nil {
		return nil
	}
	ls, _, err := s.taskListsOn(ctx, hs, false)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for i, l := range ls {
		for _, t := range l.Tasks {
			if t.Finished() {
				continue
			}
			for _, sid := range t.Sessions {
				out[sid] = append(out[sid], taskRefOn(hs[i], t.ID))
			}
		}
	}
	return out
}
