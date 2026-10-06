package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	"github.com/takumanakagame/ccmanage/internal/hub"
	"github.com/takumanakagame/ccmanage/internal/hubcfg"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

func hubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hub",
		Short: "Web portal: run the hub (serve) or connect this machine to one (join)",
		Long: `Hub mode lets you use every machine's ccdash from a browser.

On a home server:   ccdash hub serve --public-url https://ccdash.example.net ...
On each machine:    ccdash hub join --url https://ccdash.example.net

A joined machine's collector dials OUT to the hub and keeps the tunnel open;
the machine never accepts inbound connections. Run the collector persistently
there ('ccdash server' as a user service, or 'ccdash -k').`,
	}
	c.AddCommand(hubServeCmd())
	c.AddCommand(hubJoinCmd())
	c.AddCommand(hubLeaveCmd())
	c.AddCommand(hubStatusCmd())
	return c
}

func envOr(flagVal, env string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(env)
}

func hubServeCmd() *cobra.Command {
	var listen, dataDir, publicURL, issuer, clientID, secretFile, readTokenFile string
	var emails []string
	var noAuth, tsAuth bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the hub (web portal + device tunnel endpoint)",
		Long: `Run the hub in the foreground.

Login is OIDC (in the home lab: a Cloudflare Access for SaaS application).
Every flag can also come from the environment, which is how the container
is configured:

  CCDASH_HUB_LISTEN, CCDASH_HUB_DATA_DIR, CCDASH_HUB_PUBLIC_URL,
  CCDASH_HUB_OIDC_ISSUER, CCDASH_HUB_OIDC_CLIENT_ID,
  CCDASH_HUB_OIDC_CLIENT_SECRET (or --oidc-client-secret-file),
  CCDASH_HUB_ALLOWED_EMAILS (comma-separated),
  CCDASH_HUB_READ_TOKENS (comma-separated; or --read-token-file /
  CCDASH_HUB_READ_TOKEN_FILE)

Read tokens are for machine clients (bots, scripts): a request with
"Authorization: Bearer <token>" may GET /api/board, /api/active,
/api/devices, /api/d/{id}/api/sessions and
/api/d/{id}/api/sessions/{sid}/transcript (tail only) — nothing else.

TLS is expected to terminate in front of the hub (Caddy / ingress).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			listen = envOr(listen, "CCDASH_HUB_LISTEN")
			if listen == "" {
				listen = "127.0.0.1:8080"
			}
			dataDir = envOr(dataDir, "CCDASH_HUB_DATA_DIR")
			if dataDir == "" {
				sd, err := paths.StateDir()
				if err != nil {
					return err
				}
				dataDir = filepath.Join(sd, "hub")
			}
			publicURL = envOr(publicURL, "CCDASH_HUB_PUBLIC_URL")
			if publicURL == "" {
				if !noAuth {
					return fmt.Errorf("--public-url is required (the origin browsers use, e.g. https://ccdash.example.net)")
				}
				publicURL = "http://" + listen
			}
			secret := os.Getenv("CCDASH_HUB_OIDC_CLIENT_SECRET")
			if secretFile != "" {
				b, err := os.ReadFile(secretFile)
				if err != nil {
					return fmt.Errorf("read client secret: %w", err)
				}
				secret = strings.TrimSpace(string(b))
			}
			if len(emails) == 0 {
				for e := range strings.SplitSeq(os.Getenv("CCDASH_HUB_ALLOWED_EMAILS"), ",") {
					if e = strings.TrimSpace(e); e != "" {
						emails = append(emails, e)
					}
				}
			}
			readTokens, err := hub.ParseReadTokens(os.Getenv("CCDASH_HUB_READ_TOKENS"))
			if err != nil {
				return fmt.Errorf("CCDASH_HUB_READ_TOKENS: %w", err)
			}
			if f := envOr(readTokenFile, "CCDASH_HUB_READ_TOKEN_FILE"); f != "" {
				ft, err := hub.ReadTokenFile(f)
				if err != nil {
					return err
				}
				readTokens = append(readTokens, ft...)
			}
			if tsAuth {
				host, _, err := net.SplitHostPort(listen)
				if err != nil || !isLoopbackHost(host) {
					return fmt.Errorf("--tailscale-auth trusts headers set by `tailscale serve` and requires a loopback --listen (got %q)", listen)
				}
			}
			if noAuth {
				host, _, err := net.SplitHostPort(listen)
				if err != nil || !isLoopbackHost(host) {
					return fmt.Errorf("--no-auth is for local development only and requires a loopback --listen (got %q)", listen)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: --no-auth — anyone who can reach", listen, "can drive every joined machine")
			}
			ctx, cancel := signalContext(cmd.Context())
			defer cancel()
			h, err := hub.New(ctx, hub.Config{
				Listen:    listen,
				DataDir:   dataDir,
				PublicURL: publicURL,
				Auth: hub.AuthConfig{
					Issuer:         envOr(issuer, "CCDASH_HUB_OIDC_ISSUER"),
					ClientID:       envOr(clientID, "CCDASH_HUB_OIDC_CLIENT_ID"),
					ClientSecret:   secret,
					AllowedEmails:  emails,
					NoAuth:         noAuth,
					TailscaleServe: tsAuth,
				},
				ReadTokens: readTokens,
			})
			if err != nil {
				return err
			}
			defer h.Close()
			return h.ListenAndServe(ctx)
		},
	}
	c.Flags().StringVar(&listen, "listen", "", "bind address (default 127.0.0.1:8080; the container uses :8080)")
	c.Flags().StringVar(&dataDir, "data-dir", "", "where hub.sqlite lives (default $XDG_STATE_HOME/ccdash/hub)")
	c.Flags().StringVar(&publicURL, "public-url", "", "browser-facing origin, e.g. https://ccdash.g3.lab-dev.net")
	c.Flags().StringVar(&issuer, "oidc-issuer", "", "OIDC issuer URL (Cloudflare Access for SaaS: https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client-id>)")
	c.Flags().StringVar(&clientID, "oidc-client-id", "", "OIDC client id")
	c.Flags().StringVar(&secretFile, "oidc-client-secret-file", "", "file holding the OIDC client secret (else $CCDASH_HUB_OIDC_CLIENT_SECRET)")
	c.Flags().StringSliceVar(&emails, "allowed-email", nil, "email allowed to log in (repeatable)")
	c.Flags().BoolVar(&tsAuth, "tailscale-auth", false, "log users in by the Tailscale-User-Login header of `tailscale serve` (loopback --listen; logins must be in --allowed-email)")
	c.Flags().BoolVar(&noAuth, "no-auth", false, "skip login (local development only; loopback --listen required)")
	c.Flags().StringVar(&readTokenFile, "read-token-file", "", "file of read-only bearer tokens for machine clients, one per line (adds to $CCDASH_HUB_READ_TOKENS)")
	return c
}

func hubJoinCmd() *cobra.Command {
	var hubURL, token string
	var skipCheck bool
	c := &cobra.Command{
		Use:   "join",
		Short: "Connect this machine's collector to a hub",
		Long: `Register this machine with a hub using a device token from the portal's
"Add device" dialog. The token is prompted for (so it stays out of your shell
history) unless --token is given or it is piped on stdin.

The collector picks the change up within ~30 s — no restart needed — but it
has to be running: 'ccdash server' (e.g. a systemd user unit / launchd agent)
or 'ccdash -k'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hubURL == "" {
				return fmt.Errorf("--url is required (the hub's address, e.g. https://ccdash.example.net)")
			}
			if _, err := tunnel.DialURL(hubURL); err != nil {
				return err
			}
			if token == "" {
				t, err := readToken(cmd)
				if err != nil {
					return err
				}
				token = t
			}
			if !strings.HasPrefix(token, "ccdh_") {
				return fmt.Errorf("that doesn't look like a device token (expected ccdh_…)")
			}
			cfg := hubcfg.Config{URL: strings.TrimRight(hubURL, "/"), Token: token}
			if !skipCheck {
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
				host, _ := os.Hostname()
				hdr := http.Header{}
				hdr.Set(tunnel.HeaderDeviceVersion, buildinfo.Version)
				hdr.Set(tunnel.HeaderDeviceHost, host)
				sess, err := tunnel.Dial(ctx, cfg.URL, cfg.Token, hdr)
				cancel()
				if err != nil {
					return fmt.Errorf("could not connect to the hub: %w (use --skip-check to save anyway)", err)
				}
				_ = sess.Close()
			}
			if err := hubcfg.Save(cfg); err != nil {
				return err
			}
			p, _ := hubcfg.Path()
			fmt.Printf("joined %s → %s\n", cfg.URL, p)
			if !collectorRunning() {
				fmt.Println("note: no collector is running here yet — start one ('ccdash server' or 'ccdash -k') so the hub can reach this machine")
			} else {
				fmt.Println("the running collector will connect within ~30 s")
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubURL, "url", "", "hub origin, e.g. https://ccdash.g3.lab-dev.net")
	c.Flags().StringVar(&token, "token", "", "device token (prompted for when omitted)")
	c.Flags().BoolVar(&skipCheck, "skip-check", false, "save without test-connecting to the hub")
	return c
}

func readToken(cmd *cobra.Command) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(cmd.ErrOrStderr(), "device token: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no token on stdin")
	}
	return strings.TrimSpace(line), nil
}

func collectorRunning() bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/healthz", paths.DefaultHost, paths.DefaultPort))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func hubLeaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "leave",
		Short: "Disconnect this machine from its hub (deletes the local device token)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := hubcfg.Remove(); err != nil {
				return err
			}
			fmt.Println("left the hub; a running collector drops the tunnel within ~10 s")
			fmt.Println("(also delete the device in the portal so its token can't be reused)")
			return nil
		},
	}
}

func hubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which hub this machine is joined to",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := hubcfg.Load()
			if errors.Is(err, hubcfg.ErrNotFound) {
				fmt.Println("not joined to a hub (run: ccdash hub join --url https://…)")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Printf("hub:       %s\n", cfg.URL)
			if d, err := openDB(); err == nil {
				s, _ := settings.Load(cmd.Context(), d)
				d.Close()
				fmt.Printf("enabled:   %v (settings → Hub connection)\n", s.HubEnabled)
			}
			fmt.Printf("collector: %v\n", map[bool]string{true: "running", false: "not running"}[collectorRunning()])
			return nil
		},
	}
}
