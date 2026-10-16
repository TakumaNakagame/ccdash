package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/yamux"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	"github.com/takumanakagame/ccmanage/internal/hubcfg"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/skills"
	"github.com/takumanakagame/ccmanage/internal/tracing"
	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

// hubLoop keeps this collector connected to the hub named in hubcfg (if
// any). It re-reads the join state and the hub_enabled setting before every
// attempt, so `ccdash hub join` / `leave` and the settings toggle apply
// without restarting the collector. No join file → it just idles.
func (s *Server) hubLoop(ctx context.Context) {
	backoff := time.Second
	lastErr := ""
	for ctx.Err() == nil {
		cfg, err := hubcfg.Load()
		wait := 30 * time.Second
		switch {
		case errors.Is(err, hubcfg.ErrNotFound):
		case err != nil:
			if err.Error() != lastErr {
				log.Printf("hub: %v", err)
				lastErr = err.Error()
			}
		case !s.hubEnabled(ctx):
		default:
			err := s.hubServeOnce(ctx, cfg)
			switch {
			case err == nil:
				// Connected, then dropped: reconnect promptly.
				backoff = time.Second
				lastErr = ""
				wait = backoff
			case errors.Is(err, tunnel.ErrUnauthorized):
				if err.Error() != lastErr {
					log.Printf("hub: %v", err)
					lastErr = err.Error()
				}
				wait = 5 * time.Minute
			default:
				if err.Error() != lastErr {
					log.Printf("hub: connect %s: %v (retrying)", cfg.URL, err)
					lastErr = err.Error()
				}
				wait = backoff
				backoff = min(backoff*2, time.Minute)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func (s *Server) hubEnabled(ctx context.Context) bool {
	cfg, err := settings.Load(ctx, s.db)
	return err != nil || cfg.HubEnabled // fail open to the default (on)
}

// hubServeOnce dials the hub and serves tunnel requests until the tunnel
// drops, ctx ends, or the join state / hub_enabled changes underneath it.
// A nil error means a session was established (and has now ended).
func (s *Server) hubServeOnce(ctx context.Context, cfg hubcfg.Config) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	host, _ := os.Hostname()
	hdr := http.Header{}
	hdr.Set(tunnel.HeaderDeviceVersion, buildinfo.Version)
	hdr.Set(tunnel.HeaderDeviceHost, host)
	sess, rh, err := tunnel.DialHeader(dialCtx, cfg.URL, cfg.Token, hdr)
	cancel()
	if err != nil {
		return err
	}
	log.Printf("hub: connected to %s", cfg.URL)
	// What the hub calls this machine; its letter prefixes session numbers.
	// An older hub sends neither: keep what we had.
	if l := rh.Get(tunnel.HeaderDeviceLetter); l != "" {
		if err := hubcfg.SetIdentity(l, rh.Get(tunnel.HeaderDeviceName)); err != nil {
			log.Printf("hub: save identity: %v", err)
		}
	}
	hs := &http.Server{Handler: tracing.Handler(s.hubHandler()), ReadHeaderTimeout: 10 * time.Second}
	stop := make(chan struct{})
	go s.hubWatch(ctx, sess, cfg, stop)
	_ = hs.Serve(sess) // returns once the session closes
	close(stop)
	_ = sess.Close()
	_ = hs.Close()
	log.Printf("hub: disconnected from %s", cfg.URL)
	return nil
}

// hubWatch closes the tunnel when ctx ends or the operator leaves / turns
// the hub off, so a disabled hub stops reaching this machine within ~10 s.
func (s *Server) hubWatch(ctx context.Context, sess *yamux.Session, cfg hubcfg.Config, stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			_ = sess.Close()
			return
		case <-t.C:
			cur, err := hubcfg.Load()
			if err != nil || cur != cfg || !s.hubEnabled(ctx) {
				_ = sess.Close()
				return
			}
		}
	}
}

// hubHandler serves requests arriving through the hub tunnel. The hub is
// authenticated by the tunnel itself (the device dialed it with its own
// device token over TLS), so requests here carry no X-Ccdash-Token; we
// inject ours after an allowlist check and hand off to the normal mux.
//
// The allowlist is the security boundary for hub mode: the hub can read
// sessions, drive ccdash-hosted PTYs, and decide approvals — each gated by
// the same device-side toggles as the TUI — but it can never reach the hook
// endpoints, /shutdown, or settings writes (so a compromised hub can't
// switch the device's own safety toggles back on).
func (s *Server) hubHandler() http.Handler { return s.tunnelHandler(false) }

// tunnelHandler is hubHandler's body. local is the local portal's link
// (`ccdash web`, localportal.go): the same allowlist and gates, but not
// the hub_enabled toggle, which governs the outbound tunnel to a hub.
func (s *Server) tunnelHandler(local bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, err := settings.Load(r.Context(), s.db)
		if err != nil {
			cfg = settings.Defaults()
		}
		if !local && !cfg.HubEnabled {
			http.Error(w, "hub connection is OFF on this device", http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hub/info":
			s.handleHubInfo(w, cfg)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/hub/upload":
			if !cfg.AttachEnabled {
				http.Error(w, "attach is OFF on this device", http.StatusForbidden)
				return
			}
			handleHubUpload(w, r)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/git/status":
			s.handleHubGitStatus(w, r)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/git/diff":
			s.handleHubGitDiff(w, r)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/file":
			s.handleHubSentFile(w, r)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/subagents":
			s.handleHubSubagents(w, r)
			return
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/hub/subagents/"):
			sid, agent, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/hub/subagents/"), "/")
			if !ok || sid == "" || strings.Contains(agent, "/") {
				http.NotFound(w, r)
				return
			}
			s.handleHubSubagentTranscript(w, r, sid, agent)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/settings":
			handleHubSettings(w, cfg)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/skills":
			if !cfg.AttachEnabled {
				http.Error(w, "attach is OFF on this device", http.StatusForbidden)
				return
			}
			transcript := ""
			if sid := r.URL.Query().Get("session"); sid != "" {
				if x, ok, err := s.db.GetSession(r.Context(), sid); err == nil && ok {
					transcript = x.TranscriptPath
				}
			}
			handleHubSkills(w, r, cfg, transcript)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/hub/dirs":
			if !cfg.AttachEnabled {
				http.Error(w, "attach is OFF on this device", http.StatusForbidden)
				return
			}
			s.handleHubDirs(w, r, cfg)
			return
		}
		if ok, why := hubAllowed(r, cfg); !ok {
			http.Error(w, why, http.StatusForbidden)
			return
		}
		r.Header.Set(auth.HeaderName, s.token)
		s.mux.ServeHTTP(w, r)
	})
}

