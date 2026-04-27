package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/hookcfg"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/selfupdate"
	"github.com/takumanakagame/ccmanage/internal/server"
	"github.com/takumanakagame/ccmanage/internal/tui"
	"github.com/takumanakagame/ccmanage/internal/wrapper"
)

func Root(version string) *cobra.Command {
	var keepServer bool
	var showVersion bool
	root := &cobra.Command{
		Use:     "ccdash",
		Short:   "Local dashboard for Claude Code sessions",
		Version: version,
		Long: `ccdash is a local TUI dashboard for monitoring multiple Claude Code sessions.

Quick start:
  1) ccdash install-hooks  (one-time: enables real-time event capture)
  2) ccdash               (opens the dashboard; embeds the server while open)

The default 'ccdash' command opens the TUI. If no collector is already
listening on 127.0.0.1:9123, ccdash spawns one in the same process and tears
it down when you quit the TUI — so a single command is enough.

Pass --keep-server (-k) to spawn a detached server that keeps running after
the TUI exits, so events are captured even while you're not watching. Use
'ccdash server' for a foreground collector (e.g. as a systemd user unit).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Println(version)
				return nil
			}
			return runTUI(cmd.Context(), keepServer)
		},
	}
	root.Flags().BoolVarP(&keepServer, "keep-server", "k", false,
		"keep a detached collector running after the TUI exits")
	root.Flags().BoolVar(&showVersion, "version", false, "print version and exit")
	root.AddCommand(serverCmd())
	root.AddCommand(claudeCmd())
	root.AddCommand(sessionsCmd())
	root.AddCommand(eventsCmd())
	root.AddCommand(approvalsCmd())
	root.AddCommand(installHooksCmd())
	root.AddCommand(uninstallHooksCmd())
	root.AddCommand(tuiCmd())
	root.AddCommand(updateCmd(version))
	return root
}

func updateCmd(currentVersion string) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with the latest GitHub release",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("ccdash: current version %s\n", currentVersion)
			res, err := selfupdate.Run(cmd.Context(), currentVersion)
			if err != nil {
				return err
			}
			if res.NoOp {
				fmt.Printf("ccdash: %s\n", res.Reason)
				return nil
			}
			fmt.Printf("ccdash: upgraded to %s\n", res.NewVersion)
			fmt.Printf("ccdash: replaced %s\n", res.BinaryPath)
			return nil
		},
	}
}

func serverCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the HTTP hook collector",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB()
			if err != nil {
				return err
			}
			defer d.Close()
			ctx, cancel := signalContext(cmd.Context())
			defer cancel()
			s := server.New(d, addr)
			return s.ListenAndServe(ctx)
		},
	}
	c.Flags().StringVar(&addr, "addr", fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort), "bind address")
	return c
}

func claudeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "claude [-- args...]",
		Short: "Run claude with extra metadata (tmux pane, wrapper pid)",
		Long: `Optional wrapper around 'claude'. ccdash observes plain 'claude' invocations
just fine via the installed hooks; use this command only when you want
ccdash to also record tmux pane / session and the wrapper PID.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return wrapper.Exec(cmd.Context(), args)
		},
	}
	return c
}

func sessionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB()
			if err != nil {
				return err
			}
			defer d.Close()
			ss, err := d.ListSessions(cmd.Context(), false)
			if err != nil {
				return err
			}
			if len(ss) == 0 {
				fmt.Println("(no sessions)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "STATUS\tSESSION\tCWD\tBRANCH\tLAST_SEEN\tPENDING")
			for _, s := range ss {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
					s.Status,
					shortID(s.SessionID),
					shorten(s.Cwd, 40),
					s.Branch,
					humanTime(s.LastSeen),
					s.PendingCount,
				)
			}
			return tw.Flush()
		},
	}
}

func eventsCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "events <session_id>",
		Short: "List events for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB()
			if err != nil {
				return err
			}
			defer d.Close()
			es, err := d.ListEvents(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tTYPE\tTOOL\tSUMMARY")
			for _, e := range es {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					e.Timestamp.Local().Format("15:04:05"),
					e.EventType,
					e.Tool,
					shorten(e.Summary, 100),
				)
			}
			return tw.Flush()
		},
	}
	c.Flags().IntVar(&limit, "limit", 200, "max events to show")
	return c
}

func approvalsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approvals",
		Short: "List pending permission requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := openDB()
			if err != nil {
				return err
			}
			defer d.Close()
			as, err := d.ListPendingApprovals(cmd.Context())
			if err != nil {
				return err
			}
			if len(as) == 0 {
				fmt.Println("(no pending approvals)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSESSION\tTOOL\tINPUT\tWAITING")
			for _, a := range as {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
					a.ID,
					shortID(a.SessionID),
					a.Tool,
					shorten(string(a.ToolInput), 100),
					humanDuration(time.Since(a.Timestamp)),
				)
			}
			return tw.Flush()
		},
	}
}

