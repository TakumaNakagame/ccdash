package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

func TestDeviceLetters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := st.createDevice(ctx, "pc")
	b, _, _ := st.createDevice(ctx, "lxc")
	if a.Letter != "A" || b.Letter != "B" {
		t.Fatalf("letters %q %q", a.Letter, b.Letter)
	}
	// Devices from before letters get one in registration order.
	_, _ = st.db.Exec(`UPDATE devices SET letter = ''`)
	_, _ = st.db.Exec(`UPDATE devices SET created_at = ? WHERE id = ?`, time.Now().Add(time.Hour).Format(time.RFC3339Nano), a.ID)
	if err := st.backfillLetters(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.listDevices(ctx)
	got := map[string]string{}
	for _, d := range ds {
		got[d.Name] = d.Letter
	}
	if got["lxc"] != "A" || got["pc"] != "B" {
		t.Fatalf("backfill %v", got)
	}
	if err := st.setLetter(ctx, a.ID, "A"); err != ErrDuplicateLetter {
		t.Fatalf("duplicate letter: %v", err)
	}
	if err := st.setLetter(ctx, a.ID, "C"); err != nil {
		t.Fatal(err)
	}
	c, _, _ := st.createDevice(ctx, "third")
	if c.Letter != "B" { // the first free one
		t.Fatalf("next letter %q", c.Letter)
	}
	used := map[string]bool{}
	for i := 0; i < 26; i++ {
		used[string(rune('A'+i))] = true
	}
	if l := nextLetter(used); l != "AA" {
		t.Fatalf("after Z: %q", l)
	}
	_ = st.Close()
}

func TestSetLetterRoute(t *testing.T) {
	h := newTestHub(t, true)
	d, _, _ := h.st.createDevice(context.Background(), "x")
	for body, want := range map[string]int{`{"letter":"1"}`: 400, `{"letter":"abc"}`: 400, `{"letter":"q"}`: 204} {
		if w := serve(h.Handler(), "POST", "/api/devices/"+d.ID+"/letter", body, nil); w.Code != want {
			t.Errorf("%s = %d, want %d", body, w.Code, want)
		}
	}
	ds, _ := h.st.listDevices(context.Background())
	if ds[0].Letter != "Q" {
		t.Errorf("stored %q", ds[0].Letter)
	}
	if w := serve(h.Handler(), "POST", "/api/devices/"+d.ID+"/letter", `{"letter":"Z"}`, map[string]string{"Authorization": "Bearer ccdh_x"}); w.Code != http.StatusUnauthorized {
		t.Errorf("peer may not set letters: %d", w.Code)
	}
}

// The dial's answer tells the device its letter and name.
func TestAgentDialCarriesLetter(t *testing.T) {
	h := newTestHub(t, true)
	d, tok, _ := h.st.createDevice(context.Background(), "pc")
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, rh, err := tunnel.DialHeader(ctx, srv.URL, tok, http.Header{"Host": {"hub.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if rh.Get(tunnel.HeaderDeviceLetter) != d.Letter || rh.Get(tunnel.HeaderDeviceName) != "pc" {
		t.Errorf("headers %v", rh)
	}
}

func TestK8sStoreLetters(t *testing.T) {
	ctx := context.Background()
	_, api := newFakeAPI(t)
	s, err := openK8sStore(ctx, api, "state", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _, _ := s.createDevice(ctx, "pc")
	b, _, _ := s.createDevice(ctx, "lxc")
	if a.Letter != "A" || b.Letter != "B" {
		t.Fatalf("letters %q %q", a.Letter, b.Letter)
	}
	if err := s.setLetter(ctx, b.ID, "A"); err != ErrDuplicateLetter {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.setLetter(ctx, b.ID, "L"); err != nil {
		t.Fatal(err)
	}
	// A secret from before letters is backfilled in registration order.
	doc := stateDoc{Devices: []docDevice{
		{ID: "x", Name: "late", CreatedAt: time.Unix(200, 0)},
		{ID: "y", Name: "early", CreatedAt: time.Unix(100, 0)},
	}}
	if !doc.backfillLetters() || doc.Devices[1].Letter != "A" || doc.Devices[0].Letter != "B" {
		t.Fatalf("backfill %+v", doc.Devices)
	}
	if doc.backfillLetters() {
		t.Fatal("second backfill changed something")
	}
}
