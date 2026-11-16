package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// noteCollector plays a collector for start_session: it records each
// /pty/start body and answers like a current collector (info != nil) or
// an older one that knows neither the notes nor /api/start-info.
type noteCollector struct {
	info    *store.StartInfo
	starts  []map[string]any
	probes  []string
	failure string
}

func (c *noteCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/settings":
		_ = json.NewEncoder(w).Encode(map[string]string{"attach_enabled": "1"})
	case "/api/start-info":
		if c.info == nil {
			http.NotFound(w, r)
			return
		}
		c.probes = append(c.probes, r.URL.Query().Get("project"))
		_ = json.NewEncoder(w).Encode(c.info)
	case "/pty/start":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.starts = append(c.starts, b)
		if c.failure != "" {
			http.Error(w, c.failure, http.StatusBadGateway)
			return
		}
		if c.info == nil {
			_, _ = w.Write([]byte(`{"ptyKey":"pid-42","tag":1000001}`))
			return
		}
		cwd, _ := b["cwd"].(string)
		if cwd == "" {
			cwd = c.info.Dir
		}
		notes := []string{}
		if b["plain"] != true && b["prompt"] != nil {
			notes = []string{"common", "project"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ptyKey": "pid-42", "tag": 1000001, "orchestrated": true, "notes": notes, "cwd": cwd})
	case "/pty/":
		_, _ = w.Write([]byte(`[]`))
	default:
		http.NotFound(w, r)
	}
}

func (c *noteCollector) last() map[string]any { return c.starts[len(c.starts)-1] }

// TestStartSessionNotes: start_session marks its start as orchestrated,
// lets the collector find the directory when only the project is given,
// and reports which notes were appended and where claude started.
func TestStartSessionNotes(t *testing.T) {
	fastSpawn(t)
	c := &noteCollector{info: &store.StartInfo{Dir: "/w/web"}}
	ts := httptest.NewServer(c)
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false)
	s.self = func() string { return "boss" }

	// Project only: no cwd is sent, the collector's directory comes back.
	out := call(t, s, "start_session", `{"project":"web","prompt":"ログイン画面を直す"}`)
	if b := c.last(); b["orchestrated"] != true || b["cwd"] != "" || b["plain"] != nil || b["project"] != "web" || b["prompt"] != "ログイン画面を直す" {
		t.Errorf("request = %v", b)
	}
	if len(c.probes) != 1 || c.probes[0] != "web" {
		t.Errorf("start-info asked %v", c.probes)
	}
	for _, want := range []string{`"cwd": "/w/web"`, `"common"`, `"project"`, `"notes": [`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, "notes_note") {
		t.Errorf("a current collector needs no notes_note: %s", out)
	}

	// An explicit cwd wins and needs no question first; plain is passed on
	// and nothing is appended.
	out = call(t, s, "start_session", `{"cwd":"/w/other/","project":"web","prompt":"そのまま","plain":true}`)
	if b := c.last(); b["cwd"] != "/w/other" || b["plain"] != true || b["orchestrated"] != true {
		t.Errorf("request = %v", b)
	}
	if len(c.probes) != 1 || !strings.Contains(out, `"notes": []`) || !strings.Contains(out, `"cwd": "/w/other"`) {
		t.Errorf("plain: probes=%v out=%s", c.probes, out)
	}

	// Nowhere to start: refused before anything is sent.
	n := len(c.starts)
	c.info = &store.StartInfo{}
	if e := callErr(t, s, "start_session", `{"project":"nodir","prompt":"x"}`); !strings.Contains(e, `project "nodir" has no working directory`) {
		t.Errorf("no dir: %s", e)
	}
	if e := callErr(t, s, "start_session", `{"prompt":"x"}`); !strings.Contains(e, "cwd is required") {
		t.Errorf("no cwd, no project: %s", e)
	}
	if len(c.starts) != n {
		t.Errorf("a refused start reached the collector: %v", c.starts[n:])
	}
	// …unless the operator's setup command may name the directory.
	c.info = &store.StartInfo{Setup: true}
	call(t, s, "start_session", `{"project":"nodir","prompt":"x"}`)
	if b := c.last(); len(c.starts) != n+1 || b["cwd"] != "" || b["orchestrated"] != true {
		t.Errorf("setup decides: %v", c.starts[n:])
	}
	// A failed setup command is the start's error, as the collector put it.
	c.failure = "the setup command (setting session_setup_command) failed (exit status 3); nothing was started: could not clone"
	if e := callErr(t, s, "start_session", `{"cwd":"/w/web","prompt":"x"}`); !strings.Contains(e, "could not clone") {
		t.Errorf("setup failure: %s", e)
	}
	c.failure = ""

	// An older collector: a start with a cwd still works and says nothing
	// was appended; one without a cwd is refused rather than left to start
	// in the collector's own directory.
	c.info = nil
	n = len(c.starts)
	out = call(t, s, "start_session", `{"cwd":"/w/web","project":"web","prompt":"x"}`)
	if !strings.Contains(out, `"notes": []`) || !strings.Contains(out, "notes_note") {
		t.Errorf("older collector: %s", out)
	}
	if e := callErr(t, s, "start_session", `{"project":"web","prompt":"x"}`); !strings.Contains(e, "cwd is required") {
		t.Errorf("older collector without cwd: %s", e)
	}
	if len(c.starts) != n+1 {
		t.Errorf("older collector got a start with no cwd: %v", c.starts[n:])
	}
}

