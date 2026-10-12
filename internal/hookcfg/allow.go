package hookcfg

// AllowTools adds tool names (e.g. "mcp__ccdash__set_status") to
// permissions.allow in the settings file at path, so Claude Code runs them
// without asking. Entries already there are left alone; it reports whether
// the file changed.
func AllowTools(path string, tools []string) (bool, error) {
	m, err := readSettings(path)
	if err != nil {
		return false, err
	}
	perms, _ := m["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		m["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)
	have := map[string]bool{}
	for _, a := range allow {
		if s, ok := a.(string); ok {
			have[s] = true
		}
	}
	changed := false
	for _, t := range tools {
		if !have[t] {
			allow = append(allow, t)
			have[t] = true
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	perms["allow"] = allow
	return true, writeSettings(path, m)
}
