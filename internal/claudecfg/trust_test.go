package claudecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnder(t *testing.T) {
	home, _ := os.UserHomeDir()
	roots := "~/DevWorks/x, /srv/a\n"
	for dir, want := range map[string]bool{
		filepath.Join(home, "DevWorks/x"):         true,
		filepath.Join(home, "DevWorks/x/new"):     true,
		filepath.Join(home, "DevWorks/xy"):        false,
		"/srv/a/b/c":                              true,
		"/srv":                                    false,
		filepath.Join(home, "DevWorks/x/../evil"): false,
	} {
		if got := Under(dir, roots); got != want {
			t.Errorf("Under(%q) = %v, want %v", dir, got, want)
		}
	}
	if Under("/anything", "") {
		t.Error("empty roots trusted something")
	}
}

func TestTrust(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	orig := `{"numStartups": 12345678901234567, "projects": {"/old": {"hasTrustDialogAccepted": true, "x": "<a&b>"}}, "other": [1, 2.5]}`
	if err := os.WriteFile(cfg, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(dir, "new")
	_ = os.Mkdir(proj, 0o755)
	changed, err := Trust(cfg, proj)
	if err != nil || !changed {
		t.Fatalf("Trust = %v, %v", changed, err)
	}
	b, _ := os.ReadFile(cfg)
	s := string(b)
	for _, want := range []string{`"numStartups": 12345678901234567`, `"x": "<a&b>"`, `2.5`, `"` + proj + `": {`, `"hasTrustDialogAccepted": true`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if changed, err := Trust(cfg, proj); err != nil || changed {
		t.Fatalf("second Trust = %v, %v", changed, err)
	}
	if _, err := Trust(filepath.Join(dir, "missing.json"), proj); err == nil {
		t.Fatal("missing config was created")
	}
}
