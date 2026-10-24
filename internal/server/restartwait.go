package server

// Scheduled restarts: POST /api/restart {"when":"idle"} doesn't stop
// anything yet. The collector checks every restartCheckEvery whether any
// hosted claude is busy — working, holding an approval or a dialog on its
// screen, or declared waiting on something in the background — and once
// none has been for restartQuiet it restarts the way POST /api/restart
// always has (save, re-exec, resume each session). Nobody's turn is cut
// off; people get a warning meanwhile (GET /api/restart, carried by the
// hub to the portal and the app). A schedule that waits past its deadline
// gives up instead of forcing it. DELETE /api/restart cancels it.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Vars so tests can run the wait fast.
var (
	restartCheckEvery = 2 * time.Second
	restartQuiet      = 4 * time.Second // idle this long in a row: two clear looks
)

const (
	restartDefaultTimeout = 2 * time.Hour
	restartMaxTimeout     = 24 * time.Hour
	restartLastKeep       = 30 * time.Minute // how long "cancelled" / "expired" is reported
)

// restartReq is the body of POST /api/restart.
type restartReq struct {
	When       string `json:"when"`
	TimeoutMin int    `json:"timeout_min"`
	By         string `json:"by"`
}

// restartSchedule is the pending scheduled restart (Server.restartSched).
type restartSchedule struct {
	since, deadline time.Time
	by              string
	stop            context.CancelFunc
}

// scheduleRestart starts (or keeps) a scheduled restart and returns the
// state. Asking again while one is pending keeps the first, with the later
// deadline.
func (s *Server) scheduleRestart(req restartReq) model.RestartState {
	timeout := time.Duration(req.TimeoutMin) * time.Minute
	if timeout <= 0 {
		timeout = restartDefaultTimeout
	}
	timeout = min(timeout, restartMaxTimeout)
	by := strings.TrimSpace(req.By)
	if len(by) > 32 {
		by = by[:32]
	}
	now := time.Now()
	s.restartMu.Lock()
	if p := s.restartSched; p != nil {
		if d := now.Add(timeout); d.After(p.deadline) {
			p.deadline = d
		}
		s.restartMu.Unlock()
		return s.restartState(context.Background())
	}
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := context.WithCancel(parent)
	s.restartSched = &restartSchedule{since: now, deadline: now.Add(timeout), by: by, stop: stop}
	s.restartLast, s.restartLastAt = "", time.Time{}
	s.restartMu.Unlock()
	log.Printf("restart scheduled for when the hosted sessions are idle (by %q, gives up after %s)", by, timeout)
	go s.restartWaitLoop(ctx)
	return s.restartState(context.Background())
}

// cancelScheduledRestart drops the pending schedule; why ("cancelled",
// "expired", or "" when a restart supersedes it) is reported for a while.
func (s *Server) cancelScheduledRestart(why string) bool {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	p := s.restartSched
	if p == nil {
		return false
	}
	p.stop()
	s.restartSched = nil
	if why != "" {
		s.restartLast, s.restartLastAt = why, time.Now()
		log.Printf("scheduled restart %s", why)
	}
	return true
}

// restartWaitLoop restarts once nothing has been busy for restartQuiet.
func (s *Server) restartWaitLoop(ctx context.Context) {
	t := time.NewTicker(restartCheckEvery)
	defer t.Stop()
	var clearSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.restartMu.Lock()
		p := s.restartSched
		var deadline time.Time
		if p != nil {
			deadline = p.deadline
		}
		s.restartMu.Unlock()
		if p == nil {
			return
		}
		if time.Now().After(deadline) {
			s.cancelScheduledRestart("expired")
			return
		}
		busy, _, _ := s.restartBlockers(ctx)
		if len(busy) > 0 {
			clearSince = time.Time{}
			continue
		}
		if clearSince.IsZero() {
			clearSince = time.Now()
		}
		if time.Since(clearSince) < restartQuiet {
			continue
		}
		s.restartMu.Lock()
		if s.restartSched != p { // cancelled meanwhile
			s.restartMu.Unlock()
			return
		}
		s.restartSched = nil
		s.restartMu.Unlock()
		log.Printf("scheduled restart: the hosted sessions are idle; restarting")
		if _, err := s.restartNow(ctx); err != nil {
			log.Printf("scheduled restart: %v", err)
			s.restartMu.Lock()
			s.restartLast, s.restartLastAt = "failed", time.Now()
			s.restartMu.Unlock()
		}
		return
	}
}

// restartBlockers lists the hosted sessions a restart would cut off, and
// counts the hosted sessions (resumed afterwards) and the hosted claudes
// with no session yet (dropped: nothing to resume, no conversation yet).
func (s *Server) restartBlockers(ctx context.Context) (busy []model.RestartBlocker, hosted, unregistered int) {
	s.ptyMu.Lock()
	named := map[*ptyEntry]string{}
	for k, e := range s.ptyMap {
		if !strings.HasPrefix(k, "pid-") && e.sess.Alive() {
			named[e] = k
		}
	}
	for k, e := range s.ptyMap {
		if strings.HasPrefix(k, "pid-") && e.sess.Alive() && named[e] == "" {
			unregistered++
		}
	}
	s.ptyMu.Unlock()
	hosted = len(named)
	for e, sid := range named {
		x, ok, err := s.db.GetSession(ctx, sid)
		if err != nil || !ok {
			continue
		}
		why := ""
		switch {
		case x.Status == model.StatusActive:
			why = "作業中"
		case x.PendingCount > 0:
			why = "承認待ち"
		case x.Wait == model.WaitBackground:
			why = model.WaitLabel(x.Wait)
			if x.WaitNote != "" {
				why += ": " + x.WaitNote
			}
		default:
			// A question / menu / permission prompt on screen: the
			// transcript doesn't keep it, so --resume would lose it.
			why = screenReason(e)
		}
		if why != "" {
			busy = append(busy, model.RestartBlocker{SessionID: sid, Num: x.Num, Title: x.DisplayTitle(), Why: why})
		}
	}
	return busy, hosted, unregistered
}

// restartState is what GET /api/restart reports.
func (s *Server) restartState(ctx context.Context) model.RestartState {
	st := model.RestartState{CanRestart: s.CanRestart && s.cancelFn != nil, Waiting: []model.RestartBlocker{}}
	s.restartMu.Lock()
	if p := s.restartSched; p != nil {
		st.Pending, st.Since, st.Deadline, st.By = true, p.since, p.deadline, p.by
	}
	if s.restartLast != "" && time.Since(s.restartLastAt) < restartLastKeep {
		st.Last, st.LastAt = s.restartLast, s.restartLastAt
	}
	s.restartMu.Unlock()
	busy, hosted, unreg := s.restartBlockers(ctx)
	st.Hosted, st.Unregistered = hosted, unreg
	if st.Pending {
		st.Waiting = append(st.Waiting, busy...)
	}
	return st
}

// handleRestartState: GET /api/restart.
func (s *Server) handleRestartState(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.restartState(r.Context()))
}

// handleRestartCancel: DELETE /api/restart — cancel a scheduled restart.
func (s *Server) handleRestartCancel(w http.ResponseWriter, r *http.Request) {
	s.cancelScheduledRestart("cancelled")
	writeOK(w, s.restartState(r.Context()))
}
