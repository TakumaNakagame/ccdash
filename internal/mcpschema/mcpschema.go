// Package mcpschema checks a tools/list result the way an MCP client does.
//
// Claude Code validates the whole result against the MCP schema and, when one
// tool fails, drops every tool of the server ("Failed to fetch tools") — the
// server still connects and its instructions still show, so nothing looks
// wrong from the server's side. v1.5.0-rc.2 shipped `"required": null` on one
// tool (a nil []string) and no session had a ccdash tool until it was fixed.
// The tests of both MCP servers run their tools/list through Check.
package mcpschema

import (
	"encoding/json"
	"fmt"
)

// Check returns what a strict client would refuse in a tools/list result
// (the JSON of {"tools": [...]}), one line per problem; none = fine.
func Check(result []byte) []string {
	var doc struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(result, &doc); err != nil {
		return []string{"result: " + err.Error()}
	}
	if doc.Tools == nil {
		return []string{"result: tools is missing or null"}
	}
	var bad []string
	seen := map[string]bool{}
	for i, tl := range doc.Tools {
		var name string
		if json.Unmarshal(tl["name"], &name) != nil || name == "" {
			bad = append(bad, fmt.Sprintf("tools.%d: name must be a non-empty string", i))
			name = fmt.Sprintf("tools.%d", i)
		}
		if seen[name] {
			bad = append(bad, name+": listed twice")
		}
		seen[name] = true
		for _, k := range []string{"description", "title"} {
			var s string
			if raw, ok := tl[k]; ok && json.Unmarshal(raw, &s) != nil {
				bad = append(bad, fmt.Sprintf("%s: %s must be a string", name, k))
			}
		}
		for _, k := range []string{"inputSchema", "outputSchema"} {
			raw, ok := tl[k]
			if !ok {
				if k == "inputSchema" {
					bad = append(bad, name+": inputSchema is missing")
				}
				continue
			}
			for _, p := range checkObjectSchema(raw) {
				bad = append(bad, fmt.Sprintf("%s: %s.%s", name, k, p))
			}
		}
		if raw, ok := tl["annotations"]; ok {
			var ann map[string]json.RawMessage
			if json.Unmarshal(raw, &ann) != nil || ann == nil {
				bad = append(bad, name+": annotations must be an object")
			}
			for k, v := range ann {
				var b bool
				var s string
				if k == "title" {
					if json.Unmarshal(v, &s) != nil {
						bad = append(bad, name+": annotations.title must be a string")
					}
				} else if json.Unmarshal(v, &b) != nil {
					bad = append(bad, fmt.Sprintf("%s: annotations.%s must be a boolean", name, k))
				}
			}
		}
		// A null anywhere is never what the schema asks for, and in Go it
		// is what a nil slice or map turns into.
		for k, raw := range tl {
			for _, p := range nulls(raw, k) {
				bad = append(bad, fmt.Sprintf("%s: %s is null", name, p))
			}
		}
	}
	return bad
}

// checkObjectSchema: {"type":"object", "properties"?: object, "required"?:
// [string…]} — the shape MCP fixes for a tool's input / output schema.
func checkObjectSchema(raw json.RawMessage) []string {
	var sc map[string]json.RawMessage
	if json.Unmarshal(raw, &sc) != nil || sc == nil {
		return []string{"must be an object"}
	}
	var bad []string
	var typ string
	if json.Unmarshal(sc["type"], &typ) != nil || typ != "object" {
		bad = append(bad, `type must be "object"`)
	}
	var props map[string]json.RawMessage
	if p, ok := sc["properties"]; ok {
		if json.Unmarshal(p, &props) != nil || props == nil {
			bad = append(bad, "properties must be an object")
		}
		for arg, v := range props {
			var o map[string]json.RawMessage
			if json.Unmarshal(v, &o) != nil || o == nil {
				bad = append(bad, fmt.Sprintf("properties.%s must be an object", arg))
			}
		}
	}
	if r, ok := sc["required"]; ok {
		var req []string
		if json.Unmarshal(r, &req) != nil || req == nil {
			bad = append(bad, "required must be an array of strings, got "+string(r))
		}
		for _, arg := range req {
			if _, ok := props[arg]; !ok {
				bad = append(bad, fmt.Sprintf("required names %q, which is not in properties", arg))
			}
		}
	}
	return bad
}

// nulls lists the paths under raw whose value is JSON null.
func nulls(raw json.RawMessage, path string) []string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case nil:
			out = append(out, path)
		case map[string]any:
			for k, c := range x {
				walk(c, path+"."+k)
			}
		case []any:
			for i, c := range x {
				walk(c, fmt.Sprintf("%s.%d", path, i))
			}
		}
	}
	walk(v, path)
	return out
}
