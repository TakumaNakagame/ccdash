// Package tunnel is the wire between a device's collector and the hub.
//
// The device dials OUT to the hub over a WebSocket (so it never has to
// accept inbound connections), and both ends run yamux on top of it. The
// roles are deliberately inverted relative to the dial: the hub is the
// yamux client — it opens one stream per proxied HTTP request — and the
// device is the yamux server, accepting those streams and serving them with
// its ordinary collector handler. A yamux session is a net.Listener on the
// accepting side and its streams are net.Conns, so plain net/http (Hijack
// included, for the pty-raw upgrade) works unchanged over the tunnel.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Path is where the hub accepts device tunnels.
const Path = "/agent/connect"

// HeaderDeviceVersion carries the device's ccdash version on the dial so
// the hub can show it in the device list.
const HeaderDeviceVersion = "X-Ccdash-Version"

// HeaderDeviceHost carries the device's hostname on the dial.
const HeaderDeviceHost = "X-Ccdash-Hostname"

// maxMessage bounds one WebSocket message. yamux frames are at most its
// receive window (256 KiB by default) plus a header, so 1 MiB is ample.
const maxMessage = 1 << 20

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// The keepalive doubles as the idle traffic that stops the proxies on
	// the path (Caddy, ingress-nginx: 3600 s read timeout) from reaping a
	// quiet tunnel, and detects a dead peer within ~a minute.
	c.KeepAliveInterval = 20 * time.Second
	c.ConnectionWriteTimeout = 15 * time.Second
	c.LogOutput = io.Discard
	return c
}

// Dial connects to the hub at hubURL (http(s):// or ws(s)://, with or
// without the tunnel path) and returns the device side of the tunnel: a
// yamux session whose Accept yields one conn per hub request.
func Dial(ctx context.Context, hubURL, token string, hdr http.Header) (*yamux.Session, error) {
	u, err := DialURL(hubURL)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range hdr {
		h[k] = v
	}
	h.Set("Authorization", "Bearer "+token)
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	ws.SetReadLimit(maxMessage)
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Server(nc, yamuxConfig())
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return sess, nil
}

// ErrUnauthorized means the hub rejected the device token — retrying won't
// help until the operator re-joins.
var ErrUnauthorized = fmt.Errorf("hub rejected the device token (re-run `ccdash hub join`)")

// DialURL normalizes an operator-supplied hub URL into the tunnel endpoint.
func DialURL(hubURL string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"),
		strings.HasPrefix(u, "wss://"), strings.HasPrefix(u, "ws://"):
	default:
		return "", fmt.Errorf("hub url must start with https:// (or http:// for testing), got %q", hubURL)
	}
	if !strings.HasSuffix(u, Path) {
		u += Path
	}
	return u, nil
}

// Accept upgrades a device's dial on the hub side and returns the hub end
// of the tunnel: a yamux session whose Open yields a conn to the device's
// collector. The caller authenticates the request before calling Accept.
func Accept(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	// Devices are not browsers; there is no Origin to check.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessage)
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Client(nc, yamuxConfig())
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return sess, nil
}
