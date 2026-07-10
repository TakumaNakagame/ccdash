package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/summarize"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// defaultSummaryTimeoutSec mirrors settings.Defaults().SummaryTimeoutSec —
// duplicated here (rather than importing internal/settings) to avoid a
// store↔settings import cycle: internal/settings.Load/Set take a Store.
const defaultSummaryTimeoutSec = 180

// Local implements Store directly against the embedded/managed collector's
// SQLite DB and the on-disk transcript files. This is ccdash's original
// same-host behavior, predating remote mode.
type Local struct {
	db     *db.DB
	client *http.Client
}

// NewLocal wraps d. The returned Store's DecideApproval reaches the
// collector on 127.0.0.1:paths.DefaultPort — the same loopback address the
// embedded or `-k`/`ccdash server` collector listens on.
func NewLocal(d *db.DB) *Local {
	return &Local{db: d, client: &http.Client{Timeout: 5 * time.Second}}
}

func (l *Local) ListSessions(ctx context.Context, archived bool) ([]model.Session, error) {
	return l.db.ListSessions(ctx, archived)
}

func (l *Local) ListPendingApprovals(ctx context.Context) ([]model.Approval, error) {
	return l.db.ListPendingApprovals(ctx)
}

func (l *Local) SetArchived(ctx context.Context, sessionID string, v bool) error {
	return l.db.SetArchived(ctx, sessionID, v)
}

func (l *Local) SetFavorite(ctx context.Context, sessionID string, v bool) error {
	return l.db.SetFavorite(ctx, sessionID, v)
}

func (l *Local) SetCustomTitle(ctx context.Context, sessionID, title string) error {
	return l.db.SetCustomTitle(ctx, sessionID, title)
}

func (l *Local) SetUserGroup(ctx context.Context, sessionID, group string) error {
	return l.db.SetUserGroup(ctx, sessionID, group)
}

func (l *Local) GetSetting(ctx context.Context, key string) (string, error) {
	return l.db.GetSetting(ctx, key)
}

func (l *Local) SetSetting(ctx context.Context, key, value string) error {
	return l.db.SetSetting(ctx, key, value)
}

// DecideApproval POSTs to the collector's own /approvals/{id}/decide route,
// exactly like the TUI did directly before Store existed. It cannot write
// approvals.status straight into SQLite because the thing actually blocked
// is a goroutine sitting on a channel in server.Server.pending; only the
// HTTP route can reach it.
func (l *Local) DecideApproval(ctx context.Context, id int64, behavior, reason string, keep bool) error {
	body, err := json.Marshal(map[string]any{"behavior": behavior, "reason": reason, "keep": keep})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("http://%s:%d/approvals/%d/decide", paths.DefaultHost, paths.DefaultPort, id)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok, err := auth.Load(); err == nil {
		req.Header.Set(auth.HeaderName, tok)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Summarize flips summary_status to "running" synchronously (so the very
// next ListSessions poll shows progress) and runs `claude -p` in a
// background goroutine; the result lands via SetSummary when it completes.
func (l *Local) Summarize(ctx context.Context, sessionID string) error {
	sess, ok, err := l.db.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if sess.TranscriptPath == "" {
		return fmt.Errorf("no transcript path recorded for this session")
	}
	if err := l.db.SetSummaryStatus(ctx, sessionID, "running"); err != nil {
		return err
	}
	timeoutSec := defaultSummaryTimeoutSec
	if v, err := l.db.GetSetting(ctx, "summary_timeout_sec"); err == nil && v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil && n > 0 {
			timeoutSec = n
		}
	}
	path := sess.TranscriptPath
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		summary, err := summarize.Run(bg, path)
		if err != nil {
			_ = l.db.SetSummary(context.Background(), sessionID, err.Error(), "error")
			return
		}
		_ = l.db.SetSummary(context.Background(), sessionID, summary, "done")
	}()
	return nil
}

func (l *Local) TranscriptStat(ctx context.Context, s model.Session) (time.Time, int64, error) {
	if s.TranscriptPath == "" {
		return time.Time{}, 0, fmt.Errorf("no transcript path recorded for this session")
	}
	fi, err := os.Stat(s.TranscriptPath)
	if err != nil {
		return time.Time{}, 0, err
	}
	return fi.ModTime(), fi.Size(), nil
}

func (l *Local) TranscriptTail(ctx context.Context, s model.Session, budgetBytes int) (TailResult, error) {
	if s.TranscriptPath == "" {
		return TailResult{}, fmt.Errorf("no transcript path recorded for this session")
	}
	data, mtime, size, err := transcript.TailBytes(s.TranscriptPath, int64(budgetBytes))
	if err != nil {
		return TailResult{}, err
	}
	return TailResult{Messages: transcript.ParseBytes(data), Mtime: mtime, Size: size}, nil
}

func (l *Local) TranscriptFull(ctx context.Context, s model.Session) ([]transcript.Message, error) {
	if s.TranscriptPath == "" {
		return nil, fmt.Errorf("no transcript path recorded for this session")
	}
	return transcript.Load(s.TranscriptPath)
}
