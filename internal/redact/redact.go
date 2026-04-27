// Package redact masks common secret patterns out of strings before they
// land in the SQLite database. We can't know every possible secret format,
// but the well-known token shapes are easy wins and dramatically lower the
// chance of an operator's API key showing up in a transcript view or in
// `ccdash sessions` output.
package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

// String runs every redaction pattern over s and returns the masked text.
// Pure function — safe to apply multiple times.
func String(s string) string {
	if s == "" {
		return s
	}
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// JSON parses raw as JSON, redacts every string value, and re-marshals.
// Falls back to running String over the raw bytes if parsing fails so we
// never leave un-masked content because of an unexpected shape.
func JSON(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte(String(string(raw)))
	}
	walk(&v)
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(String(string(raw)))
	}
	return out
}

func walk(v *any) {
	switch t := (*v).(type) {
	case string:
		*v = String(t)
	case map[string]any:
		for k, vv := range t {
			// Mask keys that scream "secret" entirely, regardless of value
			// shape (some shells store creds in nested objects).
			if isSensitiveKey(k) {
				t[k] = redacted
				continue
			}
			walk(&vv)
			t[k] = vv
		}
	case []any:
		for i := range t {
			walk(&t[i])
		}
	}
}

const redacted = "<REDACTED>"

var sensitiveKeys = map[string]struct{}{
	"password": {}, "passwd": {}, "secret": {}, "token": {},
	"api_key": {}, "apikey": {}, "auth": {}, "authorization": {},
	"access_key": {}, "access_token": {}, "private_key": {},
	"client_secret": {}, "session_key": {},
}

func isSensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	if _, ok := sensitiveKeys[lower]; ok {
		return true
	}
	// Heuristic: any key containing both a "secret-ish" word and ending in
	// _key / _token / _secret. e.g. "github_token", "openai_api_key".
	for _, suffix := range []string{"_token", "_key", "_secret", "_password", "_passwd"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

type pattern struct {
	re   *regexp.Regexp
	repl string
}

// patterns intentionally err on the side of redacting too much rather than
// too little. The cost of a false positive (slightly less readable log) is
// way lower than the cost of a missed secret.
var patterns = []pattern{
	// AWS access key id (deterministic prefix + 16 base32-ish chars).
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AKIA<REDACTED>"},
	// AWS secret access key (40 base64-ish chars after a likely-name).
	{regexp.MustCompile(`(?i)aws[_-]?secret[_-]?access[_-]?key["'\s:=]+["']?[A-Za-z0-9/+=]{40}["']?`),
		"aws_secret_access_key=<REDACTED>"},
	// GitHub personal access tokens.
	{regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`), "ghp_<REDACTED>"},
	{regexp.MustCompile(`gho_[A-Za-z0-9]{36,}`), "gho_<REDACTED>"},
	{regexp.MustCompile(`ghs_[A-Za-z0-9]{36,}`), "ghs_<REDACTED>"},
	// Anthropic / OpenAI / generic sk- keys.
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`), "sk-<REDACTED>"},
	// Generic Bearer / Basic auth headers.
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{8,}`), "Bearer <REDACTED>"},
	{regexp.MustCompile(`(?i)basic\s+[A-Za-z0-9+/=]{8,}`), "Basic <REDACTED>"},
	// URL credentials: scheme://user:pass@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s@]+:[^/\s@]+@`), "${1}<REDACTED>@"},
	// KEY=VALUE in shell-ish strings where KEY screams "secret".
	{regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9_]*?(?:TOKEN|KEY|SECRET|PASSWORD|PASSWD)[A-Z0-9_]*)\s*=\s*['"]?([^\s'"]{4,})['"]?`),
		"${1}=<REDACTED>"},
	// --password=... / --token=... command line flags.
	{regexp.MustCompile(`(?i)(--?(?:password|token|api[_-]?key|secret)[\s=])([^\s]+)`),
		"${1}<REDACTED>"},
}
