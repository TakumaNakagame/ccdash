package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestMessages: the inbox keeps messages in order, hands back the
// undelivered ones, stamps a delivery once, and all of it is still there
// when the database is opened again.
func TestMessages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range []model.Message{
		{SessionID: "kid", FromSession: "boss", FromRef: "A1", FromHost: "laptop", Text: "一つ目"},
		{SessionID: "kid", FromSession: "boss", FromRef: "A1", FromHost: "laptop", Text: "二つ目"},
		{SessionID: "boss", FromSession: "kid", FromRef: "#2", Text: "終わりました"},
	} {
		id, err := d.InsertMessage(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := d.MarkMessageDelivered(ctx, ids[0], model.MessageTyped); err != nil {
		t.Fatal(err)
	}
	first, _, _ := d.Message(ctx, ids[0])
	// A second stamp changes nothing.
	_ = d.MarkMessageDelivered(ctx, ids[0], model.MessageResumed)
	if m, ok, _ := d.Message(ctx, ids[0]); !ok || m.How != model.MessageTyped || m.DeliveredAt.IsZero() || !m.DeliveredAt.Equal(first.DeliveredAt) {
		t.Errorf("delivered message = %+v", m)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(path) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pend, err := d.PendingMessages(ctx)
	if err != nil || len(pend) != 2 || pend[0].ID != ids[1] || pend[1].ID != ids[2] {
		t.Fatalf("pending after reopen = %+v (%v)", pend, err)
	}
	if m := pend[0]; m.Text != "二つ目" || m.FromSession != "boss" || m.FromRef != "A1" || m.FromHost != "laptop" || m.CreatedAt.IsZero() || !m.DeliveredAt.IsZero() || m.How != "" {
		t.Errorf("queued message = %+v", m)
	}
	all, _ := d.SessionMessages(ctx, "kid")
	if len(all) != 2 || all[0].ID != ids[0] || all[0].How != model.MessageTyped || all[1].ID != ids[1] {
		t.Errorf("kid's messages = %+v", all)
	}
	if _, ok, _ := d.Message(ctx, 999); ok {
		t.Error("an unknown id was found")
	}
}
