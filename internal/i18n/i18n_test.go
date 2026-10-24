package i18n

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var call = regexp.MustCompile(`i18n\.Tf?\(("(?:[^"\\]|\\.)*")`)

// TestEveryCallTranslated: every literal passed to i18n.T / Tf in the
// repository has an English entry.
func TestEveryCallTranslated(t *testing.T) {
	n := 0
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range call.FindAllStringSubmatch(string(b), -1) {
			var s string
			if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
				t.Errorf("%s: %s: %v", path, m[1], err)
				continue
			}
			n++
			if _, ok := en[s]; !ok {
				t.Errorf("%s: i18n.T(%q) has no English in en.go", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("found only %d calls; did the pattern stop matching?", n)
	}
}

func TestTranslate(t *testing.T) {
	defer Set(Auto)
	Set(JA)
	if got := T("要対応"); got != "要対応" {
		t.Errorf("ja T = %q", got)
	}
	if got := Reason("回答待ち: どっち？"); got != "回答待ち: どっち？" {
		t.Errorf("ja Reason = %q", got)
	}
	Set(EN)
	for _, c := range []struct {
		fn       func(string) string
		in, want string
	}{
		{T, "要対応", "Needs you"},
		{T, "not in the table", "not in the table"},
		{Reason, "承認待ち", "Awaiting approval"},
		{Reason, "回答待ち: どっち？", "Awaiting answer: どっち？"},
		{Reason, "free text: x", "free text: x"},
		{Heading, "今日（10/13 火）", "Today (10/13 Tue)"},
		{Heading, "来週（10/19 月〜10/23 金）", "Next week (10/19 Mon – 10/23 Fri)"},
		{Heading, "未定", "Unplanned"},
		{Timeline, "状態を「未着手」から「完了」に変更", `Status changed from "To do" to "Done"`},
		{Timeline, "やる日を「2026-10-13」から「未定」に自動で変更（Slack で延期）", `Plan changed automatically from "2026-10-13" to "Unplanned" (Slack で延期)`},
		{Timeline, "レビュー依頼を出した", "レビュー依頼を出した"},
	} {
		if got := c.fn(c.in); got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
	if got := Tf("#%d を追加しました: %s\n", 3, "x"); got != "Added #3: x\n" {
		t.Errorf("Tf = %q", got)
	}
}

func TestResolve(t *testing.T) {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(k, "")
	}
	if Resolve(Auto) != EN {
		t.Error("no locale should be English")
	}
	t.Setenv("LANG", "ja_JP.UTF-8")
	if Resolve(Auto) != JA || Resolve("") != JA {
		t.Error("ja_JP should be Japanese")
	}
	t.Setenv("LC_ALL", "C.UTF-8")
	if Resolve(Auto) != EN {
		t.Error("LC_ALL wins over LANG")
	}
	if Resolve(JA) != JA || Resolve(EN) != EN {
		t.Error("an explicit choice wins")
	}
}
