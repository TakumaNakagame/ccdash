package cli

import (
	"os"
	"testing"
)

// TestWebStateRoundTrip: web.json records the portal and its collector
// mode, and only the process that wrote it removes it.
func TestWebStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	st := newWebState(19224, "http://127.0.0.1:19224", "127.0.0.1:19223")
	st.save()
	st.setMode(webEmbedded)
	got, err := loadWebState()
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != os.Getpid() || got.Port != 19224 || got.CollectorAddr != "127.0.0.1:19223" || got.Collector != webEmbedded {
		t.Fatalf("loaded %+v", got)
	}
	if run, ok := runningWeb(); ok {
		t.Fatalf("nothing answers on the port, yet running: %+v", run)
	}

	other := newWebState(19224, "", "")
	other.PID = os.Getpid() + 1000000 // not ours
	other.remove()
	if _, err := loadWebState(); err != nil {
		t.Fatal("removed by a process that didn't write it")
	}
	st.remove()
	if _, err := loadWebState(); err == nil {
		t.Fatal("web.json left behind")
	}
}
