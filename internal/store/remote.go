package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Remote implements Store against a ccdash collector's HTTP API
// (internal/server's /api/... routes), for `ccdash --remote http://host:port`.
// Every request carries the shared token via X-Ccdash-Token, the same
// header the hook + approval-decide requests already use.
type Remote struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewRemote builds a Remote store. baseURL is the collector's origin, e.g.
// "http://192.168.20.132:9123" (no trailing slash required).
func NewRemote(baseURL, token string) *Remote {
	return &Remote{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// Belt-and-suspenders cap on top of the per-call context timeouts
		// below, in case a caller passes a context with no deadline at all.
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// mutationTimeout caps small write requests (archive/favorite/title/group/
// settings). Some of these run synchronously inside Bubble Tea's Update
// (the settings page mutates via settings.Set inline), so a dead collector
// must not freeze the UI longer than this. Reads and the summarize kick
// keep their own longer budgets.
const mutationTimeout = 3 * time.Second

// transcriptEnvelope is the wire shape for GET
// /api/sessions/{id}/transcript — see internal/server's handler for the
// producing side.
type transcriptEnvelope struct {
	Mtime string `json:"mtime"`
	Size  int64  `json:"size"`
	Data  string `json:"data,omitempty"` // base64-encoded raw JSONL bytes
}

// doJSON issues a request, enforces timeout via ctx, and (for 2xx responses)
// decodes the body into out when out is non-nil. Non-2xx responses become an
// error carrying the response body (truncated) so callers get an actionable
// message instead of a bare status code.
func (r *Remote) doJSON(ctx context.Context, timeout time.Duration, method, path string, query url.Values, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u := r.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderName, r.token)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (r *Remote) ListSessions(ctx context.Context, archived bool) ([]model.Session, error) {
	q := url.Values{"archived": {"0"}}
	if archived {
		q.Set("archived", "1")
	}
	var out []model.Session
	if err := r.doJSON(ctx, 5*time.Second, http.MethodGet, "/api/sessions", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Remote) ListPendingApprovals(ctx context.Context) ([]model.Approval, error) {
	var out []model.Approval
	if err := r.doJSON(ctx, 5*time.Second, http.MethodGet, "/api/approvals", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Remote) SetArchived(ctx context.Context, sessionID string, v bool) error {
	return r.doJSON(ctx, mutationTimeout, http.MethodPost, sessionPath(sessionID, "archive"), nil,
		map[string]any{"archived": v}, nil)
}

func (r *Remote) SetFavorite(ctx context.Context, sessionID string, v bool) error {
	return r.doJSON(ctx, mutationTimeout, http.MethodPost, sessionPath(sessionID, "favorite"), nil,
		map[string]any{"favorite": v}, nil)
}

func (r *Remote) SetCustomTitle(ctx context.Context, sessionID, title string) error {
	return r.doJSON(ctx, mutationTimeout, http.MethodPost, sessionPath(sessionID, "title"), nil,
		map[string]any{"title": title}, nil)
}

func (r *Remote) SetUserGroup(ctx context.Context, sessionID, group string) error {
	return r.doJSON(ctx, mutationTimeout, http.MethodPost, sessionPath(sessionID, "group"), nil,
		map[string]any{"group": group}, nil)
}

// DecideApproval hits the same /approvals/{id}/decide route the embedded
// collector has always exposed — it isn't under /api because it predates
// remote mode and Local uses the identical route against its own loopback
// collector (see store.Local.DecideApproval).
func (r *Remote) DecideApproval(ctx context.Context, id int64, behavior, reason string, keep bool) error {
	path := fmt.Sprintf("/approvals/%d/decide", id)
	return r.doJSON(ctx, 5*time.Second, http.MethodPost, path, nil,
		map[string]any{"behavior": behavior, "reason": reason, "keep": keep}, nil)
}

func (r *Remote) GetSetting(ctx context.Context, key string) (string, error) {
	all, err := r.AllSettings(ctx)
	if err != nil {
		return "", err
	}
	return all[key], nil
}

// SetSetting runs synchronously inside Bubble Tea's Update on the settings
// page, so it uses the shorter mutation timeout — see mutationTimeout.
func (r *Remote) SetSetting(ctx context.Context, key, value string) error {
	path := "/api/settings/" + url.PathEscape(key)
	return r.doJSON(ctx, mutationTimeout, http.MethodPut, path, nil, map[string]string{"value": value}, nil)
}

// AllSettings fetches the collector's whole settings map in one GET —
// settings.Load calls this once at startup instead of one GetSetting round
// trip per key.
func (r *Remote) AllSettings(ctx context.Context) (map[string]string, error) {
	var all map[string]string
	if err := r.doJSON(ctx, 5*time.Second, http.MethodGet, "/api/settings", nil, nil, &all); err != nil {
		return nil, err
	}
	return all, nil
}

// Summarize only kicks off the server-side flow; the 10s budget covers the
// round trip to start it (status flip to "running"), not the summarization
// itself, which the server runs in its own background goroutine and reports
// back through the session row (summary/summary_status), same as Local.
func (r *Remote) Summarize(ctx context.Context, sessionID string) error {
	return r.doJSON(ctx, 10*time.Second, http.MethodPost, sessionPath(sessionID, "summarize"), nil, nil, nil)
}

func (r *Remote) TranscriptStat(ctx context.Context, s model.Session) (time.Time, int64, error) {
	q := url.Values{"mode": {"stat"}}
	var env transcriptEnvelope
	if err := r.doJSON(ctx, 5*time.Second, http.MethodGet, sessionPath(s.SessionID, "transcript"), q, nil, &env); err != nil {
		return time.Time{}, 0, err
	}
	mtime, _ := time.Parse(time.RFC3339Nano, env.Mtime)
	return mtime, env.Size, nil
}

func (r *Remote) TranscriptTail(ctx context.Context, s model.Session, budgetBytes int) (TailResult, error) {
	q := url.Values{"mode": {"tail"}, "bytes": {strconv.Itoa(budgetBytes)}}
	var env transcriptEnvelope
	if err := r.doJSON(ctx, 10*time.Second, http.MethodGet, sessionPath(s.SessionID, "transcript"), q, nil, &env); err != nil {
		return TailResult{}, err
	}
	data, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return TailResult{}, fmt.Errorf("decode transcript data: %w", err)
	}
	mtime, _ := time.Parse(time.RFC3339Nano, env.Mtime)
	// The server already dropped the boundary-fragment line (it did the
	// seeking), so this is a plain parse — no re-trimming needed.
	return TailResult{Messages: transcript.ParseBytes(data), Mtime: mtime, Size: env.Size}, nil
}

func (r *Remote) TranscriptFull(ctx context.Context, s model.Session) ([]transcript.Message, error) {
	q := url.Values{"mode": {"full"}}
	var env transcriptEnvelope
	if err := r.doJSON(ctx, 20*time.Second, http.MethodGet, sessionPath(s.SessionID, "transcript"), q, nil, &env); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, fmt.Errorf("decode transcript data: %w", err)
	}
	return transcript.ParseBytes(data), nil
}

func sessionPath(sessionID, suffix string) string {
	return "/api/sessions/" + url.PathEscape(sessionID) + "/" + suffix
}
