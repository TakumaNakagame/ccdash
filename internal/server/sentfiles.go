package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// inlineImage lists the types the portal may show inline. Anything else —
// including SVG and HTML, which could run script on the hub's origin — is
// served as a download.
var inlineImage = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
}

// handleHubSentFile serves a file Claude sent to the operator with
// SendUserFile (images, APKs, reports…) so the portal can show it like
// the Claude app does. GET /hub/file?session=<id>&path=<absolute path>.
//
// Only a path recorded as an attachment of that session's own transcript
// (a toolUseResult.attachments[].path) is served — the request can't name
// any other file on the device.
func (s *Server) handleHubSentFile(w http.ResponseWriter, r *http.Request) {
	sid, path := r.URL.Query().Get("session"), r.URL.Query().Get("path")
	sess, ok, err := s.db.GetSession(r.Context(), sid)
	if err != nil || !ok || sess.TranscriptPath == "" || path == "" || !filepath.IsAbs(path) {
		http.NotFound(w, r)
		return
	}
	media, sent := sentAttachment(sess.TranscriptPath, path)
	if !sent {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "the file is gone from the device", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if media == "" {
		media = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	media, _, _ = strings.Cut(media, ";")
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, max-age=3600")
	name := filepath.Base(path)
	if inlineImage[media] {
		h.Set("Content-Type", media)
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// sentAttachment reports whether transcript records path as a file sent to
// the user, and its media type.
func sentAttachment(transcript, path string) (string, bool) {
	f, err := os.Open(transcript)
	if err != nil {
		return "", false
	}
	defer f.Close()
	quoted, _ := json.Marshal(path)
	needle := quoted[1 : len(quoted)-1] // the path as it appears inside JSON
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"attachments"`)) || !bytes.Contains(line, needle) {
			continue
		}
		var e struct {
			ToolUseResult struct {
				Attachments []struct {
					Path      string `json:"path"`
					MediaType string `json:"media_type"`
				} `json:"attachments"`
			} `json:"toolUseResult"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		for _, a := range e.ToolUseResult.Attachments {
			if a.Path == path {
				return a.MediaType, true
			}
		}
	}
	return "", false
}
