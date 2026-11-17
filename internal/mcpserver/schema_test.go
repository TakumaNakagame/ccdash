package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/mcpschema"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// TestToolSchemas: every variant of the server answers tools/list with
// something a strict MCP client takes. Claude Code drops ALL of a server's
// tools when one fails its schema check — v1.5.0-rc.2 wrote
// `"required": null` for start_session and no session had a ccdash tool.
// It reads the bytes that go over the wire, not the Go maps: the null came
// from how a nil slice is written.
func TestToolSchemas(t *testing.T) {
	st := store.NewRemote("http://127.0.0.1:1", "x")
	for name, s := range map[string]*Server{
		"full":      New(st, "test", false),
		"read-only": New(st, "test", true),
		"fleet":     New(st, "test", false).WithHubURL("http://127.0.0.1:1").WithFleet(NewFleet("http://127.0.0.1:1", "ccdh_tok")),
	} {
		var out bytes.Buffer
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
		if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out.String())
		}
		for _, bad := range mcpschema.Check(reply.Result) {
			t.Errorf("%s: %s", name, bad)
		}
	}
}
