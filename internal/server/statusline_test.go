package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestStatusLineRelay: the relayed status JSON is kept per session and
// served back; one without a session id is ignored.
func TestStatusLineRelay(t *testing.T) {
	s, _, tok := newTestServer(t)
	body := `{"session_id":"s1","model":{"display_name":"Opus"},"cost":{"total_cost_usd":1.5},"context_window":{"context_window_size":1000000,"used_percentage":35}}`
	if r := do(t, s, http.MethodPost, "/hooks/statusline", tok, []byte(body)); r.status != http.StatusOK {
		t.Fatalf("post: %d %s", r.status, r.body)
	}
	do(t, s, http.MethodPost, "/hooks/statusline", tok, []byte(`{"model":{}}`))
	if r := do(t, s, http.MethodPost, "/hooks/statusline", "", []byte(body)); r.status != http.StatusUnauthorized {
		t.Errorf("no token = %d", r.status)
	}
	r := do(t, s, http.MethodGet, "/api/sessions/s1/statusline", tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("get: %d", r.status)
	}
	var got struct {
		Data struct {
			ContextWindow struct {
				Size int `json:"context_window_size"`
			} `json:"context_window"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.body, &got); err != nil || got.Data.ContextWindow.Size != 1000000 {
		t.Fatalf("body = %s", r.body)
	}
	if r := do(t, s, http.MethodGet, "/api/sessions/nope/statusline", tok, nil); r.status != http.StatusNotFound || strings.Contains(string(r.body), "Opus") {
		t.Errorf("unknown session = %d", r.status)
	}
}
