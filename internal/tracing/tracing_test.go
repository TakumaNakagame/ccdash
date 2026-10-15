package tracing

import (
	"net/http/httptest"
	"testing"
)

func TestRoute(t *testing.T) {
	for in, want := range map[string]string{
		"/pty/start": "/pty/start",
		"/pty/6f1c2a7e-0b4d-4c55-9a1e-3f0d2b9c8e11/input":     "/pty/{id}/input",
		"/pty/pid-4242/restart":                               "/pty/{id}/restart",
		"/approvals/17/decide":                                "/approvals/{id}/decide",
		"/api/d/a1b2c3d4e5/api/sessions/abc123def/project":    "/api/d/{id}/api/sessions/{id}/project",
		"/hooks/permission-request":                           "/hooks/permission-request",
		"/_theme/v1/theme.css":                                "/_theme/v1/theme.css",
		"/hub/subagents/6f1c2a7e-0b4d/agent-a0f3b2c1d4e5f607": "/hub/subagents/{id}/{id}",
	} {
		if got := Route(in); got != want {
			t.Errorf("Route(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTraced(t *testing.T) {
	for _, c := range []struct {
		method, path string
		upgrade      bool
		want         bool
	}{
		{"POST", "/pty/start", false, true},
		{"POST", "/api/d/x1y2z3a4/pty/start", false, true},
		{"POST", "/api/restart", false, true},
		{"POST", "/hooks/permission-request", false, true},
		{"POST", "/hooks/statusline", false, false},
		{"POST", "/hooks/pre-tool", false, false},
		{"POST", "/hooks/post-tool-failure", false, false},
		{"POST", "/api/d/x1y2z3a4/api/sessions/s1s2s3s4/seen", false, false},
		{"POST", "/pty/k/resize", false, false},
		// polling
		{"GET", "/api/me", false, false},
		{"GET", "/api/board", false, false},
		{"GET", "/api/d/x1y2z3a4/api/sessions", false, false},
		{"GET", "/api/sessions", false, false},
		{"GET", "/pty/", false, false},
		{"GET", "/healthz", false, false},
		// streams
		{"GET", "/pty/k/screen", true, false},
		{"GET", "/auth/callback", true, false},
		// worth a span
		{"GET", "/auth/callback", false, true},
		{"GET", "/s/6f1c2a7e", false, true},
		{"GET", "/api/d/x1y2z3a4/hub/git/diff", false, true},
		{"GET", "/hub/git/diff", false, true},
		{"GET", "/api/usage", false, true},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		if got := Traced(r); got != c.want {
			t.Errorf("Traced(%s %s upgrade=%v) = %v, want %v", c.method, c.path, c.upgrade, got, c.want)
		}
	}
}

func TestStartOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if Enabled() {
		t.Fatal("Enabled with no endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	if !Enabled() {
		t.Fatal("not Enabled with an endpoint")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if Enabled() {
		t.Fatal("Enabled with OTEL_SDK_DISABLED=true")
	}
}
