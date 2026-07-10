package summarize

import (
	"context"
	"errors"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/settings"
)

// Sentinel errors so callers can map failures to their own surface:
// store.Local returns them verbatim to the TUI; the server's
// POST /api/sessions/{id}/summarize handler maps them to 403 / 404 / 422.
var (
	ErrDisabled        = errors.New("summarize is disabled (summary_enabled=off)")
	ErrSessionNotFound = errors.New("session not found")
	ErrNoTranscript    = errors.New("no transcript path recorded for this session")
)

// Kickoff starts the summary flow for sessionID against d: it enforces the
// summary_enabled gate, flips summary_status to "running" synchronously (so
// the very next sessions poll shows the in-progress spinner), and runs
// `claude -p` in a background goroutine whose result lands via SetSummary.
// This is the ONE implementation shared by store.Local.Summarize and the
// server's summarize API handler — they were diverging copies before
// (the server checked the gate, Local didn't).
//
// If a summary is already running for the session, Kickoff is a no-op that
// returns nil — both the local TUI and a remote client (via the server's
// 202) see the same "success, watch the row" behavior instead of stacking a
// second claude -p run.
func Kickoff(ctx context.Context, d *db.DB, sessionID string) error {
	cfg, err := settings.Load(ctx, d)
	if err != nil {
		return err
	}
	if !cfg.SummaryEnabled {
		return ErrDisabled
	}
	sess, ok, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSessionNotFound
	}
	if sess.TranscriptPath == "" {
		return ErrNoTranscript
	}
	if sess.SummaryStatus == "running" {
		// Dedup: one in-flight summary per session. No-op success.
		return nil
	}
	if err := d.SetSummaryStatus(ctx, sessionID, "running"); err != nil {
		return err
	}
	timeoutSec := cfg.SummaryTimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	path := sess.TranscriptPath
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		summary, err := Run(bg, path)
		if err != nil {
			_ = d.SetSummary(context.Background(), sessionID, err.Error(), "error")
			return
		}
		_ = d.SetSummary(context.Background(), sessionID, summary, "done")
	}()
	return nil
}
