// Package accounts loads the multi-account configuration from
// ~/.local/state/ccdash/accounts.json. When the file is absent or empty a
// single default account pointing at ~/.claude is returned so existing
// single-account setups require no configuration.
package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

// Account describes one Claude installation directory.
type Account struct {
	// Name is the human-readable label shown in the TUI (e.g. "personal",
	// "enterprise"). Must be unique across entries.
	Name string `json:"name"`
	// Dir is the Claude config directory (e.g. "~/.claude", "~/.claude-ent").
	// Tilde expansion is performed on load.
	Dir string `json:"dir"`
}

// Load reads the accounts config file and returns the configured accounts.
// If the file does not exist a single default account is returned.
func Load() ([]Account, error) {
	cfgPath, err := paths.AccountsConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return defaults()
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return defaults()
	}
	var accs []Account
	if err := json.Unmarshal(b, &accs); err != nil {
		return nil, err
	}
	if len(accs) == 0 {
		return defaults()
	}
	// Expand leading ~ in Dir.
	home, _ := os.UserHomeDir()
	for i := range accs {
		accs[i].Dir = expandTilde(accs[i].Dir, home)
	}
	return accs, nil
}

func defaults() ([]Account, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return []Account{{Name: "default", Dir: filepath.Join(home, ".claude")}}, nil
}

func expandTilde(p, home string) string {
	if p == "~" {
		return home
	}
	if len(p) >= 2 && p[:2] == "~/" {
		return filepath.Join(home, p[2:])
	}
	return p
}
