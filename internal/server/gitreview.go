package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Read-only git views of a session's working directory for the portal's
// diff review: GET /hub/git/status?session=<id> and
// GET /hub/git/diff?session=<id>[&path=<file>]. The directory always comes
// from the session's DB row, never from the request, and every command is
// read-only (GIT_OPTIONAL_LOCKS=0 so a status refresh can't take the index
// lock from under a running claude).

const gitTimeout = 10 * time.Second

// maxDiff bounds one diff response; bigger diffs are cut with a marker.
const maxDiff = 2 << 20

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		return nil, &gitError{msg: strings.TrimSpace(errb.String()), err: err}
	}
	return out.Bytes(), nil
}

type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.err.Error()
}

func (s *Server) sessionCwd(r *http.Request) (string, bool) {
	sess, ok, err := s.db.GetSession(r.Context(), r.URL.Query().Get("session"))
	if err != nil || !ok || sess.Cwd == "" {
		return "", false
	}
	return sess.Cwd, true
}

type gitFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // M A D R ? (porcelain XY collapsed)
	Staged  bool   `json:"staged"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// handleHubGitStatus summarizes the working tree: branch, upstream
// distance, last commit, and changed files with line counts.
func (s *Server) handleHubGitStatus(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.sessionCwd(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	root, err := runGit(r.Context(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		writeOK(w, map[string]any{"repo": false})
		return
	}
	out := map[string]any{"repo": true, "root": strings.TrimSpace(string(root))}
	if b, err := runGit(r.Context(), dir, "status", "--porcelain=v1", "-b", "-uall"); err == nil {
		files := []gitFile{}
		byPath := map[string]int{}
		for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if i == 0 && strings.HasPrefix(line, "## ") {
				parseBranchLine(strings.TrimPrefix(line, "## "), out)
				continue
			}
			if len(line) < 4 {
				continue
			}
			x, y, p := line[0], line[1], line[3:]
			if i := strings.Index(p, " -> "); i >= 0 {
				p = p[i+4:]
			}
			st := string(y)
			if y == ' ' {
				st = string(x)
			}
			if x == '?' {
				st = "?"
			}
			byPath[p] = len(files)
			files = append(files, gitFile{Path: p, Status: st, Staged: x != ' ' && x != '?'})
		}
		// Line counts against HEAD (staged + unstaged together).
		if nb, err := runGit(r.Context(), dir, "diff", "HEAD", "--numstat"); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(string(nb)), "\n") {
				f := strings.SplitN(l, "\t", 3)
				if len(f) != 3 {
					continue
				}
				i, ok := byPath[f[2]]
				if !ok {
					continue
				}
				if f[0] == "-" {
					files[i].Binary = true
					continue
				}
				files[i].Added, _ = strconv.Atoi(f[0])
				files[i].Deleted, _ = strconv.Atoi(f[1])
			}
		}
		for i := range files {
			if files[i].Status == "?" && !files[i].Binary {
				if b, err := os.ReadFile(filepath.Join(out["root"].(string), files[i].Path)); err == nil && len(b) < maxDiff {
					files[i].Added = bytes.Count(b, []byte("\n"))
				}
			}
		}
		out["files"] = files
	}
	if b, err := runGit(r.Context(), dir, "log", "-1", "--format=%h%x00%s%x00%cr"); err == nil {
		if f := strings.SplitN(strings.TrimSpace(string(b)), "\x00", 3); len(f) == 3 {
			out["head"] = map[string]string{"hash": f[0], "subject": f[1], "when": f[2]}
		}
	}
	writeOK(w, out)
}

// parseBranchLine reads "main...origin/main [ahead 1, behind 2]".
func parseBranchLine(l string, out map[string]any) {
	rest := ""
	if i := strings.Index(l, " ["); i >= 0 {
		l, rest = l[:i], l[i+2:]
	}
	branch, upstream, _ := strings.Cut(l, "...")
	out["branch"] = strings.TrimPrefix(branch, "No commits yet on ")
	if upstream != "" {
		out["upstream"] = upstream
	}
	for _, part := range strings.Split(strings.TrimSuffix(rest, "]"), ", ") {
		if n, ok := strings.CutPrefix(part, "ahead "); ok {
			out["ahead"], _ = strconv.Atoi(n)
		}
		if n, ok := strings.CutPrefix(part, "behind "); ok {
			out["behind"], _ = strconv.Atoi(n)
		}
	}
}

// handleHubGitDiff returns the unified diff of the working tree against
// HEAD — one file with ?path=, everything otherwise. Untracked files are
// shown as additions.
func (s *Server) handleHubGitDiff(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.sessionCwd(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	root, err := runGit(r.Context(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		http.Error(w, "not a git repository", http.StatusUnprocessableEntity)
		return
	}
	top := strings.TrimSpace(string(root))
	path := r.URL.Query().Get("path")
	if path != "" {
		// Keep it inside the repository.
		clean := filepath.Clean(path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		path = clean
	}
	args := []string{"diff", "HEAD", "--no-color", "--no-ext-diff", "-M"}
	if path != "" {
		args = append(args, "--", path)
	}
	diff, err := runGit(r.Context(), top, args...)
	if err != nil {
		// A repository without commits has no HEAD: diff the index instead.
		diff, _ = runGit(r.Context(), top, "diff", "--cached", "--no-color")
	}
	var buf bytes.Buffer
	buf.Write(diff)
	// Untracked files.
	if ub, err := runGit(r.Context(), top, "ls-files", "--others", "--exclude-standard"); err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(ub)), "\n") {
			if f == "" || (path != "" && f != path) || buf.Len() > maxDiff {
				continue
			}
			if nd, _ := runGit(r.Context(), top, "diff", "--no-index", "--no-color", "--", "/dev/null", f); len(nd) > 0 {
				buf.Write(nd)
			}
		}
	}
	out := buf.Bytes()
	truncated := false
	if len(out) > maxDiff {
		out, truncated = out[:maxDiff], true
	}
	writeOK(w, map[string]any{"diff": string(out), "truncated": truncated, "root": top})
}
