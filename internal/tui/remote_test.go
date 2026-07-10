package tui

import "testing"

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"a;rm -rf /", "'a;rm -rf /'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRemoteCD(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// ~ stays unquoted so the REMOTE shell expands it.
		{"~", "cd ~"},
		{"~/projects", "cd ~/'projects'"},
		{"~/dir with space", "cd ~/'dir with space'"},
		// Absolute paths are fully quoted, including shell metacharacters
		// and embedded single quotes.
		{"/srv/app", "cd '/srv/app'"},
		{"/srv/dir with space", "cd '/srv/dir with space'"},
		{"/srv/it's", `cd '/srv/it'\''s'`},
		{"/srv/$(rm -rf /)", "cd '/srv/$(rm -rf /)'"},
	}
	for _, c := range cases {
		if got := remoteCD(c.in); got != c.want {
			t.Errorf("remoteCD(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestSSHArgs pins the exact remote invocation shape: the script must run
// under `bash -l` (login shell → .profile/.bash_profile PATH additions like
// ~/.local/bin apply, where claude typically lives) and must be
// single-quoted as ONE argument so the remote user's login shell hands it
// to bash verbatim.
func TestSSHArgs(t *testing.T) {
	got := sshArgs("op@devbox", "cd '/srv/app' && exec claude --resume 'sid-1'")
	want := []string{"-t", "op@devbox", `bash -lc 'cd '\''/srv/app'\'' && exec claude --resume '\''sid-1'\'''`}
	if len(got) != len(want) {
		t.Fatalf("sshArgs = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sshArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