// TestStartSessionNotesOtherHost: through the hub the target host's
// directory and notes apply — the start goes there orchestrated, with no
// cwd of this machine's.
func TestStartSessionNotesOtherHost(t *testing.T) {
	s, f, _ := multiHostServer(t, true)
	fastSpawn(t)
	f.startInfo = &store.StartInfo{Dir: "/srv/infra"}
	out := call(t, s, "start_session", `{"host":"server","project":"infra","prompt":"ノードを入れ替える"}`)
	if len(f.started) != 1 || f.started[0]["orchestrated"] != true || f.started[0]["cwd"] != "" {
		t.Fatalf("request to server = %v", f.started)
	}
	for _, want := range []string{`"host": "server"`, `"cwd": "/srv/infra"`, `"common"`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s: %s", want, out)
		}
	}
	f.startInfo = nil
	if e := callErr(t, s, "start_session", `{"host":"server","project":"infra","prompt":"x"}`); !strings.Contains(e, "collector on server") || !strings.Contains(e, "cwd is required") {
		t.Errorf("older collector on the other host: %s", e)
	}
}

// TestListProjectsDir: each host's row carries the project's directory —
// the operator's, or as a hint where its newest session ran — and whether
// it has a note; the note's text is not shown.
func TestListProjectsDir(t *testing.T) {
	s, f, d := multiHostServer(t, true)
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := d.SetProjectDir(ctx, "web", dir); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProjectBrief(ctx, "web", "この文は一覧に出ない"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProject(ctx, "lap-1", "site"); err != nil {
		t.Fatal(err)
	}
	f.projects = []model.Project{{Name: "web", RecentDir: "/srv/web"}}

	var res struct {
		Projects []projectRow
	}
	out := call(t, s, "list_projects", `{}`)
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]projectRow{}
	for _, p := range res.Projects {
		got[p.Host+"/"+p.Name] = p
	}
	if p := got["laptop/web"]; p.Dir != dir || p.DirFrom != "" || !p.HasBrief {
		t.Errorf("laptop/web = %+v", p)
	}
	if p := got["laptop/site"]; p.Dir != "/home/me/web" || p.DirFrom != "recent" || p.HasBrief {
		t.Errorf("laptop/site = %+v", p)
	}
	if p := got["server/web"]; p.Dir != "/srv/web" || p.DirFrom != "recent" || p.HasBrief {
		t.Errorf("server/web = %+v", p)
	}
	if strings.Contains(out, "この文は一覧に出ない") {
		t.Errorf("list_projects shows the note's text: %s", out)
	}
}

// TestNoToolWritesNotes: the notes and a project's directory are the
// operator's to write; no tool takes them.
func TestNoToolWritesNotes(t *testing.T) {
	for _, list := range [][]map[string]any{readTools, writeTools} {
		for _, tool := range list {
			name, _ := tool["name"].(string)
			if strings.Contains(name, "brief") || strings.Contains(name, "note") && !strings.Contains(name, "task") || strings.HasSuffix(name, "_dir") {
				t.Errorf("tool %s looks like it writes the operator's notes", name)
			}
			props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			for arg := range props {
				if arg == "brief" || arg == "dir" || arg == "common" {
					t.Errorf("tool %s takes %q", name, arg)
				}
			}
		}
	}
}
