// Package skills lists the Claude Code skills and slash commands installed
// for the current user, so the TUI can offer them in a picker and start a
// fresh session with `claude "/<name>"`.
//
// Sources, in listing order:
//   - ~/.claude/skills/<name>/SKILL.md            → <name>
//   - ~/.claude/skills/synced/<id>/<name>/SKILL.md → anthropic-skills:<name>
//   - ~/.claude/commands/[<dir>/]<name>.md        → [<dir>:]<name>
//   - enabled plugins (installed_plugins.json + settings.json
//     enabledPlugins): <installPath>/skills/<name>/SKILL.md and
//     <installPath>/commands/<name>.md → <plugin>:<name>
//
// ListProject adds a project's own <dir>/.claude/skills and
// <dir>/.claude/commands; those only exist for a session started in that
// directory, so each carries its Dir.
//
// Everything is best-effort: unreadable files are skipped, never errors.
package skills

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one invocable entry.
type Skill struct {
	Name        string // what follows the slash, e.g. "code-flow:ship-pr"
	Description string // first line-ish of the frontmatter description
	Source      string // "user", "plugin", "synced", "command", "project"
	Dir         string // project root for Source "project"; "" otherwise
}

// List scans claudeDir (normally ~/.claude) and returns the skills sorted
// by name with duplicates dropped.
func List(claudeDir string) []Skill {
	var out []Skill
	out = append(out, skillDirs(filepath.Join(claudeDir, "skills"), "", "user")...)
	if ids, err := os.ReadDir(filepath.Join(claudeDir, "skills", "synced")); err == nil {
		for _, id := range ids {
			if id.IsDir() {
				out = append(out, skillDirs(filepath.Join(claudeDir, "skills", "synced", id.Name()), "anthropic-skills:", "synced")...)
			}
		}
	}
	out = append(out, commandFiles(filepath.Join(claudeDir, "commands"), "", "command")...)
	for _, p := range enabledPlugins(claudeDir) {
		out = append(out, skillDirs(filepath.Join(p.path, "skills"), p.name+":", "plugin")...)
		out = append(out, commandFiles(filepath.Join(p.path, "commands"), p.name+":", "plugin")...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	seen := map[string]bool{}
	dedup := out[:0]
	for _, s := range out {
		if !seen[s.Name] {
			seen[s.Name] = true
			dedup = append(dedup, s)
		}
	}
	return dedup
}

// ListProject returns the skills and commands defined under
// <dir>/.claude, sorted by name, each tagged with Dir.
func ListProject(dir string) []Skill {
	base := filepath.Join(dir, ".claude")
	out := append(skillDirs(filepath.Join(base, "skills"), "", "project"),
		commandFiles(filepath.Join(base, "commands"), "", "project")...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for i := range out {
		out[i].Dir = dir
	}
	return out
}

// ProjectRoots returns the directories among dirs and their ancestors
// (stopping before home, whose .claude is the user-level one) that hold a
// .claude/skills or .claude/commands directory. Order follows dirs, each
// root reported once.
func ProjectRoots(dirs []string, home string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		for d = filepath.Clean(d); d != home && d != filepath.Dir(d); d = filepath.Dir(d) {
			if seen[d] {
				break // this dir and everything above it were already checked
			}
			seen[d] = true
			if isDir(filepath.Join(d, ".claude", "skills")) || isDir(filepath.Join(d, ".claude", "commands")) {
				out = append(out, d)
			}
		}
	}
	return out
}

// validName reports whether name works as a /command: non-empty and
// only letters, digits, '-', '_', '.' or ':'.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// skillDirs reads <dir>/<name>/SKILL.md entries.
func skillDirs(dir, prefix, source string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		name, desc := frontmatter(path)
		if !validName(name) {
			// A display-style name ("Datadog Alert Improvement") isn't
			// typeable as a slash command; the directory name is.
			name = e.Name()
		}
		out = append(out, Skill{Name: prefix + name, Description: desc, Source: source})
	}
	return out
}

// commandFiles reads <dir>/**/<name>.md slash commands; a subdirectory
// becomes a "<sub>:" namespace like Claude Code does.
func commandFiles(dir, prefix, source string) []Skill {
	var out []Skill
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, strings.TrimSuffix(path, ".md"))
		if err != nil {
			return nil
		}
		_, desc := frontmatter(path)
		name := strings.ReplaceAll(filepath.ToSlash(rel), "/", ":")
		out = append(out, Skill{Name: prefix + name, Description: desc, Source: source})
		return nil
	})
	return out
}

type plugin struct{ name, path string }

// enabledPlugins returns the install paths of plugins switched on in
// settings.json. Plugin keys look like "<plugin>@<marketplace>".
func enabledPlugins(claudeDir string) []plugin {
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if b, err := os.ReadFile(filepath.Join(claudeDir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &settings)
	}
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if b, err := os.ReadFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json")); err == nil {
		_ = json.Unmarshal(b, &installed)
	}
	var out []plugin
	for key, on := range settings.EnabledPlugins {
		inst := installed.Plugins[key]
		if !on || len(inst) == 0 || inst[0].InstallPath == "" {
			continue
		}
		name, _, _ := strings.Cut(key, "@")
		out = append(out, plugin{name: name, path: inst[0].InstallPath})
	}
	return out
}

// frontmatter pulls name and description out of a Markdown file's YAML
// frontmatter. Only the shapes skills actually use are handled: plain
// `key: value` and folded/literal blocks (`key: >` / `key: |`).
func frontmatter(path string) (name, desc string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	var block *string // key whose indented continuation lines we're collecting
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if block != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			*block = strings.TrimSpace(*block + " " + strings.TrimSpace(line))
			continue
		}
		block = nil
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		var dst *string
		switch strings.TrimSpace(key) {
		case "name":
			dst = &name
		case "description":
			dst = &desc
		default:
			continue
		}
		if strings.HasPrefix(val, ">") || strings.HasPrefix(val, "|") {
			block = dst
			continue
		}
		*dst = strings.Trim(val, `"'`)
	}
	return name, desc
}
