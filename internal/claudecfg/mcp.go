package claudecfg

import (
	"errors"
	"os"
	"reflect"
)

// AddMCPServer registers a user-scope MCP server (mcpServers[name] in the
// config at path, what `claude mcp add --scope user` writes) and reports
// whether the file changed. An existing entry under that name is replaced
// only when it differs. A missing file is created holding just the entry.
func AddMCPServer(path, name, command string, args []string) (bool, error) {
	cfg, err := load(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg, err = map[string]any{}, nil
	}
	if err != nil {
		return false, err
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		cfg["mcpServers"] = servers
	}
	as := make([]any, len(args))
	for i, a := range args {
		as[i] = a
	}
	want := map[string]any{"type": "stdio", "command": command, "args": as, "env": map[string]any{}}
	if reflect.DeepEqual(servers[name], want) {
		return false, nil
	}
	servers[name] = want
	return true, save(path, cfg)
}
