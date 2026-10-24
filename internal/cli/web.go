package cli

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/buildinfo"
	"github.com/TakumaNakagame/ccdash/internal/hub"
	"github.com/TakumaNakagame/ccdash/internal/paths"
	"github.com/TakumaNakagame/ccdash/internal/server"
	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// defaultWebPort is the local portal's port (the collector has 9123).
const defaultWebPort = 9124

// webReexecEnv marks a `ccdash web` that re-exec'd itself (portal →
// restart), so it doesn't open another browser tab.
const webReexecEnv = "CCDASH_WEB_REEXEC"

func webCmd() *cobra.Command {
	var port int
	var noOpen, foreground bool
	var collector string
	c := &cobra.Command{
		Use:   "web",
		Short: "Start the web portal for this machine on 127.0.0.1 in the background (no hub needed)",
		Long: `Serve the hub's web portal for this machine only, on 127.0.0.1.

'ccdash web' starts the portal in the background and returns: it runs
detached from the terminal (closing it doesn't stop the portal), logs to
$XDG_STATE_HOME/ccdash/web.log, and opens a login link in your browser.
If a portal is already running it only opens the login link.

  ccdash web stop            stop it (and the collector + claude sessions it runs)
  ccdash web stop --resume   ... remembering those sessions for the next start
  ccdash web restart         restart it; hosted claude sessions are resumed
  ccdash web status          is it running, where, which collector
  ccdash web open            log in again (another browser, expired cookie)

--foreground keeps it in the terminal instead (Ctrl-C stops it); use it
under launchd / systemd. A portal started by launchd (parent PID 1) or a
systemd unit runs in the foreground by itself.

The portal talks to this machine's collector in-process: if one is already
running (ccdash server, ccdash -k, a TUI's) it links to that; otherwise it
runs the collector itself, like 'ccdash server'. A collector run here stops
with the portal, and POST /shutdown to it stops the whole portal; the TUI's
"Restart ccdash" restarts it (portal included) with its sessions resumed. Nothing listens beyond loopback and nothing
connects out, apart from Web Push notifications once you turn them on in
the portal (the browser's push service).

See docs/hub.md ("Local portal") and docs/decisions/0005-local-portal.md.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _, err := net.SplitHostPort(collector)
			if err != nil || !isLoopbackHost(host) {
				return fmt.Errorf("--collector-addr must be a loopback host:port, got %q", collector)
			}
			if !foreground && !underSupervisor() {
				return startWebDaemon(port, collector, !noOpen)
			}
			stopTracing := startTracing(cmd.Context(), "ccdash-collector")
			defer stopTracing()
			err = runWeb(cmd.Context(), port, collector, !noOpen && os.Getenv(webReexecEnv) == "")
			if errors.Is(err, server.ErrRestart) {
				_ = os.Setenv(webReexecEnv, "1")
				stopTracing()
				return reexecSelf()
			}
			if errors.Is(err, errCollectorStopped) {
				log.Printf("web: the collector was shut down; ccdash web exits with it")
				return nil
			}
			return err
		},
	}
	c.Flags().IntVar(&port, "port", defaultWebPort, "port on 127.0.0.1")
	c.Flags().BoolVar(&noOpen, "no-open", false, "don't open a browser (and don't print the login link; use 'ccdash web open')")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run in this terminal instead of the background (for launchd / systemd)")
	// Testing aid: link to / run a collector on another loopback port, so a
	// throwaway HOME can be tried next to the real collector on 9123.
	c.Flags().StringVar(&collector, "collector-addr", fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort), "collector address (loopback)")
	_ = c.Flags().MarkHidden("collector-addr")
	c.AddCommand(webOpenCmd(), webStopCmd(), webRestartCmd(), webStatusCmd())
	return c
}

func webLoginFile() (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "web-login"), nil
}

// webURLFile holds the running local portal's origin, so `ccdash mcp` on a
// machine that never joined a hub can still hand out session links.
func webURLFile() (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "web-url"), nil
}

// localPortalURL is the origin of a running `ccdash web`, or "".
func localPortalURL() string {
	f, err := webURLFile()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func runWeb(parent context.Context, port int, collector string, openIt bool) error {
	sd, err := paths.StateDir()
	if err != nil {
		return err
	}
	loginFile, err := webLoginFile()
	if err != nil {
		return err
	}
	listen := fmt.Sprintf("%s:%d", paths.DefaultHost, port)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w (another 'ccdash web' running? 'ccdash web open' logs into it)", listen, err)
	}
	defer ln.Close()

	ctx, cancel := signalContext(parent)
	defer cancel()
	h, err := hub.New(ctx, hub.Config{
		Listen:    listen,
		DataDir:   filepath.Join(sd, "web"),
		PublicURL: "http://" + listen,
		Local:     &hub.LocalConfig{LoginFile: loginFile, User: osUser()},
	})
	if err != nil {
		return err
	}
	defer h.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Serve(ctx, ln) }()

	if f, err := webURLFile(); err == nil && os.WriteFile(f, []byte("http://"+listen+"\n"), 0o600) == nil {
		defer os.Remove(f)
	}
	st := newWebState(port, "http://"+listen, collector)
	st.save()
	defer st.remove()

	log.Printf("ccdash web %s on http://%s (loopback only)", buildinfo.Version, listen)
	if openIt {
		fmt.Printf("ccdash web: http://%s — opening a login link in your browser\n", listen)
		if err := openLogin(h.LoginURL()); err != nil {
			fmt.Println("ccdash web: could not open a browser:", err, "— run 'ccdash web open --print'")
		}
	} else {
		fmt.Printf("ccdash web: http://%s — run 'ccdash web open' to log in\n", listen)
	}

	linkErr := make(chan error, 1)
	go func() { linkErr <- linkCollector(ctx, h, collector, st.setMode) }()
	select {
	case err := <-serveErr:
		cancel()
		<-linkErr
		return err
	case err := <-linkErr:
		cancel()
		<-serveErr
		return err
	}
}

// linkCollector keeps the portal's one device linked to this machine's
// collector: a running one through its /local/portal upgrade, else one
// run here in-process over net.Pipe. Returns at ctx end, with
// server.ErrRestart when the embedded collector was asked to restart
// (POST /api/restart: the caller re-execs), or with errCollectorStopped
// when it was shut down (POST /shutdown) — never by starting another
// collector in this process next to what the old one hosted.
func linkCollector(ctx context.Context, h *hub.Hub, addr string, mode func(string)) error {
	host, _ := os.Hostname()
	lastErr := ""
	note := func(err error) {
		if err.Error() != lastErr {
			log.Printf("web: %v", err)
			lastErr = err.Error()
		}
	}
	for ctx.Err() == nil {
		if alive, hasPTY := serverCapabilities(addr); alive {
			mode(webLinked)
			err := linkRunning(ctx, h, addr, host, hasPTY)
			if err == nil {
				lastErr = ""
				// Unlinked. A collector that restarts itself (POST
				// /api/restart) leaves the port free for a moment while it
				// re-execs; give it relinkGrace to come back before running
				// one here, or the portal takes the port — and the saved
				// sessions — from it.
				waitCollectorBack(ctx, addr, relinkGrace)
				continue
			}
			note(err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		mode(webEmbedded)
		err := runEmbeddedCollector(ctx, h, addr, host)
		if errors.Is(err, server.ErrRestart) {
			return err
		}
		if err == nil && ctx.Err() == nil {
			return errCollectorStopped
		}
		if err != nil {
			note(err)
			sleepCtx(ctx, 2*time.Second)
		}
	}
	return nil
}

// relinkGrace is how long a portal whose link dropped waits for the
// collector to answer again before running its own (linkCollector).
const relinkGrace = 10 * time.Second

// waitCollectorBack polls addr until a collector answers there or d passes.
func waitCollectorBack(ctx context.Context, addr string, d time.Duration) {
	deadline := time.Now().Add(d)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if pingServer(addr) {
			return
		}
		sleepCtx(ctx, 200*time.Millisecond)
	}
}

// linkRunning links to the collector already serving addr until the link
// drops.
func linkRunning(ctx context.Context, h *hub.Hub, addr, host string, hasPTY bool) error {
	if !hasPTY {
		return fmt.Errorf("the collector on %s is too old for 'ccdash web' — restart it with this ccdash", addr)
	}
	tok, err := loadToken()
	if err != nil {
		return fmt.Errorf("read the collector token: %w", err)
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sess, err := server.DialLocalPortal(dctx, addr, tok)
	cancel()
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	log.Printf("web: linked to the running collector on %s", addr)
	err = h.ServeLocal(sess, host, "")
	log.Printf("web: collector link closed")
	return err
}

// runEmbeddedCollector runs the collector in this process (as 'ccdash
// server' would) and links the portal to it over net.Pipe, until the
// collector stops.
func runEmbeddedCollector(ctx context.Context, h *hub.Hub, addr, host string) error {
	d, err := openDB()
	if err != nil {
		return err
	}
	defer d.Close()
	s := server.New(d, addr)
	s.CanRestart = true // the portal's restart re-execs 'ccdash web'
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hubEnd, devEnd := net.Pipe()
	go func() { _ = s.ServeLocalPortal(sctx, devEnd) }()
	sess, err := yamux.Client(hubEnd, tunnel.YamuxConfig())
	if err != nil {
		_ = hubEnd.Close()
		return err
	}
	defer sess.Close()
	go func() { _ = h.ServeLocal(sess, host, buildinfo.Version) }()

	log.Printf("web: running the collector in this process")
	return s.ListenAndServe(sctx) // closes the claudes it hosted
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "local"
}

func webOpenCmd() *cobra.Command {
	var printOnly bool
	c := &cobra.Command{
		Use:   "open",
		Short: "Log in to the running 'ccdash web' (opens a fresh one-time link)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return openWebLogin(printOnly)
		},
	}
	c.Flags().BoolVar(&printOnly, "print", false, "print the login link instead of opening a browser")
	return c
}

// openWebLogin opens (or prints) the running portal's current login link.
func openWebLogin(printOnly bool) error {
	p, err := webLoginFile()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("'ccdash web' has never run here — start it first")
	}
	if err != nil {
		return err
	}
	link := strings.TrimSpace(string(b))
	base, _, _ := strings.Cut(link, "/auth/")
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		return fmt.Errorf("'ccdash web' is not running (%s): start it first", base)
	}
	resp.Body.Close()
	if printOnly {
		fmt.Println(link)
		return nil
	}
	fmt.Println("ccdash web: opening a login link for", base)
	return openLogin(link)
}

// openLogin opens the login link without putting it on a command line,
// where other users could read it (ps): like Jupyter, it writes a 0600
// redirect page and opens that file. WSL hands the URL to Windows, which
// can't open a Linux path reliably, so it passes the link itself there.
func openLogin(link string) error {
	if isWSL() {
		return openBrowser(link)
	}
	sd, err := paths.StateDir()
	if err != nil {
		return err
	}
	page := filepath.Join(sd, "web-open.html")
	_ = os.Remove(page) // recreate with 0600 whatever the old mode was
	body := fmt.Sprintf(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%[1]s"><title>ccdash web</title><a href="%[1]s">ccdash web</a>`, html.EscapeString(link))
	if err := os.WriteFile(page, []byte(body), 0o600); err != nil {
		return err
	}
	return openBrowser("file://" + page)
}

// openBrowser opens u in the desktop's default browser: open (macOS),
// wslview / Windows' URL handler (WSL), xdg-open (Linux).
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin":
		cmd = exec.Command("open", u)
	case isWSL():
		if _, err := exec.LookPath("wslview"); err == nil {
			cmd = exec.Command("wslview", u)
		} else {
			cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u)
		}
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func isWSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}
