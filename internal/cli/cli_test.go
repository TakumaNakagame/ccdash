package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.5", true},
		{"::1", true},
		{"localhost", true},
		// Wildcards accept traffic from every interface — they must NOT
		// count as loopback or ":9123" would silently bypass the
		// non-loopback warning and the token fail-closed guard.
		{"", false},
		{"0.0.0.0", false},
		{"::", false},
		// Specific non-loopback addresses / names.
		{"192.168.20.132", false},
		{"devbox", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestCheckBindSafety(t *testing.T) {
	// Point the state dir at a fresh temp dir so auth.Load sees no token.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Loopback binds never need a token.
	for _, addr := range []string{"127.0.0.1:9123", "localhost:9123", "[::1]:9123"} {
		if err := checkBindSafety(addr); err != nil {
			t.Errorf("checkBindSafety(%q) = %v, want nil", addr, err)
		}
	}

	// Non-loopback (including the wildcard spellings) without a token:
	// fail closed.
	for _, addr := range []string{":9123", "0.0.0.0:9123", "[::]:9123", "192.168.20.132:9123"} {
		if err := checkBindSafety(addr); err == nil {
			t.Errorf("checkBindSafety(%q) = nil, want refuse-without-token error", addr)
		}
	}

	// Malformed address.
	if err := checkBindSafety("no-port"); err == nil {
		t.Error("checkBindSafety(no-port) = nil, want error")
	}

	// With a token on disk, non-loopback proceeds (warning only).
	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "ccdash")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{":9123", "0.0.0.0:9123", "192.168.20.132:9123"} {
		if err := checkBindSafety(addr); err != nil {
			t.Errorf("checkBindSafety(%q) with token = %v, want nil", addr, err)
		}
	}
}
