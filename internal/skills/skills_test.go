package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins", "cache", "m", "flow", "1.0")
	write(t, filepath.Join(dir, "skills", "mine", "SKILL.md"), "---\nname: mine\ndescription: my skill\n---\nbody\n")
	write(t, filepath.Join(dir, "skills", "alert-fix", "SKILL.md"), "---\nname: Alert Fix\ndescription: spaced\n---\n")
	write(t, filepath.Join(dir, "skills", "synced", "abc", "docs", "SKILL.md"), "---\nname: docs\ndescription: >\n  folded\n  text\n---\n")
	write(t, filepath.Join(dir, "commands", "git", "fix.md"), "---\ndescription: \"quoted\"\n---\n")
	write(t, filepath.Join(pluginDir, "skills", "ship", "SKILL.md"), "---\nname: ship\ndescription: ship it\n---\n")
	write(t, filepath.Join(dir, "plugins", "cache", "m", "off", "1.0", "skills", "x", "SKILL.md"), "---\nname: x\n---\n")
	write(t, filepath.Join(dir, "settings.json"), `{"enabledPlugins":{"flow@m":true,"off@m":false}}`)
	write(t, filepath.Join(dir, "plugins", "installed_plugins.json"),
		`{"plugins":{"flow@m":[{"installPath":"`+pluginDir+`"}],"off@m":[{"installPath":"`+filepath.Join(dir, "plugins", "cache", "m", "off", "1.0")+`"}]}}`)

	got := List(dir)
	want := []Skill{
		{Name: "alert-fix", Description: "spaced", Source: "user"},
		{Name: "anthropic-skills:docs", Description: "folded text", Source: "synced"},
		{Name: "flow:ship", Description: "ship it", Source: "plugin"},
		{Name: "git:fix", Description: "quoted", Source: "command"},
		{Name: "mine", Description: "my skill", Source: "user"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestProjectRoots(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "working")
	write(t, filepath.Join(work, ".claude", "skills", "status", "SKILL.md"), "---\nname: status\ndescription: check\n---\n")
	write(t, filepath.Join(home, ".claude", "skills", "mine", "SKILL.md"), "---\nname: mine\n---\n") // user-level, not a project
	sub := filepath.Join(work, "projects", "a")
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	roots := ProjectRoots([]string{sub, filepath.Join(work, "projects", "b"), other, work}, home)
	if len(roots) != 1 || roots[0] != work {
		t.Fatalf("roots = %v, want [%s]", roots, work)
	}
	got := ListProject(work)
	if len(got) != 1 || got[0] != (Skill{Name: "status", Description: "check", Source: "project", Dir: work}) {
		t.Fatalf("ListProject = %+v", got)
	}
}
