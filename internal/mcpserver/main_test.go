package mcpserver

import (
	"os"
	"testing"
)

// TestMain keeps the tests off the real state dir: a joined machine's
// hub.json would give this machine's rows a letter ("A1" for "#1"). HOME
// moves too, so store.Local's Peers never reads the real ~/.claude/sessions.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccdash-mcp-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
