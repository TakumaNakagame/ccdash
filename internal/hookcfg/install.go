package hookcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/paths"
)

// MarkerKey identifies hook entries managed by ccdash so we can remove or
// replace them idempotently without touching user-defined hooks.
const MarkerKey = "X-Ccdash-Managed"

// TokenHeaderKey is the header carrying the loopback shared secret on every
// hook entry written by Apply().
const TokenHeaderKey = "X-Ccdash-Token"

// Endpoints maps Claude Code hook event names to ccdash HTTP paths.
var Endpoints = map[string]string{
	"SessionStart":       "/hooks/session-start",
	"SessionEnd":         "/hooks/session-end",
	"UserPromptSubmit":   "/hooks/user-prompt",
	"PreToolUse":         "/hooks/pre-tool",
	"PostToolUse":        "/hooks/post-tool",
	"PostToolUseFailure": "/hooks/post-tool-failure",
	"PermissionRequest":  "/hooks/permission-request",
	"Stop":               "/hooks/stop",
	"SubagentStop":       "/hooks/subagent-stop",
	"Notification":       "/hooks/notification",
}

var allowedEnvVars = []string{
	"CCDASH_WRAPPER_PID",
	"CCDASH_GIT_REPO",
	"CCDASH_GIT_BRANCH",
	"CCDASH_GIT_COMMIT",
	"CCDASH_TMUX_PANE",
	"CCDASH_TMUX_SESSION",
}

func defaultHeaders(token string) map[string]string {
	return map[string]string{
		MarkerKey:              "true",
		auth.HeaderName:        token,
		"X-Ccdash-Wrapper-Pid": "${CCDASH_WRAPPER_PID}",
		"X-Ccdash-Git-Repo":    "${CCDASH_GIT_REPO}",
		"X-Ccdash-Git-Branch":  "${CCDASH_GIT_BRANCH}",
		"X-Ccdash-Git-Commit":  "${CCDASH_GIT_COMMIT}",
		"X-Ccdash-Tmux-Pane":   "${CCDASH_TMUX_PANE}",
		"X-Ccdash-Tmux-Session": "${CCDASH_TMUX_SESSION}",
	}
}

type Install struct {
	BaseURL string // e.g. "http://127.0.0.1:9123"
	Path    string // settings.json path
	DryRun  bool
}

func DefaultInstall() (*Install, error) {
	p, err := paths.ClaudeUserSettingsPath()
	if err != nil {
		return nil, err
	}
	return &Install{
		BaseURL: fmt.Sprintf("http://%s:%d", paths.DefaultHost, paths.DefaultPort),
		Path:    p,
	}, nil
}

func (in *Install) Apply() (changed bool, err error) {
	tok, err := auth.LoadOrCreate()
	if err != nil {
		return false, fmt.Errorf("load auth token: %w", err)
	}
	settings, err := readSettings(in.Path)
	if err != nil {
		return false, err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	for event, path := range Endpoints {
		entry := buildEntry(in.BaseURL+path, tok)
		hooks[event] = mergeEvent(hooks[event], entry)
	}
	settings["hooks"] = hooks

	if in.DryRun {
		buf, _ := json.MarshalIndent(settings, "", "  ")
		fmt.Println(string(buf))
		return false, nil
	}
	return true, writeSettings(in.Path, settings)
}

func (in *Install) Remove() error {
	settings, err := readSettings(in.Path)
	if err != nil {
		return err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	for event := range Endpoints {
		hooks[event] = removeManagedFromEvent(hooks[event])
	}
	// drop empty event entries
	for k, v := range hooks {
		if arr, ok := v.([]any); ok && len(arr) == 0 {
			delete(hooks, k)
		}
	}
	settings["hooks"] = hooks
	return writeSettings(in.Path, settings)
}

// InstalledTokenAt returns the X-Ccdash-Token currently baked into the
// settings.json at path. Empty string means the user never ran install-hooks
// (or removed our entries). Returns an error only on filesystem trouble; a
// missing file or absent token is reported as ("", nil).
func InstalledTokenAt(path string) (string, error) {
	settings, err := readSettings(path)
	if err != nil {
		return "", err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return "", nil
	}
	for _, event := range hooks {
		arr, ok := event.([]any)
		if !ok {
			continue
		}
		for _, item := range arr {
			if !isManagedEntry(item) {
				continue
			}
			m := item.(map[string]any)
			handlers, _ := m["hooks"].([]any)
			for _, h := range handlers {
				hm, _ := h.(map[string]any)
				headers, _ := hm["headers"].(map[string]any)
				if v, ok := headers[TokenHeaderKey]; ok {
					if s, ok := v.(string); ok {
						return s, nil
					}
				}
			}
		}
	}
	return "", nil
}

func buildEntry(url, token string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":           "http",
				"url":            url,
				"timeout":        30,
				"allowedEnvVars": toAnySlice(allowedEnvVars),
				"headers":        toAnyMap(defaultHeaders(token)),
			},
		},
	}
}

// mergeEvent appends the ccdash entry to existing event hooks, replacing any
// previous ccdash-managed entry (identified by the marker header).
func mergeEvent(existing any, entry map[string]any) []any {
	var arr []any
	if existing != nil {
		if a, ok := existing.([]any); ok {
			arr = a
		}
	}
	// remove any prior ccdash entry
	cleaned := make([]any, 0, len(arr)+1)
	for _, item := range arr {
		if !isManagedEntry(item) {
			cleaned = append(cleaned, item)
		}
	}
	cleaned = append(cleaned, entry)
	return cleaned
}

func removeManagedFromEvent(existing any) []any {
	arr, ok := existing.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		if !isManagedEntry(item) {
			out = append(out, item)
		}
	}
	return out
}

func isManagedEntry(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		headers, ok := hm["headers"].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := headers[MarkerKey]; ok {
			if s, ok := v.(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func writeSettings(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
