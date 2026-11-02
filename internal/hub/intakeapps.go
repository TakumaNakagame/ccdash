package hub

// Intake apps: the operator's own web apps hand the hub a request about
// the page someone is looking at (contrib/intake-widget). Each app has a
// bearer token whose hash is registered here together with everything the
// request may affect — the machine, directory, project, permission mode
// and whether it starts a session at once or waits as a draft for the
// operator. The caller can't choose any of those: a leaked token reaches
// only its app's repository, and a "draft" token only queues text.
//
// Big things (screenshots, the app's context, console errors) go straight
// to the target machine's upload dir (/hub/upload, swept after 7 days); the
// hub keeps only short draft rows in its state.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// IntakeApp is one app allowed to send requests (the --intake-apps-file).
type IntakeApp struct {
	Name        string `json:"name"`
	TokenSHA256 string `json:"token_sha256"`
	Owner       string `json:"owner"`
	Host        string `json:"host"`
	Cwd         string `json:"cwd"`
	Project     string `json:"project,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Action      string `json:"action,omitempty"` // "draft" (default) | "session"
}

const (
	intakeActionDraft   = "draft"
	intakeActionSession = "session"

	maxIntakeDrafts   = 20
	intakeRatePerHour = 30
	maxIntakeFiles    = 5
	maxIntakeFileSize = 20 << 20
	maxIntakeBody     = maxIntakeFiles*maxIntakeFileSize + 1<<20
)

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	intakeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// LoadIntakeApps reads and checks an apps file: {"apps": [IntakeApp, …]}.
func LoadIntakeApps(path string) ([]IntakeApp, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("intake apps: %w", err)
	}
	var f struct {
		Apps []IntakeApp `json:"apps"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("intake apps %s: %w", path, err)
	}
	if err := checkIntakeApps(f.Apps); err != nil {
		return nil, fmt.Errorf("intake apps %s: %w", path, err)
	}
	return f.Apps, nil
}

func checkIntakeApps(apps []IntakeApp) error {
	names, hashes := map[string]bool{}, map[string]bool{}
	for i := range apps {
		a := &apps[i]
		a.TokenSHA256 = strings.ToLower(strings.TrimSpace(a.TokenSHA256))
		a.Owner = strings.ToLower(strings.TrimSpace(a.Owner))
		if a.Action == "" {
			a.Action = intakeActionDraft
		}
		switch {
		case !intakeName.MatchString(a.Name):
			return fmt.Errorf("app %d: name must be 1-64 of A-Z a-z 0-9 . _ -", i+1)
		case names[a.Name]:
			return fmt.Errorf("app %q twice", a.Name)
		case !sha256Hex.MatchString(a.TokenSHA256):
			return fmt.Errorf("app %q: token_sha256 must be 64 hex characters (printf %%s \"$TOKEN\" | sha256sum)", a.Name)
		case hashes[a.TokenSHA256]:
			return fmt.Errorf("app %q: token shared with another app", a.Name)
		case !strings.Contains(a.Owner, "@"):
			return fmt.Errorf("app %q: owner must be the login (email) that sees the drafts", a.Name)
		case a.Host == "":
			return fmt.Errorf("app %q: host (device id or name) is required", a.Name)
		case !strings.HasPrefix(a.Cwd, "/") && !strings.HasPrefix(a.Cwd, "~"):
			return fmt.Errorf("app %q: cwd must be an absolute path", a.Name)
		case !intakeModes[a.Mode]:
			return fmt.Errorf("app %q: mode must be \"\", manual, acceptEdits, auto or plan", a.Name)
		case a.Action != intakeActionDraft && a.Action != intakeActionSession:
			return fmt.Errorf("app %q: action must be draft or session", a.Name)
		}
		names[a.Name], hashes[a.TokenSHA256] = true, true
	}
	return nil
}

