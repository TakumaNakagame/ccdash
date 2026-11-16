package accounts

import "github.com/TakumaNakagame/ccdash/internal/procmap"

// Peers maps the session id of every running claude, across the configured
// accounts, to the name Claude Code's SendMessage reaches it by
// (procmap.Peers). Read live; the names change per process.
func Peers() map[string]string {
	out := map[string]string{}
	accs, err := Load()
	if err != nil {
		accs = []Account{{Name: "default"}}
	}
	for _, acc := range accs {
		for sid, name := range procmap.Peers(acc.Dir) {
			out[sid] = name
		}
	}
	return out
}
