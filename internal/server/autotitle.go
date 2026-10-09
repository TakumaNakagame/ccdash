package server

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/discovery"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/summarize"
)

// Automatic titles: once a session has had an exchange or two, the
// collector titles it with the same batched `claude -p` run as ctrl+t —
// once per session (a done or failed attempt is not retried; ctrl+t still
// can be).
const (
	// autoTitleQuiet: a session with a single prompt qualifies once it
	// has been quiet this long (the first turn is over).
	autoTitleQuiet = 2 * time.Minute
	// autoTitleWindow: only sessions active this recently are considered,
	// so the first run after an upgrade doesn't title the whole history.
	autoTitleWindow = 24 * time.Hour
	// autoTitleEvery spaces out kickoffs (each is one claude -p call).
	autoTitleEvery = time.Minute
)

// titleSignal is what discovery saw of a transcript.
type titleSignal struct {
	prompts      int
	lastModified time.Time
}

type autoTitler struct {
	mu      sync.Mutex
	signals map[string]titleSignal
	lastRun time.Time
}

func (a *autoTitler) note(d discovery.Discovered) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signals == nil {
		a.signals = map[string]titleSignal{}
	}
	a.signals[d.SessionID] = titleSignal{prompts: d.Prompts, lastModified: d.LastModified}
}

// ready reports whether a session has had enough of a conversation.
func (a *autoTitler) ready(sessionID string, now time.Time) bool {
	a.mu.Lock()
	sig, ok := a.signals[sessionID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	return sig.prompts >= discovery.PromptCap ||
		sig.prompts >= 1 && now.Sub(sig.lastModified) >= autoTitleQuiet
}

// maybeAutoTitle starts one title batch for the sessions that are ready
// and have never been titled, at most every autoTitleEvery.
func (s *Server) maybeAutoTitle(ctx context.Context, now time.Time) {
	s.titler.mu.Lock()
	due := now.Sub(s.titler.lastRun) >= autoTitleEvery
	s.titler.mu.Unlock()
	if !due {
		return
	}
	cfg, err := settings.Load(ctx, s.db)
	if err != nil || !cfg.AutoTitle || !cfg.SummaryEnabled {
		return
	}
	ss, err := s.db.ListSessions(ctx, false)
	if err != nil {
		log.Printf("auto-title: %v", err)
		return
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].LastSeen.After(ss[j].LastSeen) })
	var ids []string
	for _, x := range ss {
		if len(ids) == summarize.MaxTitleBatch {
			break
		}
		if x.Num <= 0 || x.TranscriptPath == "" || x.GenTitle != "" || x.CustomTitle != "" ||
			x.TitleStatus != "" || now.Sub(x.LastSeen) > autoTitleWindow || !s.titler.ready(x.SessionID, now) {
			continue
		}
		ids = append(ids, x.SessionID)
	}
	if len(ids) == 0 {
		return
	}
	s.titler.mu.Lock()
	s.titler.lastRun = now
	s.titler.mu.Unlock()
	if err := summarize.KickoffTitles(ctx, s.db, ids); err != nil && !errors.Is(err, summarize.ErrNoSessions) {
		log.Printf("auto-title: %v", err)
		return
	}
	log.Printf("auto-title: titling %d sessions", len(ids))
}
