package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/claudecfg"
	"github.com/TakumaNakagame/ccdash/internal/db"
	"github.com/TakumaNakagame/ccdash/internal/settings"
)

// ignoreCmd edits the ignore_dirs setting (folders whose automated claude
// runs ccdash never records) and prunes rows such runs left behind.
func ignoreCmd(rf *remoteFlags) *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:   "ignore [dir...]",
		Short: "List / add / remove ignored folders (automated claude -p runs there never show in ccdash)",
		Long: `Headless claude runs (claude -p) in the folders listed here (and below them) are
never recorded: no row in the list, the archive, MCP or the portal. Sessions
ccdash starts there itself ('n', the portal, MCP start_session) still show.

A script can instead opt one run out with CCDASH_IGNORE=1 in claude's environment.

  ccdash ignore                         # list
  ccdash ignore ~/nudge-inbox           # add
  ccdash ignore --remove ~/old          # remove
  ccdash ignore prune                   # list rows earlier automated runs left
  ccdash ignore prune --yes             # delete them

Same as "Ignored folders" on the TUI settings page.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			cfg, err := settings.Load(ctx, st)
			if err != nil {
				return err
			}
			cur := claudecfg.SplitRoots(cfg.IgnoreDirs)
			if len(args) > 0 {
				home, _ := os.UserHomeDir()
				for _, a := range args {
					abs := claudecfg.SplitRoots(a)
					if len(abs) == 0 {
						if p, err := filepath.Abs(a); err == nil {
							abs = []string{p}
						}
					}
					for _, d := range abs {
						if !remove && (d == "/" || d == home) {
							return fmt.Errorf("%s would hide every session; pick the automation's own folder", d)
						}
						cur = slices.DeleteFunc(cur, func(x string) bool { return x == d })
						if !remove {
							cur = append(cur, d)
						}
					}
				}
				if _, err := settings.Set(ctx, st, cfg, "ignore_dirs", strings.Join(cur, ", ")); err != nil {
					return err
				}
			}
			if len(cur) == 0 {
				fmt.Println("(no ignored folders)")
			}
			for _, d := range cur {
				fmt.Println(d)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "remove the given folders instead of adding them")
	c.AddCommand(ignorePruneCmd())
	return c
}

func ignorePruneCmd() *cobra.Command {
	var yes bool
	var dirs []string
	c := &cobra.Command{
		Use:   "prune",
		Short: "List (or with --yes delete) rows left by automated claude runs",
		Long: `Lists stored sessions that were automated runs: headless (claude -p) rows
with no prompt and no transcript on disk (claude mcp list and other
subcommands), and
headless rows under the ignored folders (plus --dir). Rows you kept — pinned,
in a project, renamed, watch later, on the grid, linked to a task — running
ones and ones ccdash started are never listed. With --yes they are deleted and
remembered as ignored, so the transcript scan doesn't bring them back.

Run it on the machine whose collector holds the rows (it opens the local DB).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB()
			if err != nil {
				return err
			}
			defer d.Close()
			ctx := cmd.Context()
			cfg, err := settings.Load(ctx, d)
			if err != nil {
				return err
			}
			roots := cfg.IgnoreDirs
			for _, x := range dirs {
				if p, err := filepath.Abs(x); err == nil && !strings.HasPrefix(x, "~") {
					x = p
				}
				roots += "," + x
			}
			rows, err := d.PruneCandidates(ctx,
				func(cwd string) bool { return claudecfg.Under(cwd, roots) },
				func(p string) bool { _, err := os.Stat(p); return err == nil })
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Println("(nothing to prune)")
				return nil
			}
			printPrune(rows)
			if !yes {
				fmt.Printf("\n%d rows. Run again with --yes to delete them.\n", len(rows))
				return nil
			}
			n := 0
			for _, r := range rows {
				ok, err := d.IgnoreSession(ctx, r.SessionID)
				if err != nil {
					return err
				}
				if ok {
					n++
				}
			}
			fmt.Printf("\ndeleted %d rows.\n", n)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "delete the listed rows")
	c.Flags().StringSliceVar(&dirs, "dir", nil, "also treat headless rows under this folder as automated (repeatable)")
	return c
}

func printPrune(rows []db.AutomatedRow) {
	for _, r := range rows {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:40]) + "…"
		}
		fmt.Printf("#%-5d %s  %-8s %-15s %s  %s\n", r.Num, r.FirstSeen.Format("2006-01-02 15:04"), r.Status, r.Why, r.Cwd, title)
	}
}