var (
	hubReadRoute   = regexp.MustCompile(`^/api/(sessions|approvals|projects|usage|usage/sessions|sessions/[^/]+/(transcript|usage|statusline)|tasks|tasks/[^/]+)$`)
	hubSessionEdit = regexp.MustCompile(`^/api/(sessions/[^/]+/(archive|favorite|later|grid|color|title|group|project|seen)|projects/(color|rename|order))$`)
	// claude -p runs; summarize.KickoffTitles refuses them itself while
	// summary_enabled is off.
	hubTitles      = regexp.MustCompile(`^/api/titles$`)
	hubDecideRoute = regexp.MustCompile(`^/approvals/\d+/decide$`)
	// The person-sized task edits: add, edit (status, plan, archive …), a
	// note, reorder. Not sync / import: those replace the list wholesale
	// and belong to the machine's own automation.
	hubTaskEdit = regexp.MustCompile(`^/api/tasks(/[^/]+(/notes)?)?$`)
)

// hubAllowed reports whether a tunnel request may reach the mux.
func hubAllowed(r *http.Request, cfg settings.Settings) (bool, string) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && hubReadRoute.MatchString(p):
		return true, ""
	case r.Method == http.MethodPost && hubSessionEdit.MatchString(p):
		return true, ""
	case r.Method == http.MethodPost && hubTitles.MatchString(p):
		return true, ""
	case r.Method == http.MethodPost && hubTaskEdit.MatchString(p) && p != "/api/tasks/sync" && p != "/api/tasks/import":
		return true, ""
	case r.Method == http.MethodGet && (p == "/pty/" || p == "/pty" || p == "/api/restart"):
		return true, ""
	case strings.HasPrefix(p, "/pty/"), (r.Method == http.MethodPost || r.Method == http.MethodDelete) && p == "/api/restart":
		// /api/restart only re-execs the collector and resumes the claudes it
		// hosts — what DELETE /pty + /pty/start could do one by one; a
		// scheduled one ({"when":"idle"}) waits for them to be idle, DELETE
		// cancels that.
		if !cfg.AttachEnabled {
			return false, "attach is OFF on this device"
		}
		return true, ""
	case r.Method == http.MethodPost && hubDecideRoute.MatchString(p):
		if !cfg.ApproveEnabled {
			return false, "approval blocking is OFF on this device"
		}
		return true, ""
	}
	return false, "not available through the hub"
}

