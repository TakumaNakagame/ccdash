package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func TestGitReview(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	s, d, _ := newTestServer(t)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "first")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\nTWO\nthree\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "sub", "new.txt"), []byte("hello\n"), 0o644)
	if err := d.UpsertSession(context.Background(), &model.Session{SessionID: "g1", Cwd: filepath.Join(repo, "sub"), Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	h := s.hubHandler()
	get := func(path string) map[string]any {
		t.Helper()
		r := doHandler(t, h, "GET", path)
		var m map[string]any
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		return m
	}
	st := get("/hub/git/status?session=g1")
	if st["branch"] != "main" || st["repo"] != true {
		t.Fatalf("status = %v", st)
	}
	raw, _ := json.Marshal(st["files"])
	var files []gitFile
	_ = json.Unmarshal(raw, &files)
	want := map[string]gitFile{"a.txt": {Path: "a.txt", Status: "M", Added: 2, Deleted: 1}, "sub/new.txt": {Path: "sub/new.txt", Status: "?", Added: 1}}
	if len(files) != 2 {
		t.Fatalf("files = %s", raw)
	}
	for _, f := range files {
		if f != want[f.Path] {
			t.Errorf("file %+v, want %+v", f, want[f.Path])
		}
	}
	diff := get("/hub/git/diff?session=g1")["diff"].(string)
	if !strings.Contains(diff, "+TWO") || !strings.Contains(diff, "+hello") {
		t.Fatalf("diff = %s", diff)
	}
	one := get("/hub/git/diff?session=g1&path=a.txt")["diff"].(string)
	if !strings.Contains(one, "+three") || strings.Contains(one, "hello") {
		t.Fatalf("single-file diff = %s", one)
	}
	if r := doHandler(t, h, "GET", "/hub/git/diff?session=g1&path=../../etc/passwd"); r.status != 400 {
		t.Errorf("escaping path = %d, want 400", r.status)
	}
}

func doHandler(t *testing.T, h interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, method, path string) recorded {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return recorded{status: w.Code, body: w.Body.Bytes()}
}
