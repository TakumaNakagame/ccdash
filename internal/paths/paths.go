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

func ClaudeUserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}
