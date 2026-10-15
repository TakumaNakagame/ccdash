package mcpserver

import (
	"os"
	"testing"
)

// TestMain keeps the tests off the real state dir: a joined machine's
// hub.json would give this machine's rows a letter ("A1" for "#1").
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccdash-mcp-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
