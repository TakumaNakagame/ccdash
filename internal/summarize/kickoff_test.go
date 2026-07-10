package summarize

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
)

func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func insertSession(t *testing.T, d *db.DB, id, transcriptPath string) {
	t.Helper()
	if err := d.UpsertSession(context.Background(), &model.Session{
		SessionID:      id,
		Cwd:            "/tmp",
		TranscriptPath: transcriptPath,
		Status:         model.StatusIdle,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKickoffGate(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "/tmp/whatever.jsonl")

	if err := d.SetSetting(ctx, "summary_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	if err := Kickoff(ctx, d, "s1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Kickoff with summary_enabled=0 = %v, want ErrDisabled", err)
	}
	// The gate must fire BEFORE any status flip.
	sess, _, err := d.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.SummaryStatus == "running" {
		t.Fatal("gated kickoff must not flip summary_status to running")
	}
}

func TestKickoffNotFoundAndNoTranscript(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	if err := Kickoff(ctx, d, "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Kickoff(missing) = %v, want ErrSessionNotFound", err)
	}

	insertSession(t, d, "s2", "")
	if err := Kickoff(ctx, d, "s2"); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("Kickoff(no transcript) = %v, want ErrNoTranscript", err)
	}
}

// TestKickoffDedup pins the one-in-flight-summary-per-session behavior: a
// second kickoff while summary_status is "running" is a NO-OP success — it
// neither errors (local TUI and remote 202 both read it as "success, watch
// the row") nor re-flips the row / stacks a second claude -p run (which the
// synchronous SetSummaryStatus write would precede).
func TestKickoffDedup(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	insertSession(t, d, "s3", "/tmp/whatever.jsonl")
	if err := d.SetSummary(ctx, "s3", "old summary", "done"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSummaryStatus(ctx, "s3", "running"); err != nil {
		t.Fatal(err)
	}

	if err := Kickoff(ctx, d, "s3"); err != nil {
		t.Fatalf("Kickoff while running = %v, want nil (no-op)", err)
	}
	sess, _, err := d.GetSession(ctx, "s3")
	if err != nil {
		t.Fatal(err)
	}
	if sess.SummaryStatus != "running" || sess.Summary != "old summary" {
		t.Fatalf("no-op kickoff mutated the row: %+v", sess)
	}
}

// TestSweepRunningSummaries pins the startup crash-recovery: 'running' rows
// flip to 'error' (a collector restart means the goroutine died), other
// statuses stay put.
func TestSweepRunningSummaries(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	insertSession(t, d, "run", "/tmp/a.jsonl")
	insertSession(t, d, "ok", "/tmp/b.jsonl")
	if err := d.SetSummaryStatus(ctx, "run", "running"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSummary(ctx, "ok", "fine", "done"); err != nil {
		t.Fatal(err)
	}

	if err := d.SweepRunningSummaries(ctx); err != nil {
		t.Fatal(err)
	}

	got, _, err := d.GetSession(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if got.SummaryStatus != "error" {
		t.Fatalf("swept status = %q, want error", got.SummaryStatus)
	}
	if got.Summary == "" {
		t.Fatal("swept row should carry an explanatory summary text")
	}
	untouched, _, err := d.GetSession(ctx, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if untouched.SummaryStatus != "done" || untouched.Summary != "fine" {
		t.Fatalf("done row must be untouched, got %+v", untouched)
	}
}
