package procmap

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPeers: only the name of a live claude's session file comes out.
func TestPeers(t *testing.T) {
	old := peerAlive
	peerAlive = func(pid int) bool { return pid != 3 }
	t.Cleanup(func() { peerAlive = old })
	dir := t.TempDir()
	sess := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sess, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"1.json":  `{"pid":1,"sessionId":"s-1","name":"camera-sales-f5","peerToken":"hunter2","status":"idle"}`,
		"2.json":  `{"pid":2,"sessionId":"s-2"}`,                  // no name yet
		"3.json":  `{"pid":3,"sessionId":"s-3","name":"gone-a1"}`, // dead process
		"4.json":  `not json`,
		"notes.x": `{"pid":5,"sessionId":"s-5","name":"x"}`,
	} {
		if err := os.WriteFile(filepath.Join(sess, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := Peers(dir)
	if len(got) != 1 || got["s-1"] != "camera-sales-f5" {
		t.Errorf("Peers = %v", got)
	}
	if got := Peers(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("missing dir: %v", got)
	}
}