func (s *Server) handleHubInfo(w http.ResponseWriter, cfg settings.Settings) {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	writeOK(w, map[string]any{
		"hostname":       host,
		"version":        buildinfo.Version,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"home":           home,
		"newSessionDir":  expandHome(cfg.NewSessionDir, home),
		"approveEnabled": cfg.ApproveEnabled,
		"attachEnabled":  cfg.AttachEnabled,
		"summaryEnabled": cfg.SummaryEnabled,
		"autoRepoTabs":   cfg.AutoRepoTabs,
	})
}

// handleHubDirs lists the subdirectories of ?path= (default: the new
// session directory) for the portal's working-directory picker. Names only;
// hidden directories are skipped unless ?hidden=1.
func (s *Server) handleHubDirs(w http.ResponseWriter, r *http.Request, cfg settings.Settings) {
	home, _ := os.UserHomeDir()
	p := r.URL.Query().Get("path")
	if p == "" {
		p = expandHome(cfg.NewSessionDir, home)
	}
	p = expandHome(p, home)
	if !filepath.IsAbs(p) {
		p = filepath.Join(home, p)
	}
	p = filepath.Clean(p)
	ents, err := os.ReadDir(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	hidden := r.URL.Query().Get("hidden") == "1"
	dirs := []string{}
	for _, e := range ents {
		name := e.Name()
		if !hidden && strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, name)
		} else if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(p, name)); err == nil && fi.IsDir() {
				dirs = append(dirs, name)
			}
		}
		if len(dirs) >= 1000 {
			break
		}
	}
	sort.Strings(dirs)
	writeOK(w, map[string]any{"path": p, "parent": filepath.Dir(p), "home": home, "dirs": dirs})
}

// handleHubSettings shows the device's settings to the portal, read-only:
// the hub may see the toggles but never change them (settings writes are
// not in hubAllowed).
func handleHubSettings(w http.ResponseWriter, cfg settings.Settings) {
	type row struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Help  string `json:"help"`
		Value any    `json:"value"`
	}
	out := []row{}
	for _, sp := range settings.AllSpecs() {
		if sp.Kind == settings.KindAction {
			continue
		}
		out = append(out, row{Key: sp.Key, Label: sp.Label, Help: sp.Help, Value: settings.Get(cfg, sp.Key)})
	}
	writeOK(w, out)
}

// maxSkillDirs caps the ?dir= values /hub/skills walks up from.
const maxSkillDirs = 40

