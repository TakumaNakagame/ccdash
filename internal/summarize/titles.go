package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/redact"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Title generation (ctrl+t): an isolated `claude -p` spawn over transcript
// digests, gated by summary_enabled (the setting's key predates the removal
// of the summary feature; it now gates only this). Several
// sessions go out in ONE claude call — a batch costs one CLI startup and
// one model round trip instead of N, and seeing the sessions side by side
// helps the model keep titles distinct.

const (
	// MaxTitleBatch caps how many sessions one ctrl+t run titles.
	MaxTitleBatch = 10
	// titleDigestTotal is the digest budget shared by every session in a
	// batch; titleDigestMax caps a single session's share. A title needs
	// far less context than a summary.
	titleDigestTotal = 60 * 1024
	titleDigestMax   = 16 * 1024
	// maxTitleWidth bounds a stored title in display cells (CJK = 2).
	maxTitleWidth = 48
)

// ErrDisabled is returned while title generation is switched off.
var ErrDisabled = errors.New("title generation is disabled (summary_enabled=off)")

// ErrNoSessions is returned when none of the requested sessions can be
// titled (unknown IDs, or no transcript recorded).
var ErrNoSessions = errors.New("no titleable sessions (unknown or no transcript)")

// KickoffTitles starts title generation for sessionIDs against d. It
// enforces summary_enabled, flips title_status to "running"
// synchronously, and runs claude in a background goroutine; results land
// via SetGenTitle (per session) or title_status "error". Sessions already
// running or without a transcript are skipped; IDs past MaxTitleBatch are
// dropped. Shared by store.Local and the server's POST /api/titles.
func KickoffTitles(ctx context.Context, d *db.DB, sessionIDs []string) error {
	cfg, err := settings.Load(ctx, d)
	if err != nil {
		return err
	}
	if !cfg.SummaryEnabled {
		return ErrDisabled
	}
	var batch []model.Session
	seen := map[string]bool{}
	for _, id := range sessionIDs {
		if seen[id] || len(batch) == MaxTitleBatch {
			continue
		}
		seen[id] = true
		s, ok, err := d.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if !ok || s.TranscriptPath == "" || s.TitleStatus == "running" {
			continue
		}
		batch = append(batch, s)
	}
	if len(batch) == 0 {
		return ErrNoSessions
	}
	ids := make([]string, len(batch))
	for i, s := range batch {
		ids[i] = s.SessionID
	}
	if err := d.SetTitleStatus(ctx, ids, "running"); err != nil {
		return err
	}
	timeoutSec := cfg.SummaryTimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		titles, err := RunTitles(bg, batch)
		if err != nil {
			log.Printf("titles: %v", err)
		}
		for _, s := range batch {
			t, ok := titles[s.Num]
			if err != nil || !ok {
				_ = d.SetTitleStatus(context.Background(), []string{s.SessionID}, "error")
				continue
			}
			_ = d.SetGenTitle(context.Background(), s.SessionID, t)
		}
	}()
	return nil
}

// RunTitles asks claude for one short title per session and returns them
// keyed by Session.Num. Sessions whose transcript can't be read are left
// out of the prompt (and so out of the result).
func RunTitles(ctx context.Context, sessions []model.Session) (map[int64]string, error) {
	per := titleDigestTotal / max(len(sessions), 1)
	per = min(per, titleDigestMax)
	var in strings.Builder
	n := 0
	for _, s := range sessions {
		msgs, err := transcript.Load(s.TranscriptPath)
		if err != nil {
			continue
		}
		digest := strings.TrimSpace(redact.String(buildDigest(msgs, per)))
		if digest == "" {
			continue
		}
		fmt.Fprintf(&in, "=== SESSION %d (cwd: %s) ===\n%s\n\n", s.Num, s.Cwd, digest)
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("transcripts are empty")
	}

	instruction := Marker + ` Below are transcripts of one or more Claude Code sessions,
each headed "=== SESSION <id> ===". Give each session a short title that says
what the work is about, like a commit subject or a ticket name — e.g.
"ccdash 改善: タイトル自動生成" or "Fix flaky login test". Rules:
- At most ~30 characters (~15 for CJK). Name the project or area when it helps.
- Same language as the user's prompts in that session.
- No quotes, no trailing punctuation, no "#" or ids, single line.
Reply with ONLY a JSON array, no prose, no code fence:
[{"id": <session id number>, "title": "<title>"}]`

	cmd := exec.CommandContext(ctx,
		"claude",
		"--setting-sources", "project",
		"-p", instruction,
	)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(in.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		hint := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(hint, '\n'); i > 0 {
			hint = hint[:i]
		}
		if hint == "" {
			return nil, fmt.Errorf("claude -p failed: %w", err)
		}
		return nil, fmt.Errorf("claude -p failed: %w (%s)", err, hint)
	}
	return parseTitles(stdout.String())
}

// parseTitles extracts the JSON array from claude's reply — tolerating a
// stray code fence or sentence around it — and cleans each title.
func parseTitles(out string) (map[int64]string, error) {
	i, j := strings.IndexByte(out, '['), strings.LastIndexByte(out, ']')
	if i < 0 || j < i {
		return nil, fmt.Errorf("no JSON array in reply: %q", truncate(out, 120))
	}
	var items []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &items); err != nil {
		return nil, fmt.Errorf("parse reply: %w", err)
	}
	res := map[int64]string{}
	for _, it := range items {
		if t := cleanTitle(it.Title); t != "" && it.ID > 0 {
			res[it.ID] = t
		}
	}
	return res, nil
}

// cleanTitle flattens to one line, strips wrapping quotes / a leading
// "#N", redacts, and bounds the width.
func cleanTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	t = strings.Trim(t, "\"'`「」 ")
	if strings.HasPrefix(t, "#") {
		if k := strings.IndexByte(t, ' '); k > 0 {
			t = strings.TrimSpace(t[k+1:])
		}
	}
	t = redact.String(t)
	if runewidth.StringWidth(t) > maxTitleWidth {
		t = runewidth.Truncate(t, maxTitleWidth, "…")
	}
	return t
}
