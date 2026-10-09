package summarize

import (
	"context"
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
