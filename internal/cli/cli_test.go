package cli

import (
	"os"
	"path/filepath"
	"strings"
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

// writeClientConfig drops a config.json under a temp XDG_CONFIG_HOME and
// returns after pointing the env var at it.
func writeClientConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfgDir := filepath.Join(dir, "ccdash")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveRemoteLocalMode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rc, err := resolveRemote(&remoteFlags{})
	if err != nil || rc != nil {
		t.Fatalf("no -r / --remote-url must stay local: rc=%v err=%v", rc, err)
	}
}

func TestResolveRemoteUnconfigured(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CCDASH_TOKEN", "")
	_, err := resolveRemote(&remoteFlags{remoteEnabled: true})
	if err == nil {
		t.Fatal("-r without config must error")
	}
	for _, want := range []string{"ccdash remote set", "config.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unconfigured -r error %q should mention %q", err, want)
		}
	}
}

func TestResolveRemoteFromConfig(t *testing.T) {
	tokenPath := writeTokenFile(t, "cfg-token")
	writeClientConfig(t, `{"remote":{"url":"http://192.168.20.132:9123","token_file":"`+tokenPath+`","ssh_target":"claude-code"}}`)
	t.Setenv("CCDASH_TOKEN", "")

	rc, err := resolveRemote(&remoteFlags{remoteEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if rc.baseURL != "http://192.168.20.132:9123" || rc.token != "cfg-token" || rc.sshTarget != "claude-code" {
		t.Fatalf("config-driven resolve = %+v", rc)
	}
}

func TestResolveRemotePrecedence(t *testing.T) {
	cfgTokenPath := writeTokenFile(t, "cfg-token")
	writeClientConfig(t, `{"remote":{"url":"http://cfg-host:9123","token_file":"`+cfgTokenPath+`","ssh_target":"cfg-ssh"}}`)

	// Explicit flags beat every config value; --remote-url alone implies
	// remote mode (no -r needed).
	flagTokenPath := writeTokenFile(t, "flag-token")
	rc, err := resolveRemote(&remoteFlags{
		remoteURL: "http://flag-host:9999",
		tokenFile: flagTokenPath,
		sshTarget: "flag-ssh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rc.baseURL != "http://flag-host:9999" || rc.token != "flag-token" || rc.sshTarget != "flag-ssh" {
		t.Fatalf("flag precedence = %+v", rc)
	}

	// Config token_file beats env.
	t.Setenv("CCDASH_TOKEN", "env-token")
	rc, err = resolveRemote(&remoteFlags{remoteEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if rc.token != "cfg-token" {
		t.Fatalf("config token_file should beat env, got %q", rc.token)
	}
}

func TestResolveRemoteEnvTokenFallback(t *testing.T) {
	writeClientConfig(t, `{"remote":{"url":"http://cfg-host:9123"}}`)
	t.Setenv("CCDASH_TOKEN", "env-token")

	rc, err := resolveRemote(&remoteFlags{remoteEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if rc.token != "env-token" {
		t.Fatalf("token = %q, want env-token", rc.token)
	}
	// No ssh_target anywhere → URL hostname.
	if rc.sshTarget != "cfg-host" {
		t.Fatalf("sshTarget = %q, want cfg-host (URL hostname default)", rc.sshTarget)
	}

	// No token source at all → helpful error.
	t.Setenv("CCDASH_TOKEN", "")
	_, err = resolveRemote(&remoteFlags{remoteEnabled: true})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("tokenless resolve = %v, want token guidance", err)
	}
}

func TestResolveRemoteMalformedConfigNeverIgnored(t *testing.T) {
	writeClientConfig(t, "{broken")
	t.Setenv("CCDASH_TOKEN", "env-token")

	// Even with a fully-specified override URL, a broken config file must
	// surface instead of being silently skipped.
	_, err := resolveRemote(&remoteFlags{remoteURL: "http://flag-host:9999"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed config resolve = %v, want malformed-config error", err)
	}
}

func TestResolveRemoteConfigMissingURL(t *testing.T) {
	writeClientConfig(t, `{"remote":{"ssh_target":"cfg-ssh"}}`)
	t.Setenv("CCDASH_TOKEN", "")
	_, err := resolveRemote(&remoteFlags{remoteEnabled: true})
	if err == nil || !strings.Contains(err.Error(), "ccdash remote set") {
		t.Fatalf("config without url = %v, want remote-set guidance", err)
	}
}
