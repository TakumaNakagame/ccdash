package screenprompt

import (
	"strings"
	"testing"
)

func TestKind(t *testing.T) {
	ask := []string{
		"←  ☐ 果物  ☐ 色  ✔ Submit  →", "", "好きな果物はどれですか？", "",
		"❯ 1. りんご", "     りんご", "  2. みかん", "  3. Type something.", "──────", "  4. Chat about this", "",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}
	if k, q := Kind(ask); k != "question" || q != "好きな果物はどれですか？" {
		t.Errorf("ask dialog = %q %q", k, q)
	}
	perm := []string{"╭────╮", "│ Bash command │", "│ rm -rf build │", "│ Do you want to proceed? │", "│ ❯ 1. Yes │", "│   2. No │", "╰────╯"}
	if k, q := Kind(perm); k != "confirm" || q != "Do you want to proceed?" {
		t.Errorf("permission dialog = %q %q", k, q)
	}
	trust := []string{" Quick safety check: Is this a project you created or one you trust?", "", " ❯ No, exit", "   Yes, I trust this folder", "", " Enter to confirm · Esc to cancel"}
	if k, q := Kind(trust); k != "confirm" || !strings.Contains(q, "trust") {
		t.Errorf("unnumbered trust menu = %q %q", k, q)
	}
	// A wrapped past prompt above the (empty) input box is not a menu.
	scrollback := []string{"❯ テストです。何も調べず、AskUserQuestion を使って", "  1問目「好きな果物」を聞いて", "● はい", "────", "❯ ", "────"}
	if k, _ := Kind(scrollback); k != "" {
		t.Errorf("past prompt detected as %q", k)
	}
	idle := []string{"● done", "────", "❯ ", "────", "  ⏵⏵ auto mode on"}
	if k, _ := Kind(idle); k != "" {
		t.Errorf("idle input prompt detected as %q", k)
	}
}
