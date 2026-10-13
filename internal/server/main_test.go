package server

import (
	"os"
	"os/exec"
	"testing"
)

// TestMain swaps the PTY child for `cat` before any test runs. A test that
// forgets to stub newPTYCommand would otherwise start a real claude in the
// package directory, whose installed hooks report to the operator's own
// collector and leave an untitled session in their list.
func TestMain(m *testing.M) {
	newPTYCommand = func(args ...string) *exec.Cmd { return exec.Command("sh", "-c", "cat") }
	os.Exit(m.Run())
}
