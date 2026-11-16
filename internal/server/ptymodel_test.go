package server

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/settings"
)

// TestModelArgs: a request's own model / effort win; a new session falls
// back to the settings' defaults; a resume passes nothing by itself.
func TestModelArgs(t *testing.T) {
	def := settings.Settings{DefaultModel: "sonnet", DefaultEffort: "medium"}
	for _, c := range []struct {
		name string
		req  ptyStartReq
		cfg  settings.Settings
		want []string
	}{
		{"new, nothing set", ptyStartReq{}, settings.Settings{}, nil},
		{"new takes the defaults", ptyStartReq{}, def, []string{"--model", "sonnet", "--effort", "medium"}},
		{"explicit beats the default", ptyStartReq{Model: "opus", Effort: "max"}, def, []string{"--model", "opus", "--effort", "max"}},
		{"each falls back on its own", ptyStartReq{Model: "claude-fable-5-2[1m]"}, def, []string{"--model", "claude-fable-5-2[1m]", "--effort", "medium"}},
		{"resume ignores the defaults", ptyStartReq{ResumeID: "s1"}, def, nil},
		{"resume keeps an explicit choice", ptyStartReq{ResumeID: "s1", Model: "haiku"}, def, []string{"--model", "haiku"}},
		{"a hand-edited bad default is dropped", ptyStartReq{}, settings.Settings{DefaultModel: "-p", DefaultEffort: "huge"}, nil},
	} {
		got, err := modelArgs(c.req, c.cfg)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, bad := range []ptyStartReq{
		{Model: "-x"},
		{Model: "--dangerously-skip-permissions"},
		{Model: "son net"},
		{Model: "sonnet;rm"},
		{Model: "$(id)"},
		{Model: strings.Repeat("a", 65)},
		{Effort: "ultra"},
		{Effort: "-x"},
		{Effort: "High"},
		{ResumeID: "s1", Model: "-x"},
	} {
		if got, err := modelArgs(bad, def); err == nil {
			t.Errorf("%+v: want an error, got %q", bad, got)
		}
	}
}

// TestPTYStartModel: POST /pty/start puts the choice (or the settings'
// default) on claude's command line and answers 400 to a value that could
// read as a flag.
func TestPTYStartModel(t *testing.T) {
	calls := fakeClaude(t)
	s, d, tok := newTestServer(t)
	t.Cleanup(s.closeAllPTYs)
	ctx := context.Background()
	dir := t.TempDir()
	start := func(body string) int {
		t.Helper()
		return do(t, s, http.MethodPost, "/pty/start", tok, []byte(body)).status
	}
	last := func() string {
		t.Helper()
		c := calls()
		if len(c) == 0 {
			t.Fatal("claude was not started")
		}
		return strings.Join(c[len(c)-1], " ")
	}

	// No setting, nothing asked: claude decides. Empty fields are fine.
	if st := start(`{"cwd":"` + dir + `","model":"","effort":""}`); st != http.StatusOK {
		t.Fatalf("start: %d", st)
	}
	if got := last(); got != "" {
		t.Errorf("no model asked, no default: args %q", got)
	}

	if err := d.SetSetting(ctx, "default_model", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSetting(ctx, "default_effort", "low"); err != nil {
		t.Fatal(err)
	}
	if st := start(`{"cwd":"` + dir + `","permissionMode":"plan","prompt":"hi"}`); st != http.StatusOK {
		t.Fatalf("start: %d", st)
	}
	if got := last(); got != "--permission-mode plan --model sonnet --effort low hi" {
		t.Errorf("default: args %q", got)
	}
	if st := start(`{"cwd":"` + dir + `","model":"opus","effort":"xhigh"}`); st != http.StatusOK {
		t.Fatalf("start: %d", st)
	}
	if got := last(); got != "--model opus --effort xhigh" {
		t.Errorf("explicit: args %q", got)
	}
	if st := start(`{"sessionId":"s-res","resumeId":"s-res","cwd":"` + dir + `"}`); st != http.StatusOK {
		t.Fatalf("resume: %d", st)
	}
	if got := last(); got != "--resume s-res" {
		t.Errorf("resume: args %q", got)
	}

	n := len(calls())
	for _, bad := range []string{
		`{"cwd":"` + dir + `","model":"-x"}`,
		`{"cwd":"` + dir + `","model":"a b"}`,
		`{"cwd":"` + dir + `","effort":"extreme"}`,
	} {
		if st := start(bad); st != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, st)
		}
	}
	if len(calls()) != n {
		t.Error("a refused start still ran claude")
	}
}
