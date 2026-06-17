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
	"strconv"
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
	var initialGroup string
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
			return runTUI(cmd.Context(), keepServer, initialGroup)
		},
	}
	root.Flags().BoolVarP(&keepServer, "keep-server", "k", false,
		"keep a detached collector running after the TUI exits")
	root.Flags().BoolVar(&showVersion, "version", false, "print version and exit")
	root.Flags().StringVar(&initialGroup, "group", "",
		"lock the dashboard to a single group (repo or user-named); hides the tab strip and disables group cycling")
	// --tab is the legacy spelling. Keep it accepted (hidden in --help) so
	// existing scripts don't break, but encourage the new name.
	root.Flags().StringVar(&initialGroup, "tab", "", "deprecated alias for --group")
	_ = root.Flags().MarkHidden("tab")
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
	var channelFlag string
	c := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with the latest GitHub release",
		Long: `Update ccdash from a GitHub release.

By default --channel=stable picks the latest tagged release that is not
flagged as a pre-release. --channel=dev (alias: beta / pre / prerelease)
includes pre-release tags so beta builds the maintainer cuts on a Mac
can be installed before they're promoted to stable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			channel, err := selfupdate.ParseChannel(channelFlag)
			if err != nil {
				return err
			}
			fmt.Printf("ccdash: current version %s (channel %s)\n", currentVersion, channel)
			res, err := selfupdate.Run(cmd.Context(), currentVersion, channel)
			if err != nil {
				return err
			}
			if res.NoOp {
				fmt.Printf("ccdash: %s\n", res.Reason)
				return nil
			}
			fmt.Printf("ccdash: upgraded to %s\n", res.NewVersion)
			fmt.Printf("ccdash: replaced %s\n", res.BinaryPath)
			// Print the release notes for the new tag so the operator
			// knows what just changed. Best-effort — a probe failure
			// (rate limit / offline) is silent.
			if notes, err := selfupdate.ReleaseInfo(cmd.Context(), res.NewVersion); err == nil && notes != "" {
				fmt.Println()
				fmt.Printf("--- release notes for %s ---\n", res.NewVersion)
				fmt.Println(notes)
			}
			return nil
		},
	}
	c.Flags().StringVar(&channelFlag, "channel", "stable",
		"release channel: 'stable' for tagged releases, 'dev' to include pre-releases")
	return c
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
	c.AddCommand(serverStopCmd())
	return c
}

func serverStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Gracefully shut down the running ccdash server",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
			tok, err := loadToken()
			if err != nil {
				return fmt.Errorf("load token: %w", err)
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				"http://"+addr+"/shutdown", nil)
			if err != nil {
				return err
			}
			req.Header.Set("X-Ccdash-Token", tok)
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			resp.Body.Close()
			fmt.Println("server stop requested")
			return nil
		},
	}
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
	var initialGroup string
	c := &cobra.Command{
		Use:   "tui",
		Short: "Open the dashboard TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd.Context(), keepServer, initialGroup)
		},
	}
	c.Flags().BoolVarP(&keepServer, "keep-server", "k", false,
		"keep a detached collector running after the TUI exits")
	c.Flags().StringVar(&initialGroup, "group", "",
		"lock the dashboard to a single group")
	c.Flags().StringVar(&initialGroup, "tab", "", "deprecated alias for --group")
	_ = c.Flags().MarkHidden("tab")
	return c
}

func runTUI(ctx context.Context, _ bool, lockGroup string) error {
	addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)

	// Redirect log output before the TUI takes the screen. Stray stderr
	// writes during Bubble Tea's alt-screen corrupt the visible buffer.
	restoreLog, _ := redirectLog()
	defer restoreLog()

	srvMode := tui.ServerModeExisting
	alive, hasPTY := serverCapabilities(addr)
	if !alive || !hasPTY {
		srvMode = tui.ServerModeSpawned
		if alive && !hasPTY {
			// Stale server (built before PTY support): terminate it so we can
			// bind the port with the new binary.
			killStaleServer(addr)
		}
		// No running server (or just killed the stale one) — spawn a detached
		// one. Close/reopen the DB around the spawn so the child can acquire
		// the SQLite lock.
		d0, err := openDB()
		if err != nil {
			return err
		}
		_ = d0.Close()
		if err := spawnDetachedServer(addr); err != nil {
			// Non-fatal: fall through and let the TUI show an error if
			// the server is truly unavailable.
			fmt.Fprintf(os.Stderr, "warn: could not spawn detached server: %v\n", err)
		}
	}

	d, err := openDB()
	if err != nil {
		return err
	}
	defer d.Close()

	return tui.Run(ctx, d, lockGroup, srvMode)
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
	// Save PID so we can kill a stale server on the next startup.
	saveServerPID(cmd.Process.Pid)
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

// serverCapabilities probes the healthz endpoint and returns (alive, hasPTY).
// Old servers return plain "ok"; new servers return JSON {"ok":true,"pty":true}.
// When the body can't be parsed as JSON we assume the server is alive but
// lacks PTY support so the caller can kill and respawn it.
func serverCapabilities(addr string) (alive, hasPTY bool) {
	c := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var caps struct {
		PTY bool `json:"pty"`
	}
	_ = json.Unmarshal(body, &caps)
	return true, caps.PTY
}

// pingServer returns true when an existing ccdash server already accepts
// requests on the address. We use a very short timeout because both endpoints
// are local; anything slower than ~200ms means the port isn't actually ours.
func pingServer(addr string) bool {
	alive, _ := serverCapabilities(addr)
	return alive
}

// saveServerPID writes the PID of the spawned server to state dir so we can
// kill a stale server on the next startup.
func saveServerPID(pid int) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(stateDir, "server.pid"),
		[]byte(strconv.Itoa(pid)), 0o600)
}

// killStaleServer terminates an existing server that doesn't support PTY.
// It tries a graceful shutdown first, then falls back to SIGTERM via the PID
// file. Waits up to 2s for the port to free before returning.
func killStaleServer(addr string) {
	tok, _ := loadToken()
	if tok != "" {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodPost, "http://"+addr+"/shutdown", nil)
		if err == nil {
			req.Header.Set("X-Ccdash-Token", tok)
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
	}
	// Fallback: kill via PID file.
	stateDir, err := paths.StateDir()
	if err == nil {
		data, err := os.ReadFile(filepath.Join(stateDir, "server.pid"))
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pingServer(addr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func loadToken() (string, error) {
	p, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(p, "token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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
