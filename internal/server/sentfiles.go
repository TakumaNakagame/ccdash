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

// previewExt are the document types the portal previews (Markdown and HTML
// reports are what sessions write most, after code) — files Claude wrote
// or edited in the session are served for those only.
var previewExt = map[string]bool{".md": true, ".markdown": true, ".html": true, ".htm": true, ".txt": true}

// handleHubSentFile serves a file Claude sent to the operator with
// SendUserFile (images, APKs, reports…) or a document it wrote, so the
// portal can show / preview it. GET /hub/file?session=<id>&path=<abs path>.
//
// Only a path the session's own transcript records is served: a
// SendUserFile attachment (toolUseResult.attachments[].path), or the
// file_path of a Write / Edit / MultiEdit with a previewExt extension. The
// request can't name any other file on the device.
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
// the user (or, for previewExt types, written by Claude), and its media
// type when known.
func sentAttachment(transcript, path string) (string, bool) {
	writable := previewExt[strings.ToLower(filepath.Ext(path))]
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
		if !bytes.Contains(line, needle) || !(bytes.Contains(line, []byte(`"attachments"`)) || writable && bytes.Contains(line, []byte(`"file_path"`))) {
			continue
		}
		var e struct {
			ToolUseResult json.RawMessage `json:"toolUseResult"`
			Message       struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		var res struct {
			Attachments []struct {
				Path      string `json:"path"`
				MediaType string `json:"media_type"`
			} `json:"attachments"`
		}
		if json.Unmarshal(e.ToolUseResult, &res) == nil {
			for _, a := range res.Attachments {
				if a.Path == path {
					return a.MediaType, true
				}
			}
		}
		if !writable {
			continue
		}
		var parts []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				FilePath string `json:"file_path"`
			} `json:"input"`
		}
		if json.Unmarshal(e.Message.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "tool_use" && (p.Name == "Write" || p.Name == "Edit" || p.Name == "MultiEdit") && p.Input.FilePath == path {
				return "", true
			}
		}
	}
	return "", false
}
