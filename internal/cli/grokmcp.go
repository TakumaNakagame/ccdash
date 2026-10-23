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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/takumanakagame/ccmanage/internal/grokmcp"
	"github.com/takumanakagame/ccmanage/internal/hubcfg"
)

// grokMCPCmd serves the Grok tools (internal/grokmcp) over HTTP, for an MCP
// gateway on the LAN to reach.
func grokMCPCmd(rf *remoteFlags, version string) *cobra.Command {
	var listen, tokenFile, cwd, project, hubURL string
	var allow []string
	c := &cobra.Command{
		Use:   "grok-mcp",
		Short: "Serve the Grok tools (start a session in one directory, list, read and message the ones it started) as an HTTP MCP server",
		Long: `Runs a Streamable HTTP MCP server at http://<listen>/mcp for an outside assistant (Grok,
through an MCP gateway) that may hand work to Claude without driving your own sessions:

  start_session   a new claude in --cwd (auto mode), marked as started by Grok
  list_sessions   every session, but only ref / title / status / its one-line note
  read_session    the conversation of a session Grok started
  send_message    more text for a session Grok started (resumes it when stopped)

It never answers approvals or dialogs. Requests need "Authorization: Bearer <token>"
(the first line of --token-file); --allow narrows the client addresses.`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
			srv := grokmcp.New(st, grokmcp.Config{Cwd: filepath.Clean(cwd), Project: project, HubURL: hubURL}, version)
			hs := &http.Server{Addr: listen, Handler: grokmcp.NewHandler(srv, token, prefixes), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-cmd.Context().Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = hs.Shutdown(ctx)
			}()
			log.Printf("grok-mcp: listening on %s (cwd %s)", listen, cwd)
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:9131", "address to listen on")
	c.Flags().StringVar(&tokenFile, "token-file", "", "file whose first line is the bearer token clients must send (required)")
	c.Flags().StringVar(&cwd, "cwd", "", "the only directory start_session starts claude in (required, absolute)")
	c.Flags().StringVar(&project, "project", "Grok", "ccdash project the started sessions join (\"\" = none)")
	c.Flags().StringSliceVar(&allow, "allow", nil, "client addresses or prefixes allowed (repeatable; default: any)")
	c.Flags().StringVar(&hubURL, "hub-url", "", "hub origin for session links (default: the hub this machine joined)")
	_ = c.MarkFlagRequired("token-file")
	_ = c.MarkFlagRequired("cwd")
	return c
}
