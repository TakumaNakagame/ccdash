package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/settings"
)

// TestHubSkillsManyDirs: every ?dir= is searched for project skills, so
// the portal's skill picker finds them in all recent session directories.
func TestHubSkillsManyDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(proj, name string) string {
		root := filepath.Join(home, "src", proj)
		d := filepath.Join(root, ".claude", "skills", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	a, b := mk("a", "alpha"), mk("b", "beta")
	if err := os.MkdirAll(filepath.Join(b, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	q := url.Values{"dir": {a, filepath.Join(b, "sub")}}
	rec := httptest.NewRecorder()
	handleHubSkills(rec, httptest.NewRequest("GET", "/hub/skills?"+q.Encode(), nil), settings.Settings{}, "")
	var rows []struct {
		Name, Source, Dir string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.Dir
	}
	if got["alpha"] != a || got["beta"] != b {
		t.Fatalf("skills = %+v, want alpha in %s and beta in %s", rows, a, b)
	}
}
