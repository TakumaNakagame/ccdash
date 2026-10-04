package paths

import (
	"os"
	"path/filepath"
)

const (
	DefaultPort = 9123
	DefaultHost = "127.0.0.1"
)

func StateDir() (string, error) {
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(xdg, "ccdash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func DBPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ccdash.sqlite"), nil
}

// TokenPath is the loopback shared-secret file. Kept here (rather than only in
// internal/auth) so the fire-and-forget hook script generator can bake the
// absolute path in and read the current token at runtime.
func TokenPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "token"), nil
}

// HookScriptPath is the POSIX-sh forwarder that non-blocking Claude Code hooks
// exec. It POSTs the event to the collector in the background so a stopped or
// hung ccdash never stalls the session. Written by `ccdash install-hooks`.
func HookScriptPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hook.sh"), nil
}

func ClaudeUserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func AccountsConfigPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts.json"), nil
}
