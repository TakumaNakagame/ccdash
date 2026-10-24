// Package i18n picks the language of ccdash's own words in the TUI, the
// CLI and the hub's push notifications: Japanese or English.
//
// Text is written in Japanese at the call site and looked up in en (en.go)
// when English is on. What goes over the wire or into the database —
// attention reasons, wait labels, task bucket headings, the timeline
// entries ccdash writes — stays Japanese, so every client and every older
// version reads the same values; Reason, Heading and Timeline translate it
// where it is shown. The portal does the same in internal/hub/web/i18n.js.
package i18n

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

// Setting values for the language setting ("" and "auto" follow the
// environment).
const (
	Auto = "auto"
	JA   = "ja"
	EN   = "en"
)

var cur atomic.Value // JA or EN

// Resolve turns a setting value into JA or EN: an explicit choice, else the
// locale ($LC_ALL, $LC_MESSAGES, $LANG — the first one set): Japanese for
// ja*, English for anything else.
func Resolve(setting string) string {
	switch setting {
	case JA, EN:
		return setting
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "ja") {
				return JA
			}
			return EN
		}
	}
	return EN
}

// Set applies a setting value (see Resolve) for this process.
func Set(setting string) { cur.Store(Resolve(setting)) }

// Lang is the language in effect: what Set chose, else the environment's.
func Lang() string {
	if v, ok := cur.Load().(string); ok {
		return v
	}
	return Resolve(Auto)
}

// T translates a Japanese string into the current language.
func T(ja string) string {
	if Lang() == JA {
		return ja
	}
	return En(ja)
}

// Tf is fmt.Sprintf over T(format); an English format may reorder its
// arguments with %[n]s.
func Tf(format string, args ...any) string { return fmt.Sprintf(T(format), args...) }

// En is the English for ja, or ja itself when there is none.
func En(ja string) string {
	if s, ok := en[ja]; ok {
		return s
	}
	return ja
}

// Reason translates a label ccdash stored in Japanese — exact ("承認待ち")
// or followed by a detail ("回答待ち: <note>", the detail kept as is).
func Reason(s string) string {
	if Lang() == JA || s == "" {
		return s
	}
	return EnReason(s)
}

// EnReason is Reason into English whatever the current language (the hub
// builds both languages of a notification).
func EnReason(s string) string {
	if v, ok := en[s]; ok {
		return v
	}
	if i := strings.Index(s, ": "); i > 0 {
		if v, ok := en[s[:i]]; ok {
			return v + s[i:]
		}
	}
	return s
}

var weekdayEn = map[string]string{"月": "Mon", "火": "Tue", "水": "Wed", "木": "Thu", "金": "Fri", "土": "Sat", "日": "Sun"}

var weekdayRe = regexp.MustCompile(` ([月火水木金土日])`)

// Heading translates a task bucket heading, e.g. 来週（10/27 月〜10/31 金）.
func Heading(s string) string {
	if Lang() == JA || s == "" {
		return s
	}
	label, rest, _ := strings.Cut(s, "（")
	out := En(label)
	if rest != "" {
		rest = strings.TrimSuffix(rest, "）")
		rest = weekdayRe.ReplaceAllStringFunc(rest, func(m string) string { return " " + weekdayEn[strings.TrimSpace(m)] })
		out += " (" + strings.ReplaceAll(rest, "〜", " – ") + ")"
	}
	return out
}

var timelineRe = regexp.MustCompile(`^(状態|やる日)を「(.*?)」から「(.*?)」に(自動で)?変更(?:（(.*)）)?$`)

// Timeline translates the timeline entries ccdash writes on a status or
// plan change (tasks.SetStatus / Replan); anything else — a note from the
// person or from Claude — is returned as is.
func Timeline(s string) string {
	if Lang() == JA {
		return s
	}
	m := timelineRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	what := "Status"
	if m[1] == "やる日" {
		what = "Plan"
	}
	how := ""
	if m[4] != "" {
		how = " automatically"
	}
	out := fmt.Sprintf("%s changed%s from %q to %q", what, how, En(m[2]), En(m[3]))
	if m[5] != "" {
		out += " (" + m[5] + ")"
	}
	return out
}