// handleHubSkills lists the skills / slash commands a new session could
// start with, like the TUI's skill picker (S): project skills found from
// ?dir= (repeatable: the portal's skill picker sends recent session
// directories, like the TUI's) and the new-session directory first, then
// the user-level ones. With ?session=<id> the list that session's own
// transcript names (Claude Code's skill_listing: bundled skills and
// plugins too) comes first; transcript is its path from the DB.
func handleHubSkills(w http.ResponseWriter, r *http.Request, cfg settings.Settings, transcript string) {
	home, _ := os.UserHomeDir()
	var dirs []string
	for i, d := range r.URL.Query()["dir"] {
		if i >= maxSkillDirs {
			break
		}
		if d != "" {
			dirs = append(dirs, expandHome(d, home))
		}
	}
	if cfg.NewSessionDir != "" {
		dirs = append(dirs, expandHome(cfg.NewSessionDir, home))
	}
	all := skills.FromTranscript(transcript)
	for _, root := range skills.ProjectRoots(dirs, home) {
		all = append(all, skills.ListProject(root)...)
	}
	all = append(all, skills.List(filepath.Join(home, ".claude"))...)
	out := []skills.Skill{}
	seen := map[string]bool{}
	for _, k := range all {
		if !seen[k.Name] {
			seen[k.Name] = true
			out = append(out, k)
		}
	}
	type row struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
		Dir         string `json:"dir,omitempty"`
	}
	rows := make([]row, 0, len(out))
	for _, s := range out {
		rows = append(rows, row{s.Name, s.Description, s.Source, s.Dir})
	}
	writeOK(w, rows)
}

// maxUpload bounds one portal upload.
const maxUpload = 20 << 20

// uploadKeep is how long uploaded files stay on disk; each upload sweeps
// older ones.
const uploadKeep = 7 * 24 * time.Hour

var uploadExt = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// handleHubUpload stores a file sent from the portal under
// $XDG_STATE_HOME/ccdash/uploads (0600) and returns its path, which the
// portal then pastes into claude's prompt — Claude Code treats a pasted
// image path like a drag-and-drop attachment, and Reads any other file
// (PDF, text, …) from the path. The body is the raw file; X-Filename
// (URL-encoded) keeps the original name, sanitized, in the stored one.
func handleHubUpload(w http.ResponseWriter, r *http.Request) {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	name := uploadName(r.Header.Get("X-Filename"), strings.TrimSpace(strings.ToLower(ct)))
	dir, err := paths.StateDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dir = filepath.Join(dir, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sweepUploads(dir)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(rnd[:])+name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, maxUpload+1))
	cerr := f.Close()
	switch {
	case err != nil || cerr != nil:
		_ = os.Remove(path)
		http.Error(w, "upload failed", http.StatusBadRequest)
		return
	case n > maxUpload:
		_ = os.Remove(path)
		http.Error(w, "file too large (max 20 MB)", http.StatusRequestEntityTooLarge)
		return
	case n == 0:
		_ = os.Remove(path)
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}
	writeOK(w, map[string]any{"path": path, "size": n})
}

// uploadName is the stored name's tail: "-<original base>.<ext>" from the
// sent file name (letters, digits, '.', '-', '_' kept, anything else —
// spaces, quotes, slashes — becomes '_', so the pasted path stays one
// shell-safe word), else just the extension the content type implies.
func uploadName(sent, ct string) string {
	if v, err := url.QueryUnescape(sent); err == nil {
		sent = v
	}
	sent = filepath.Base(strings.ReplaceAll(sent, "\\", "/"))
	if sent == "." || sent == "/" {
		sent = ""
	}
	ext := strings.ToLower(filepath.Ext(sent))
	base := strings.TrimSuffix(sent, filepath.Ext(sent))
	clean := func(s string, max int) string {
		var b strings.Builder
		n := 0
		for _, c := range s {
			if n >= max {
				break
			}
			if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_' || c == '.' {
				b.WriteRune(c)
			} else {
				b.WriteRune('_')
			}
			n++
		}
		return strings.Trim(b.String(), "._")
	}
	ext = clean(strings.TrimPrefix(ext, "."), 10)
	if ext == "" {
		if e, ok := uploadExt[ct]; ok {
			ext = e[1:]
		} else {
			ext = "bin"
		}
	}
	if base = clean(base, 60); base != "" {
		return "-" + base + "." + ext
	}
	return "." + ext
}

func sweepUploads(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-uploadKeep)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && !e.IsDir() && fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// expandHome resolves "", "~" and "~/..." against home.
func expandHome(p, home string) string {
	switch {
	case p == "" || p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}
