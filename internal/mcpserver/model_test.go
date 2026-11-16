package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/store"
)

// TestStartSessionModel: start_session hands model / effort to the
// collector, reports what the session was started with (the caller's
// choice, else the settings' default), and refuses a value that could not
// be a model name.
func TestStartSessionModel(t *testing.T) {
	old := spawnWait
	spawnWait = 0
	t.Cleanup(func() { spawnWait = old })
	cfg := map[string]string{"attach_enabled": "1"}
	var got map[string]any
	starts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/settings":
			_ = json.NewEncoder(w).Encode(cfg)
		case "/pty/start":
			starts++
			got = nil
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"ptyKey":"pid-7","tag":1000001}`))
		case "/pty/":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false)
	ctx := context.Background()
	call := func(args string) map[string]any {
		t.Helper()
		out, err := s.call(ctx, "start_session", json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return m
	}

	// Chosen by the caller: sent as is, reported as is.
	out := call(`{"cwd":"/w","model":"sonnet","effort":"low"}`)
	if got["model"] != "sonnet" || got["effort"] != "low" {
		t.Errorf("request = %v", got)
	}
	if out["model"] != "sonnet" || out["effort"] != "low" || out["model_from"] != nil {
		t.Errorf("result = %v", out)
	}

	// Not chosen, no default: nothing is sent, and the result says claude picks.
	out = call(`{"cwd":"/w"}`)
	if _, ok := got["model"]; ok {
		t.Errorf("model sent without being asked: %v", got)
	}
	if _, ok := got["effort"]; ok {
		t.Errorf("effort sent without being asked: %v", got)
	}
	if out["model"] != nil || !strings.Contains(out["model_from"].(string), "claude's own default") {
		t.Errorf("result = %v", out)
	}

	// Not chosen, a default set: still nothing sent (the collector applies
	// it), but the result names it.
	cfg["default_model"], cfg["default_effort"] = "haiku", "medium"
	out = call(`{"cwd":"/w","effort":"max"}`)
	if _, ok := got["model"]; ok || got["effort"] != "max" {
		t.Errorf("request = %v", got)
	}
	if out["model"] != "haiku" || !strings.Contains(out["model_from"].(string), "Default model") || out["effort"] != "max" || out["effort_from"] != nil {
		t.Errorf("result = %v", out)
	}

	n := starts
	for _, bad := range []string{
		`{"cwd":"/w","model":"-x"}`,
		`{"cwd":"/w","model":"son net"}`,
		`{"cwd":"/w","effort":"huge"}`,
	} {
		if _, err := s.call(ctx, "start_session", json.RawMessage(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	if starts != n {
		t.Error("a refused start_session still reached the collector")
	}
}