// intakeDraft is a request waiting in the owner's portal (#/intake?draft=).
type intakeDraft struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Host      string    `json:"host"` // device id
	Cwd       string    `json:"cwd"`
	Project   string    `json:"project,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	URL       string    `json:"url,omitempty"`
	Title     string    `json:"title,omitempty"`
	Sel       string    `json:"sel,omitempty"`
	Text      string    `json:"text"`
	Submitter string    `json:"submitter,omitempty"`
	Paths     []string  `json:"paths"`           // screenshots on the device
	Notes     string    `json:"notes,omitempty"` // context + errors file on the device
	Created   time.Time `json:"created"`
}

func intakeDraftsKey(owner string) string { return "intakedrafts:" + strings.ToLower(owner) }

func (h *Hub) intakeApp(r *http.Request) *IntakeApp {
	tok, ok := bearerToken(r)
	if !ok || tok == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(tok))
	return h.intakeApps[hex.EncodeToString(sum[:])]
}

// intakeAllow is a per-app sliding window (per replica: good enough to
// stop a runaway caller, not an exact quota).
func (h *Hub) intakeAllow(app string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	ts := h.intakeRate[app][:0]
	for _, t := range h.intakeRate[app] {
		if t.After(cut) {
			ts = append(ts, t)
		}
	}
	if len(ts) >= intakeRatePerHour {
		h.intakeRate[app] = ts
		return false
	}
	h.intakeRate[app] = append(ts, time.Now())
	return true
}

func intakeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// field returns a form value trimmed and capped to max runes.
func intakeField(r *http.Request, k string, max int) (string, error) {
	v := strings.TrimSpace(r.FormValue(k))
	if !utf8.ValidString(v) {
		return "", fmt.Errorf("%s is not UTF-8", k)
	}
	if utf8.RuneCountInString(v) > max {
		return "", fmt.Errorf("%s: at most %d characters", k, max)
	}
	return v, nil
}

// handleIntakeRequest: POST /api/intake/requests (bearer app token).
func (h *Hub) handleIntakeRequest(w http.ResponseWriter, r *http.Request) {
	app := h.intakeApp(r)
	if app == nil {
		log.Printf("hub: intake: rejected token from %s", clientIP(r))
		w.Header().Set("WWW-Authenticate", `Bearer realm="ccdash-intake"`)
		intakeErr(w, http.StatusUnauthorized, "unknown app token")
		return
	}
	if !h.intakeAllow(app.Name) {
		intakeErr(w, http.StatusTooManyRequests, fmt.Sprintf("more than %d requests in an hour", intakeRatePerHour))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxIntakeBody)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			intakeErr(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		intakeErr(w, http.StatusBadRequest, "want multipart/form-data: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	d := intakeDraft{App: app.Name, Cwd: app.Cwd, Project: app.Project, Mode: app.Mode, Paths: []string{}, Created: time.Now().UTC()}
	var ctxText, errText string
	var err error
	for _, f := range []struct {
		k   string
		max int
		dst *string
	}{{"text", 4000, &d.Text}, {"url", 2000, &d.URL}, {"title", 300, &d.Title}, {"sel", 4000, &d.Sel},
		{"submitter", 200, &d.Submitter}, {"context", 16000, &ctxText}, {"errors", 8000, &errText}} {
		if *f.dst, err = intakeField(r, f.k, f.max); err != nil {
			intakeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if d.Text == "" {
		intakeErr(w, http.StatusBadRequest, "text (the request) is required")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) > maxIntakeFiles {
		intakeErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d files", maxIntakeFiles))
		return
	}
	for _, fh := range files {
		ct := strings.ToLower(strings.TrimSpace(strings.SplitN(fh.Header.Get("Content-Type"), ";", 2)[0]))
		if !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
			intakeErr(w, http.StatusBadRequest, fmt.Sprintf("%s: only images (not %q)", fh.Filename, ct))
			return
		}
		if fh.Size > maxIntakeFileSize {
			intakeErr(w, http.StatusRequestEntityTooLarge, fh.Filename+": at most 20 MB")
			return
		}
	}

	dev, ok := h.intakeDevice(r.Context(), app.Host)
	if !ok {
		intakeErr(w, http.StatusServiceUnavailable, "the target machine is not joined to this hub")
		return
	}
	d.Host = dev
	dc := h.conn(dev)
	if dc == nil {
		intakeErr(w, http.StatusServiceUnavailable, "the target machine is offline")
		return
	}
	client := &http.Client{Transport: dc.rt, Timeout: 60 * time.Second}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			intakeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		p, err := deviceUpload(r.Context(), client, f, fh.Header.Get("Content-Type"), fh.Filename)
		f.Close()
		if err != nil {
			intakeErr(w, http.StatusServiceUnavailable, "could not hand the image to the target machine: "+err.Error())
			return
		}
		d.Paths = append(d.Paths, p)
	}
	if ctxText != "" || errText != "" || d.Submitter != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "# %s: what the app reported\n\n", app.Name)
		if d.Submitter != "" {
			fmt.Fprintf(&b, "Submitted by: %s\n\n", d.Submitter)
		}
		if ctxText != "" {
			fmt.Fprintf(&b, "## Context\n\n```\n%s\n```\n\n", ctxText)
		}
		if errText != "" {
			fmt.Fprintf(&b, "## Recent console errors\n\n```\n%s\n```\n", errText)
		}
		if d.Notes, err = deviceUpload(r.Context(), client, strings.NewReader(b.String()), "text/markdown", "intake-"+app.Name+".md"); err != nil {
			intakeErr(w, http.StatusServiceUnavailable, "could not hand the notes to the target machine: "+err.Error())
			return
		}
	}

	if app.Action == intakeActionSession {
		footer := ""
		if v, err := h.st.secret(r.Context(), "intake:"+app.Owner, func() string { return "{}" }); err == nil {
			var c intakeConfig
			if json.Unmarshal([]byte(v), &c) == nil {
				footer = c.Footer
			}
		}
		key, err := devicePTYStart(r.Context(), client, d, intakeSessionPrompt(d, footer))
		if err != nil {
			intakeErr(w, http.StatusServiceUnavailable, "could not start the session: "+err.Error())
			return
		}
		u := h.cfg.PublicURL + "/#/d/" + dev + "/p/" + key
		log.Printf("hub: intake: %s started a session on %s", app.Name, dev)
		go h.push(context.WithoutCancel(r.Context()), notification{
			Title: "依頼を起動: " + app.Name, Body: firstLine(d.Text, 80),
			TitleEn: "Request started: " + app.Name, BodyEn: firstLine(d.Text, 80),
			URL: "/#/d/" + dev + "/p/" + key, Tag: "intake-" + key,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "started", "url": u})
		return
	}

	d.ID = randHex(8)
	if err := h.addIntakeDraft(r.Context(), app.Owner, d); err != nil {
		intakeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("hub: intake: %s left a draft for %s", app.Name, app.Owner)
	go h.push(context.WithoutCancel(r.Context()), notification{
		Title: "依頼の下書き: " + app.Name, Body: firstLine(d.Text, 80),
		TitleEn: "Request draft: " + app.Name, BodyEn: firstLine(d.Text, 80),
		URL: "/#/intake?draft=" + d.ID, Tag: "intake-" + d.ID,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "draft", "id": d.ID, "url": h.cfg.PublicURL + "/#/intake?draft=" + d.ID})
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(s, "\n")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// intakeDevice resolves a device id or name.
func (h *Hub) intakeDevice(ctx context.Context, host string) (string, bool) {
	ds, err := h.st.listDevices(ctx)
	if err != nil {
		return "", false
	}
	for _, d := range ds {
		if d.ID == host {
			return d.ID, true
		}
	}
	for _, d := range ds {
		if d.Name == host {
			return d.ID, true
		}
	}
	return "", false
}

// deviceUpload puts body in the device's upload dir and returns its path.
func deviceUpload(ctx context.Context, c *http.Client, body io.Reader, ct, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://device/hub/upload", body)
	if err != nil {
		return "", err
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Filename", url.QueryEscape(name))
	var out struct {
		Path string `json:"path"`
	}
	if err := doDeviceJSON(c, req, &out); err != nil {
		return "", err
	}
	if out.Path == "" {
		return "", errors.New("no path in the answer")
	}
	return out.Path, nil
}

func devicePTYStart(ctx context.Context, c *http.Client, d intakeDraft, prompt string) (string, error) {
	b, _ := json.Marshal(map[string]any{"cwd": d.Cwd, "prompt": prompt, "permissionMode": d.Mode, "project": d.Project, "cols": 120, "rows": 36})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://device/pty/start", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		PtyKey string `json:"ptyKey"`
	}
	if err := doDeviceJSON(c, req, &out); err != nil {
		return "", err
	}
	return out.PtyKey, nil
}

func doDeviceJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// intakeSessionPrompt mirrors the portal's intakePrompt (app.js) for a
// session the hub starts itself.
func intakeSessionPrompt(d intakeDraft, footer string) string {
	var b strings.Builder
	title := d.Title
	if title == "" {
		title = d.URL
	}
	fmt.Fprintf(&b, "【クイック依頼】%s（%s から）\n", title, d.App)
	if d.URL != "" {
		fmt.Fprintf(&b, "画面の URL: %s\n", d.URL)
	}
	if d.Sel != "" {
		b.WriteString("\n選択していた文字:\n")
		for _, l := range strings.Split(d.Sel, "\n") {
			b.WriteString("> " + l + "\n")
		}
	}
	if len(d.Paths) > 0 {
		b.WriteString("\n添付した画面（Read で開いて見ること）:\n")
		for _, p := range d.Paths {
			b.WriteString("- " + p + "\n")
		}
	}
	if d.Notes != "" {
		fmt.Fprintf(&b, "\nアプリが添えた状態とエラー（Read で開くこと）: %s\n", d.Notes)
	}
	fmt.Fprintf(&b, "\n依頼:\n%s\n", d.Text)
	if f := strings.TrimSpace(footer); f != "" {
		b.WriteString("\n" + f + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (h *Hub) loadIntakeDrafts(ctx context.Context, owner string) ([]intakeDraft, error) {
	v, err := h.st.secret(ctx, intakeDraftsKey(owner), func() string { return "[]" })
	if err != nil {
		return nil, err
	}
	var ds []intakeDraft
	if err := json.Unmarshal([]byte(v), &ds); err != nil {
		ds = nil
	}
	return ds, nil
}

func (h *Hub) saveIntakeDrafts(ctx context.Context, owner string, ds []intakeDraft) error {
	if ds == nil {
		ds = []intakeDraft{}
	}
	b, _ := json.Marshal(ds)
	return h.st.putKV(ctx, intakeDraftsKey(owner), string(b))
}

// addIntakeDraft keeps the newest maxIntakeDrafts. Two replicas writing at
// the same moment can lose one draft; requests are rare and a lost draft
// costs a resend, so there is no lock.
func (h *Hub) addIntakeDraft(ctx context.Context, owner string, d intakeDraft) error {
	h.intakeMu.Lock()
	defer h.intakeMu.Unlock()
	ds, err := h.loadIntakeDrafts(ctx, owner)
	if err != nil {
		return err
	}
	ds = append(ds, d)
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Created.After(ds[j].Created) })
	if len(ds) > maxIntakeDrafts {
		ds = ds[:maxIntakeDrafts]
	}
	return h.saveIntakeDrafts(ctx, owner, ds)
}

// GET /api/intake/drafts: the login's drafts, newest first.
func (h *Hub) handleIntakeDrafts(w http.ResponseWriter, r *http.Request) {
	email, _ := h.auth.user(r)
	ds, err := h.loadIntakeDrafts(r.Context(), email)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ds == nil {
		ds = []intakeDraft{}
	}
	writeJSON(w, ds)
}

// DELETE /api/intake/drafts/{id}: started or thrown away.
func (h *Hub) handleIntakeDraftDelete(w http.ResponseWriter, r *http.Request) {
	email, _ := h.auth.user(r)
	h.intakeMu.Lock()
	defer h.intakeMu.Unlock()
	ds, err := h.loadIntakeDrafts(r.Context(), email)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	out := ds[:0]
	for _, d := range ds {
		if d.ID != id {
			out = append(out, d)
		}
	}
	if err := h.saveIntakeDrafts(r.Context(), email, out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