func installHooksCmd() *cobra.Command {
	var dryRun bool
	var settingsPath string
	c := &cobra.Command{
		Use:   "install-hooks",
		Short: "Add ccdash HTTP hooks to ~/.claude/settings.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := hookcfg.DefaultInstall()
			if err != nil {
				return err
			}
			if settingsPath != "" {
				in.Path = settingsPath
			}
			in.DryRun = dryRun
			changed, err := in.Apply()
			if err != nil {
				return err
			}
			if dryRun {
				return nil
			}
			if changed {
				fmt.Printf("hooks installed → %s\n", in.Path)
			} else {
				fmt.Println("no changes")
			}
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  1) start the collector:  ccdash server")
			fmt.Println("  2) run claude as usual — sessions will appear in `ccdash`")
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print merged settings.json to stdout instead of writing")
	c.Flags().StringVar(&settingsPath, "settings", "", "override settings.json path (default: ~/.claude/settings.json)")
	return c
}

func uninstallHooksCmd() *cobra.Command {
	var settingsPath string
	c := &cobra.Command{
		Use:   "uninstall-hooks",
		Short: "Remove ccdash HTTP hooks from ~/.claude/settings.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := hookcfg.DefaultInstall()
			if err != nil {
				return err
			}
			if settingsPath != "" {
				in.Path = settingsPath
			}
			if err := in.Remove(); err != nil {
				return err
			}
			fmt.Printf("hooks removed from %s\n", in.Path)
			return nil
		},
	}
	c.Flags().StringVar(&settingsPath, "settings", "", "override settings.json path")
	return c
}

func tuiCmd() *cobra.Command {
	var keepServer bool
	c := &cobra.Command{
		Use:   "tui",
		Short: "Open the dashboard TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd.Context(), keepServer)
		},
	}
	c.Flags().BoolVarP(&keepServer, "keep-server", "k", false,
		"keep a detached collector running after the TUI exits")
	return c
}

func runTUI(ctx context.Context, keepServer bool) error {
	d, err := openDB()
	if err != nil {
		return err
	}
	defer d.Close()

	addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)

	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	var serverDone chan struct{}
	embedded := false

	switch {
	case pingServer(addr):
		// Someone (probably `ccdash server`) is already running. Just open the
		// TUI and let them keep collecting.
	case keepServer:
		// Detach a child process running `ccdash server` so it survives the
		// TUI's lifetime. Close the DB first so the child can take the lock.
		_ = d.Close()
		if err := spawnDetachedServer(addr); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not spawn detached server: %v\n", err)
		}
		// Reopen the DB now that the child has a chance to be writing too.
		d, err = openDB()
		if err != nil {
			return err
		}
	default:
		// Embedded mode: run the server in this process; tear it down on quit.
		// Redirect log output to a file first — server goroutines call
		// log.Printf, and stderr writes during Bubble Tea's alt-screen render
		// corrupt the visible buffer (the symptom the user saw was the tabs
		// row vanishing seconds after startup).
		restoreLog, _ := redirectLog()
		defer restoreLog()
		s := server.New(d, addr)
		serverDone = make(chan struct{})
		embedded = true
		go func() {
			defer close(serverDone)
			_ = s.ListenAndServe(serverCtx)
		}()
	}

	tuiErr := tui.Run(ctx, d)

	cancelServer()
	if embedded && serverDone != nil {
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
		}
	}
	return tuiErr
}

// spawnDetachedServer launches `ccdash server` as a session-leader child that
// outlives the current process. We poll healthz briefly so the TUI doesn't
// race against the child binding the port.
func spawnDetachedServer(addr string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(os.TempDir(), "ccdash-server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "server")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detach: don't Wait. Release the child's process handle so the OS can
	// reap it on its own once it eventually exits.
	_ = cmd.Process.Release()

	// Wait up to ~3s for the new server to come up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pingServer(addr) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server didn't become ready within 3s (logs: %s)", logPath)
}

// redirectLog points the standard logger at $XDG_STATE_HOME/ccdash/ccdash.log
// (creating it if necessary) and returns a function that restores the
// original sink. We call this whenever we start a server in-process under a
// TUI so the alt-screen renderer doesn't get random log lines mixed into its
// output.
func redirectLog() (func(), error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		// Fall back to discarding logs entirely — better than corrupting TUI.
		prev := log.Default().Writer()
		log.SetOutput(io.Discard)
		return func() { log.SetOutput(prev) }, err
	}
	logPath := filepath.Join(stateDir, "ccdash.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		prev := log.Default().Writer()
		log.SetOutput(io.Discard)
		return func() { log.SetOutput(prev) }, err
	}
	prev := log.Default().Writer()
	log.SetOutput(f)
	return func() {
		log.SetOutput(prev)
		_ = f.Close()
	}, nil
}

// pingServer returns true when an existing ccdash server already accepts
// requests on the address. We use a very short timeout because both endpoints
// are local; anything slower than ~200ms means the port isn't actually ours.
func pingServer(addr string) bool {
	c := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func openDB() (*db.DB, error) {
	p, err := paths.DBPath()
	if err != nil {
		return nil, err
	}
	return db.Open(p)
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanDuration(time.Since(t)) + " ago"
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// helpers below kept here so we don't pull more dependencies just for printing.
var _ = json.Marshal
var _ = filepath.Base
