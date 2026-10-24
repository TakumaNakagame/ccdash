package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/i18n"
)

// The test binary doubles as a stand-in process: with this variable set it
// just sleeps, so a test can start it under any argv (e.g. "ccdash server")
// and read it back through ps.
const pidHelperEnv = "CCDASH_TEST_PID_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(pidHelperEnv) == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	i18n.Set(i18n.JA) // the tests read ccdash's Japanese text
	os.Exit(m.Run())
}

// startHelper runs the test binary as a sleeping process whose argv is args.
func startHelper(t *testing.T, args ...string) int {
	pid, _ := startHelperDone(t, args...)
	return pid
}

// startHelperDone is startHelper that also returns a channel closed when
// the process has exited (and been reaped).
func startHelperDone(t *testing.T, args ...string) (int, <-chan struct{}) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: self, Args: args, Env: append(os.Environ(), pidHelperEnv+"=1")}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	// ps sees the new argv once the child has exec'd.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := procArgs(cmd.Process.Pid); err == nil && strings.HasPrefix(a, args[0]) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd.Process.Pid, done
}

func TestParseServerPID(t *testing.T) {
	rec, err := parseServerPID([]byte("1234\n"))
	if err != nil || rec.PID != 1234 || rec.Start != "" || rec.Addr != "" {
		t.Fatalf("old format: %+v %v", rec, err)
	}
	rec, err = parseServerPID([]byte(`{"pid":42,"addr":"127.0.0.1:9123","start":"Tue Oct 13 14:22:31 2026"}`))
	if err != nil || rec.PID != 42 || rec.Addr != "127.0.0.1:9123" || rec.Start == "" {
		t.Fatalf("json: %+v %v", rec, err)
	}
	for _, bad := range []string{"", "0", "-3", "{}", "nope"} {
		if _, err := parseServerPID([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestIsServerArgs(t *testing.T) {
	for args, want := range map[string]bool{
		"ccdash server": true,
		"/opt/homebrew/bin/ccdash server --listen 0.0.0.0:9123":   true,
		"/home/u/go/bin/ccdash server":                            true,
		"ccdash web --foreground --collector-addr 127.0.0.1:9123": false,
		"ccdash":                        false,
		"/usr/sbin/sshd -D":             false,
		"vim server":                    false,
		"python3 -m http.server":        false,
		"/usr/bin/server ccdash":        false,
		"bash -c ccdash-helper; server": false,
	} {
		if got := isServerArgs(args); got != want {
			t.Errorf("isServerArgs(%q) = %v, want %v", args, got, want)
		}
	}
}

// ps must give the same lstart for one process every time, and its args —
// on macOS's BSD ps and Linux's procps alike.
func TestProcStartAndArgs(t *testing.T) {
	pid := startHelper(t, "ccdash", "server")
	a, err := procStart(pid)
	if err != nil || a == "" {
		t.Fatalf("procStart: %q %v", a, err)
	}
	b, _ := procStart(pid)
	if a != b {
		t.Fatalf("lstart changed: %q then %q", a, b)
	}
	args, err := procArgs(pid)
	if err != nil || args != "ccdash server" {
		t.Fatalf("procArgs: %q %v", args, err)
	}
	if _, err := procStart(1 << 30); err == nil {
		t.Fatal("procStart of a missing pid: want an error")
	}
}

func TestVerifyServerPID(t *testing.T) {
	const addr = "127.0.0.1:9123"
	srv := startHelper(t, "/usr/local/bin/ccdash", "server")
	other := startHelper(t, "sleeper", "server")
	start, err := procStart(srv)
	if err != nil {
		t.Fatal(err)
	}

	ok := []serverPIDRecord{
		{PID: srv, Addr: addr, Start: start},
		{PID: srv, Addr: "0.0.0.0:9123", Start: start}, // same port, wildcard bind
		{PID: srv}, // old format: args only
	}
	for _, rec := range ok {
		if err := verifyServerPID(rec, addr); err != nil {
			t.Errorf("%+v: %v", rec, err)
		}
	}
	bad := []serverPIDRecord{
		{PID: srv, Addr: addr, Start: "Thu Jan  1 00:00:00 1970"}, // pid reused
		{PID: srv, Addr: "127.0.0.1:9999", Start: start},          // another collector's
		{PID: other, Addr: addr},                                  // not ccdash
		{PID: other},
		{PID: 1 << 30}, // gone
	}
	for _, rec := range bad {
		if err := verifyServerPID(rec, addr); err == nil {
			t.Errorf("%+v: verified, want refused", rec)
		}
	}
}

func TestSignalServerPIDRefusesStranger(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	const addr = "127.0.0.1:9123"
	pid := startHelper(t, "sleeper", "server")
	saveServerPID(pid, addr)
	if signalServerPID(addr) {
		t.Fatal("signalled a process that isn't a ccdash server")
	}
	if !pidAlive(pid) {
		t.Fatal("the stranger was stopped")
	}
	if _, err := loadServerPID(); err == nil {
		t.Fatal("server.pid kept after it was found stale")
	}

	srv, done := startHelperDone(t, "ccdash", "server")
	saveServerPID(srv, addr)
	if !signalServerPID(addr) {
		t.Fatal("did not signal the recorded ccdash server")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the recorded ccdash server is still running after SIGTERM")
	}
}
