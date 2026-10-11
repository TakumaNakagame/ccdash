package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// TestHubSentFile: only files the session's transcript records as sent to
// the user are served; images inline, anything else as a sandboxed download.
func TestHubSentFile(t *testing.T) {
	s, d, _ := newTestServer(t)
	dir := t.TempDir()
	img, page, other := filepath.Join(dir, "shot.png"), filepath.Join(dir, "x.html"), filepath.Join(dir, "secret.txt")
	doc, code := filepath.Join(dir, "design.md"), filepath.Join(dir, "main.go")
	for _, f := range []string{img, page, other, doc, code} {
		_ = os.WriteFile(f, []byte("data:"+f), 0o600)
	}
	tp := filepath.Join(dir, "t.jsonl")
	_ = os.WriteFile(tp, []byte(`{"type":"user","toolUseResult":{"attachments":[{"path":"`+img+`","media_type":"image/png","isImage":true},{"path":"`+page+`","media_type":"text/html"}]}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"`+doc+`","content":"# x"}},{"type":"tool_use","name":"Write","input":{"file_path":"`+code+`"}}]}}`+"\n"), 0o600)
	if err := d.UpsertSession(context.Background(), &model.Session{SessionID: "f1", Cwd: dir, TranscriptPath: tp, Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	h := s.hubHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/hub/file?session=f1&path="+url.QueryEscape(path), nil))
		return w
	}
	if w := get(img); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != "data:"+img {
		t.Fatalf("image = %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if w := get(page); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/octet-stream" ||
		w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Content-Disposition")[:10] != "attachment" {
		t.Fatalf("html = %d %v", w.Code, w.Header())
	}
	// A document Claude wrote is previewable; code it wrote is not served.
	if w := get(doc); w.Code != http.StatusOK || w.Body.String() != "data:"+doc {
		t.Fatalf("written md = %d %s", w.Code, w.Body)
	}
	for _, p := range []string{other, code, "/etc/passwd", "relative.png", tp} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, w.Code)
		}
	}
}
