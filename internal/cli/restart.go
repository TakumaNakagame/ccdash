package cli

// `ccdash restart`: schedule a collector restart for when no hosted claude
// is busy, see it or cancel it. (An immediate restart is the TUI's
// "Restart ccdash" or the portal's button, both of which confirm.) The
// collector does the waiting (internal/server/restartwait.go), so this
// returns at once; -r drives a remote collector.

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func restartCmd(rf *remoteFlags) *cobra.Command {
	var cancel, status bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart the collector once no hosted session is busy (resuming them all)",
		Long: `Schedules a restart of the running collector (` + "`ccdash server`" + `) for when no
session it hosts is working, holding an approval or a question, or waiting
on something in the background. It then stops every hosted claude and
resumes it with claude --resume — nothing is cut off mid-turn. ccdash (TUI,
portal, app) warns meanwhile, and the schedule gives up after --timeout.

  ccdash restart            schedule it
  ccdash restart --status   show what it waits for
  ccdash restart --cancel   drop the schedule`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			var rs model.RestartState
			switch {
			case cancel:
				rs, err = st.CancelRestart(ctx)
			case status:
				rs, err = st.RestartStatus(ctx)
			default:
				rs, err = st.ScheduleRestart(ctx, "cli", timeout)
			}
			if err != nil {
				return fmt.Errorf("collector: %w (a scheduled restart needs a running `ccdash server` of v0.9.60 or later)", err)
			}
			printRestart(rs)
			return nil
		},
	}
	c.Flags().BoolVar(&cancel, "cancel", false, "cancel a scheduled restart")
	c.Flags().BoolVar(&status, "status", false, "show the scheduled restart")
	c.Flags().DurationVar(&timeout, "timeout", 0, "give up after waiting this long (default 2h, max 24h)")
	return c
}

func printRestart(rs model.RestartState) {
	if !rs.Pending {
		switch rs.Last {
		case "cancelled":
			fmt.Println("no restart scheduled (the last one was cancelled)")
		case "expired":
			fmt.Println("no restart scheduled (the last one gave up waiting)")
		default:
			fmt.Println("no restart scheduled")
		}
		return
	}
	fmt.Printf("restart scheduled (since %s, gives up at %s)\n", rs.Since.Local().Format("15:04"), rs.Deadline.Local().Format("15:04"))
	fmt.Printf("  %d hosted session(s) will be stopped and resumed", rs.Hosted)
	if rs.Unregistered > 0 {
		fmt.Printf("; %d fresh claude(s) with no session yet will be dropped", rs.Unregistered)
	}
	fmt.Println()
	if len(rs.Waiting) == 0 {
		fmt.Println("  nothing busy — restarting in a moment")
		return
	}
	fmt.Println("  waiting for:")
	for _, b := range rs.Waiting {
		ref := shortID(b.SessionID)
		if b.Num > 0 {
			ref = fmt.Sprintf("#%d", b.Num)
		}
		fmt.Printf("    %-6s %s  %s\n", ref, b.Why, shorten(b.Title, 50))
	}
}
