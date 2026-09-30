package selfupdate

import (
	"net/http"
	"sync"
	"testing"
)

func resetToken() { tokenOnce, token = sync.Once{}, "" }

func TestAPITokenPrecedence(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no gh on PATH
	t.Setenv("CCDASH_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	resetToken()
	if got := apiToken(); got != "" {
		t.Fatalf("no sources: token = %q", got)
	}

	t.Setenv("GH_TOKEN", "gh")
	t.Setenv("GITHUB_TOKEN", "github")
	resetToken()
	if got := apiToken(); got != "github" {
		t.Fatalf("GITHUB_TOKEN should beat GH_TOKEN, got %q", got)
	}
	t.Setenv("CCDASH_GITHUB_TOKEN", "ccdash")
	resetToken()
	if got := apiToken(); got != "ccdash" {
		t.Fatalf("CCDASH_GITHUB_TOKEN should win, got %q", got)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	setAPIHeaders(req)
	if req.Header.Get("Authorization") != "Bearer ccdash" {
		t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
	}
	resetToken()
}
