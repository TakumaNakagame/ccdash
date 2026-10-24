package hub

import (
	"encoding/json"
	"regexp"
	"testing"
)

var (
	portalCall  = regexp.MustCompile(`\bT\(("(?:[^"\\]|\\.)*")`)
	portalEntry = regexp.MustCompile(`(?m)^  ("(?:[^"\\]|\\.)*"): "`)
	japanese    = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)
)

// TestPortalStringsTranslated: every T("…") in app.js has an English entry
// in i18n.js, so switching the portal to English never leaves Japanese
// behind for text that was meant to be translated.
func TestPortalStringsTranslated(t *testing.T) {
	keys := map[string]bool{}
	dict, err := webFS.ReadFile("web/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range portalEntry.FindAllStringSubmatch(string(dict), -1) {
		var k string
		if err := json.Unmarshal([]byte(m[1]), &k); err != nil {
			t.Fatalf("i18n.js key %s: %v", m[1], err)
		}
		keys[k] = true
	}
	if len(keys) < 100 {
		t.Fatalf("found only %d keys in i18n.js; did its format change?", len(keys))
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range portalCall.FindAllStringSubmatch(string(app), -1) {
		var k string
		if err := json.Unmarshal([]byte(m[1]), &k); err != nil {
			t.Errorf("app.js T(%s): %v", m[1], err)
			continue
		}
		if japanese.MatchString(k) && !keys[k] {
			t.Errorf("app.js T(%q) has no English in i18n.js", k)
		}
	}
}
