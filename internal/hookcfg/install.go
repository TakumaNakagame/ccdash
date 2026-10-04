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

// Hook wiring strategy per event.
//
// PermissionRequest is the only event whose *response* Claude Code consumes:
// the collector blocks it on a 25 s channel and answers with hookSpecificOutput
// (allow/deny). It therefore stays a blocking HTTP hook with a 30 s timeout.
//
// Every other event is fire-and-forget telemetry — the collector records it and
// returns "ok", nothing Claude acts on. Wiring those as HTTP hooks means a
// stopped or (worse) hung collector stalls the session for the full timeout on
// every prompt and every tool call. Instead we point them at a tiny shell
// forwarder (HookScriptPath) that drains stdin and pushes the POST into the
// background, so a dead ccdash costs the session ~nothing.
const (
	blockingTimeoutSecs = 30 // PermissionRequest HTTP hook: waits for operator approval
	commandTimeoutSecs  = 5  // fire-and-forget forwarder: safety net; it returns in ms
)

// blockingEvent reports whether an event must use a synchronous HTTP hook
// because Claude Code needs the collector's response.
func blockingEvent(event string) bool {
	return event == "PermissionRequest"
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
		MarkerKey:               "true",
		auth.HeaderName:         token,
		"X-Ccdash-Wrapper-Pid":  "${CCDASH_WRAPPER_PID}",
		"X-Ccdash-Git-Repo":     "${CCDASH_GIT_REPO}",
		"X-Ccdash-Git-Branch":   "${CCDASH_GIT_BRANCH}",
		"X-Ccdash-Git-Commit":   "${CCDASH_GIT_COMMIT}",
		"X-Ccdash-Tmux-Pane":    "${CCDASH_TMUX_PANE}",
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

	scriptPath, err := paths.HookScriptPath()
	if err != nil {
		return false, fmt.Errorf("resolve hook script path: %w", err)
	}
	tokenPath, err := paths.TokenPath()
	if err != nil {
		return false, fmt.Errorf("resolve token path: %w", err)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	for event, path := range Endpoints {
		var entry map[string]any
		if blockingEvent(event) {
			entry = buildHTTPEntry(in.BaseURL+path, tok, blockingTimeoutSecs)
		} else {
			entry = buildCommandEntry(scriptPath, path, commandTimeoutSecs)
		}
		hooks[event] = mergeEvent(hooks[event], entry)
	}
	settings["hooks"] = hooks

	if in.DryRun {
		buf, _ := json.MarshalIndent(settings, "", "  ")
		fmt.Println(string(buf))
		return false, nil
	}
	// Write the forwarder before settings.json so no hook can ever reference a
	// missing script.
	if err := writeHookScript(scriptPath, tokenPath, in.BaseURL); err != nil {
		return false, fmt.Errorf("write hook script: %w", err)
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
	if err := writeSettings(in.Path, settings); err != nil {
		return err
	}
	// Best-effort: drop the forwarder script now that nothing references it.
	if scriptPath, err := paths.HookScriptPath(); err == nil {
		if err := os.Remove(scriptPath); err != nil && !os.IsNotExist(err) {
			// Non-fatal: settings.json is already clean.
			fmt.Fprintf(os.Stderr, "ccdash: could not remove %s: %v\n", scriptPath, err)
		}
	}
	return nil
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

// NeedsResync reports whether the ccdash entries in the settings file at path
// predate the current layout: a fire-and-forget event still wired as a
// blocking HTTP hook (installs from before the hook.sh forwarder), or a
// command entry whose forwarder script is gone. The collector re-runs Apply
// on boot when this is true, so upgrading ccdash migrates existing installs
// without a manual install-hooks. Returns false when nothing is installed.
func NeedsResync(path string) (bool, error) {
	settings, err := readSettings(path)
	if err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	scriptPath, err := paths.HookScriptPath()
	if err != nil {
		return false, err
	}
	for event, raw := range hooks {
		arr, _ := raw.([]any)
		for _, item := range arr {
			if !isManagedEntry(item) {
				continue
			}
			m := item.(map[string]any)
			handlers, _ := m["hooks"].([]any)
			for _, h := range handlers {
				hm, _ := h.(map[string]any)
				switch hm["type"] {
				case "http":
					if !blockingEvent(event) {
						return true, nil
					}
				case "command":
					if _, err := os.Stat(scriptPath); err != nil {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

func buildHTTPEntry(url, token string, timeout int) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":           "http",
				"url":            url,
				"timeout":        timeout,
				"allowedEnvVars": toAnySlice(allowedEnvVars),
				"headers":        toAnyMap(defaultHeaders(token)),
			},
		},
	}
}

// buildCommandEntry wires a fire-and-forget event to the shell forwarder. The
// endpoint path is passed as an argument; the marker for idempotent
// removal is the script path itself (see isManagedEntry).
func buildCommandEntry(scriptPath, endpoint string, timeout int) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": shellQuote(scriptPath) + " " + endpoint,
				"timeout": timeout,
			},
		},
	}
}

// shellQuote single-quotes s for sh, so a state dir containing spaces still
// yields a runnable hook command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookScriptTemplate is the POSIX-sh forwarder. %s placeholders are, in order,
// the collector base URL and the absolute token file path. It reads the hook
// payload from stdin, then backgrounds the POST with every std fd detached so
// the parent returns immediately and Claude Code sees EOF at once — a stopped
// or hung ccdash never stalls the session.
const hookScriptTemplate = `#!/bin/sh
# ccdash fire-and-forget hook forwarder — generated by ` + "`ccdash install-hooks`" + `.
# DO NOT EDIT: rewritten on every install-hooks run.
#
# Records a Claude Code hook event without ever blocking the session. The
# payload is drained from stdin in the foreground; the network POST is pushed
# into the background with all std fds detached, so if ccdash is stopped or
# hung the session proceeds instantly and the POST just fails silently. Only
# PermissionRequest still uses a blocking HTTP hook (it needs the allow/deny
# response).
#
# Usage: hook.sh <endpoint-path>   e.g. hook.sh /hooks/user-prompt
set -u

endpoint="${1:-}"
[ -n "$endpoint" ] || exit 0

# Drain stdin fully so Claude Code's pipe closes promptly.
payload="$(cat)"

tok="$(cat "%[2]s" 2>/dev/null || true)"
tok="$(printf '%%s' "$tok" | tr -d '[:space:]')"
[ -n "$tok" ] || exit 0

(
  printf '%%s' "$payload" | curl -sS --max-time 2 -X POST "%[1]s${endpoint}" \
    -H "X-Ccdash-Token: $tok" \
    -H "X-Ccdash-Managed: true" \
    -H "Content-Type: application/json" \
    -H "X-Ccdash-Wrapper-Pid: ${CCDASH_WRAPPER_PID:-}" \
    -H "X-Ccdash-Git-Repo: ${CCDASH_GIT_REPO:-}" \
    -H "X-Ccdash-Git-Branch: ${CCDASH_GIT_BRANCH:-}" \
    -H "X-Ccdash-Git-Commit: ${CCDASH_GIT_COMMIT:-}" \
    -H "X-Ccdash-Tmux-Pane: ${CCDASH_TMUX_PANE:-}" \
    -H "X-Ccdash-Tmux-Session: ${CCDASH_TMUX_SESSION:-}" \
    --data-binary @- \
) </dev/null >/dev/null 2>&1 &
exit 0
`

// writeHookScript renders and writes the forwarder atomically with 0700 perms.
func writeHookScript(scriptPath, tokenPath, baseURL string) error {
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o700); err != nil {
		return err
	}
	body := fmt.Sprintf(hookScriptTemplate, baseURL, tokenPath)
	tmp := scriptPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o700); err != nil {
		return err
	}
	return os.Rename(tmp, scriptPath)
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

// managedCommandMarker is the path fragment every generated forwarder command
// contains ("<state dir>/ccdash/hook.sh ..."). Matching on it lets Remove and
// mergeEvent recognise our command-type entries the same way the marker header
// identifies our HTTP entries — including entries left by an older install
// under a differently-resolved state dir.
const managedCommandMarker = "ccdash/hook.sh"

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
		// HTTP entries: marker header (PermissionRequest, and pre-sidecar installs).
		if headers, ok := hm["headers"].(map[string]any); ok {
			if v, ok := headers[MarkerKey]; ok {
				if s, ok := v.(string); ok && s != "" {
					return true
				}
			}
		}
		// Command entries: the fire-and-forget forwarder path.
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, managedCommandMarker) {
			return true
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
