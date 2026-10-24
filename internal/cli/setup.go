package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/claudecfg"
	"github.com/TakumaNakagame/ccdash/internal/hookcfg"
)

// statusToolPerm is the one ccdash MCP tool every session calls (the status
// rule), so it runs without a permission prompt.
const statusToolPerm = "mcp__ccdash__set_status"

// setupCmd wires Claude Code to ccdash in one go: the hooks (+ status line
// relay), the MCP server at user scope — whose instructions carry the status
// rule into every session — and the permission for set_status.
func setupCmd() *cobra.Command {
	var settingsPath, claudeJSON string
	var readOnly, noMCP bool
	c := &cobra.Command{
		Use:   "setup",
		Short: "Set Claude Code up for ccdash: hooks, the MCP server and the status rule for every session",
		Long: `Does everything Claude Code needs for ccdash, once per machine (safe to re-run):

  1. hooks + status line relay in ~/.claude/settings.json   (same as install-hooks)
  2. the ccdash MCP server, user scope, in ~/.claude.json    (claude mcp add --scope user ccdash -- ccdash mcp)
  3. permissions.allow += mcp__ccdash__set_status

The MCP server tells every session the status rule: before ending a turn, declare with
set_status whether it waits for your answer (回答待ち), for you to do something (作業待ち),
just reports (報告) or carries on by itself (自動で続行). ccdash shows that on the list,
the portal and the app, and answer / action make the session "needs you".
Restart running claude sessions to pick it up.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := hookcfg.DefaultInstall()
			if err != nil {
				return err
			}
			if settingsPath != "" {
				in.Path = settingsPath
			}
			changed, err := in.Apply()
			if err != nil {
				return fmt.Errorf("hooks: %w", err)
			}
			report("hooks + status line", in.Path, changed)

			if !noMCP {
				exe, err := setupExecutable()
				if err != nil {
					return err
				}
				if claudeJSON == "" {
					if claudeJSON, err = claudecfg.Path(); err != nil {
						return err
					}
				}
				mcpArgs := []string{"mcp"}
				if readOnly {
					mcpArgs = append(mcpArgs, "--read-only")
				}
				changed, err = claudecfg.AddMCPServer(claudeJSON, "ccdash", exe, mcpArgs)
				if err != nil {
					return fmt.Errorf("mcp server: %w", err)
				}
				report("MCP server ("+exe+" "+strings.Join(mcpArgs, " ")+")", claudeJSON, changed)
				changed, err = hookcfg.AllowTools(in.Path, []string{statusToolPerm})
				if err != nil {
					return fmt.Errorf("permissions: %w", err)
				}
				report("allow "+statusToolPerm, in.Path, changed)
			}
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  1) start the collector if it isn't running:  ccdash server  (or just `ccdash`)")
			fmt.Println("  2) restart running claude sessions — new ones follow the status rule")
			return nil
		},
	}
	c.Flags().StringVar(&settingsPath, "settings", "", "override settings.json path (default: ~/.claude/settings.json)")
	c.Flags().StringVar(&claudeJSON, "claude-json", "", "override Claude Code's config path (default: ~/.claude.json)")
	c.Flags().BoolVar(&readOnly, "read-only-mcp", false, "register the MCP server with --read-only (viewing tools + set_status)")
	c.Flags().BoolVar(&noMCP, "no-mcp", false, "only install the hooks")
	return c
}

func report(what, path string, changed bool) {
	state := "already set"
	if changed {
		state = "updated"
	}
	fmt.Printf("✓ %-40s %s (%s)\n", what, state, path)
}

// setupExecutable is the path the MCP entry runs: this binary, symlinks
// resolved — but not a `go run` build, which is gone once it exits.
func setupExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", fmt.Errorf("run setup from an installed ccdash (go install ./cmd/ccdash), not go run")
	}
	return exe, nil
}
