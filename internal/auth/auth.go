// Package auth manages the shared loopback token that every ccdash hook
// and approval-decision request must carry. Even though the collector
// binds to 127.0.0.1, on a multi-user host any other UNIX user can reach
// the port; this token (read-only to the owner) blocks them from forging
// hook events or remotely allowing tool calls.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

const HeaderName = "X-Ccdash-Token"

// LoadOrCreate returns the token at $XDG_STATE_HOME/ccdash/token, generating
// a fresh 32-byte hex value if the file is missing. The file is written
// with 0600 so only the owning UID can read it.
func LoadOrCreate() (string, error) {
	path, err := tokenPath()
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// Load returns the token without creating one. Returns an error wrapping
// os.ErrNotExist when the file isn't there yet — callers can use this to
// distinguish "never set up" from "I/O error".
func Load() (string, error) {
	path, err := tokenPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("ccdash token at %s is empty", path)
	}
	return s, nil
}

func tokenPath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "token"), nil
}
