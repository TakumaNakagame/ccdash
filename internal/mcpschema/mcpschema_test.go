package mcpschema

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	ok := `{"tools":[{"name":"a","description":"d","inputSchema":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]},"annotations":{"readOnlyHint":true}},
		{"name":"b","inputSchema":{"type":"object"}}]}`
	if bad := Check([]byte(ok)); len(bad) != 0 {
		t.Errorf("a valid list was refused: %v", bad)
	}
	for name, c := range map[string]struct{ in, want string }{
		// The v1.5.0-rc.2 failure, as Claude Code reported it:
		// "tools.10.inputSchema.required: expected array, received null".
		"required null":     {`{"tools":[{"name":"a","inputSchema":{"type":"object","properties":{},"required":null}}]}`, "required must be an array"},
		"required unknown":  {`{"tools":[{"name":"a","inputSchema":{"type":"object","properties":{},"required":["x"]}}]}`, `"x"`},
		"properties null":   {`{"tools":[{"name":"a","inputSchema":{"type":"object","properties":null}}]}`, "properties must be an object"},
		"not an object":     {`{"tools":[{"name":"a","inputSchema":{"type":"string"}}]}`, `type must be "object"`},
		"no schema":         {`{"tools":[{"name":"a"}]}`, "inputSchema is missing"},
		"no name":           {`{"tools":[{"inputSchema":{"type":"object"}}]}`, "name must be"},
		"twice":             {`{"tools":[{"name":"a","inputSchema":{"type":"object"}},{"name":"a","inputSchema":{"type":"object"}}]}`, "listed twice"},
		"nested null":       {`{"tools":[{"name":"a","inputSchema":{"type":"object","properties":{"x":{"type":"string","enum":null}}}}]}`, "inputSchema.properties.x.enum is null"},
		"annotation string": {`{"tools":[{"name":"a","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"yes"}}]}`, "must be a boolean"},
		"tools null":        {`{"tools":null}`, "tools is missing"},
	} {
		if bad := Check([]byte(c.in)); !strings.Contains(strings.Join(bad, "\n"), c.want) {
			t.Errorf("%s: got %v, want something with %q", name, bad, c.want)
		}
	}
}
