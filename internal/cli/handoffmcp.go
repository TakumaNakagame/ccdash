package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/handoffmcp"
	"github.com/TakumaNakagame/ccdash/internal/hubcfg"
)

var originRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// grokSpeechHint is what the grok-mcp alias adds to start_session: Grok
// usually relays what the operator said aloud.
const grokSpeechHint = "The operator usually speaks to you, so rewrite what they said as clear written text first: " +
	"fix speech-recognition mistakes (names of tools, repositories and services), and if a word is still unclear, " +
	"ask the operator before starting. "

// handoffMCPCmd serves the hand-off tools (internal/handoffmcp) over HTTP, for
// an MCP gateway or a bot on the LAN to reach.
func handoffMCPCmd(rf *remoteFlags, version string) *cobra.Command {
	return newHandoffMCPCmd(rf, version, "handoff-mcp", handoffmcp.DefaultOrigin, "", "")
}

// grokMCPCmd is the old name, kept for the Grok worker: the origin "grok",
// the project "Grok" and the speech-recognition advice as before.
func grokMCPCmd(rf *remoteFlags, version string) *cobra.Command {
	c := newHandoffMCPCmd(rf, version, "grok-mcp", "grok", "Grok", grokSpeechHint)
	c.Deprecated = "use handoff-mcp --origin grok --project Grok (the same server)"
	c.Hidden = true
	return c
}

func newHandoffMCPCmd(rf *remoteFlags, version, use, defOrigin, defProject, hint string) *cobra.Command {
	var listen, tokenFile, cwd, project, hubURL, origin string
	var allow []string
	c := &cobra.Command{
		Use:   use,
		Short: "Serve the hand-off tools (start a session in one directory, list, read and message the ones it started) as an HTTP MCP server",
		Long: `Runs a Streamable HTTP MCP server at http://<listen>/mcp for an outside caller (an assistant
through an MCP gateway, or a bot) that may hand work to Claude without driving your own sessions:

  start_session   a new claude in --cwd (auto mode), marked with --origin
  list_sessions   every session, but only ref / title / status / its one-line note
  read_session    the conversation of a session this server started
  send_message    more text for a session this server started (resumes it when stopped)

"Started by this server" means carrying its --origin, so instances with different origins
(one per caller) never see each other's conversations. It never answers approvals or dialogs.
Requests need "Authorization: Bearer <token>" (the first line of --token-file); --allow
narrows the client addresses.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !originRe.MatchString(origin) {
				return fmt.Errorf("--origin must be 1-32 of a-z, 0-9 and -")
			}
			if !filepath.IsAbs(cwd) {
				return fmt.Errorf("--cwd must be an absolute path")
			}
			if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
				return fmt.Errorf("--cwd %s is not a directory", cwd)
			}
			raw, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("--token-file: %w", err)
			}
			token := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
			if len(token) < 32 {
				return errors.New("--token-file: the token must be at least 32 characters")
			}
			var prefixes []netip.Prefix
			for _, a := range allow {
				p, err := netip.ParsePrefix(a)
				if err != nil {
					ip, err2 := netip.ParseAddr(a)
					if err2 != nil {
						return fmt.Errorf("--allow %q: %w", a, err)
					}
					p = netip.PrefixFrom(ip, ip.BitLen())
				}
				prefixes = append(prefixes, p)
			}
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			if hubURL == "" {
				if hc, err := hubcfg.Load(); err == nil {
					hubURL = hc.URL
				}
			}
			srv := handoffmcp.New(st, handoffmcp.Config{Cwd: filepath.Clean(cwd), Project: project, HubURL: hubURL, Origin: origin, StartHint: hint}, version)
			hs := &http.Server{Addr: listen, Handler: handoffmcp.NewHandler(srv, token, prefixes), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-cmd.Context().Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = hs.Shutdown(ctx)
			}()
			log.Printf("%s: listening on %s (cwd %s, origin %s)", use, listen, cwd, origin)
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:9131", "address to listen on")
	c.Flags().StringVar(&tokenFile, "token-file", "", "file whose first line is the bearer token clients must send (required)")
	c.Flags().StringVar(&cwd, "cwd", "", "the only directory start_session starts claude in (required, absolute)")
	c.Flags().StringVar(&project, "project", defProject, "ccdash project the started sessions join (\"\" = none)")
	c.Flags().StringVar(&origin, "origin", defOrigin, "marks the sessions it starts; it reads and messages only those (1-32 of a-z, 0-9, -)")
	c.Flags().StringSliceVar(&allow, "allow", nil, "client addresses or prefixes allowed (repeatable; default: any)")
	c.Flags().StringVar(&hubURL, "hub-url", "", "hub origin for session links (default: the hub this machine joined)")
	_ = c.MarkFlagRequired("token-file")
	_ = c.MarkFlagRequired("cwd")
	return c
}
