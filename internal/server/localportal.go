package server

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

// The local portal (`ccdash web`, docs/decisions/0005-local-portal.md)
// is the hub's web UI served on 127.0.0.1, linked to this collector by a
// yamux session — over net.Pipe when it embeds the collector, or over a
// loopback connection upgraded at /local/portal when a collector is
// already running. Either way the collector end is served here by the
// tunnel handler: the hub's allowlist and gates, minus hub_enabled.

// LocalPortalPath is the upgrade route `ccdash web` dials on a running
// collector (behind the usual token check).
const LocalPortalPath = "/local/portal"

// LocalPortalUpgrade is the Upgrade token of that route.
const LocalPortalUpgrade = "ccdash-local"

// ServeLocalPortal serves the local portal's requests arriving on conn
// (the collector end of the link) until the link closes or ctx ends.
func (s *Server) ServeLocalPortal(ctx context.Context, conn net.Conn) error {
	sess, err := yamux.Server(conn, tunnel.YamuxConfig())
	if err != nil {
		_ = conn.Close()
		return err
	}
	hs := &http.Server{Handler: s.tunnelHandler(true), ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	_ = hs.Serve(sess) // returns once the session closes
	_ = sess.Close()
	_ = hs.Close()
	return nil
}

// handleLocalPortal upgrades a token-authenticated loopback connection
// into a local portal link.
func (s *Server) handleLocalPortal(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != LocalPortalUpgrade {
		http.Error(w, "upgrade to "+LocalPortalUpgrade+" required", http.StatusUpgradeRequired)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + LocalPortalUpgrade + "\r\nConnection: Upgrade\r\n\r\n")
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return
	}
	log.Printf("local portal: linked (%s)", conn.RemoteAddr())
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	_ = s.ServeLocalPortal(ctx, &bufferedConn{Conn: conn, r: brw.Reader})
	log.Printf("local portal: unlinked")
}

// bufferedConn reads what the HTTP server already buffered before the
// hijack, then the connection itself.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// DialLocalPortal opens a local portal link to the collector at addr
// (host:port, loopback) and returns the hub end of it.
func DialLocalPortal(ctx context.Context, addr, token string) (*yamux.Session, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+LocalPortalPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", LocalPortalUpgrade)
	req.Header.Set("X-Ccdash-Token", token)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		_ = conn.Close()
		return nil, &LocalPortalError{Status: resp.StatusCode, Msg: string(msg)}
	}
	_ = conn.SetDeadline(time.Time{})
	sess, err := yamux.Client(&bufferedConn{Conn: conn, r: br}, tunnel.YamuxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sess, nil
}

// LocalPortalError is a refused upgrade; 404 means a collector that
// predates the local portal.
type LocalPortalError struct {
	Status int
	Msg    string
}

func (e *LocalPortalError) Error() string {
	if e.Status == http.StatusNotFound {
		return "the running collector predates `ccdash web` (restart it with this ccdash)"
	}
	return "local portal link refused: " + http.StatusText(e.Status) + " " + e.Msg
}
