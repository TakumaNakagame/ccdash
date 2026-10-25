package handoffmcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// protocolVersion is what we answer when the client asks for a version we
// don't know; known ones are echoed back.
const protocolVersion = "2025-06-18"

var knownVersions = map[string]bool{"2025-03-26": true, "2025-06-18": true, "2025-11-25": true}

// Handler serves the MCP endpoint at /mcp and a plain /healthz.
type Handler struct {
	srv       *Server
	tokenHash [32]byte
	allow     []netip.Prefix // empty = any address
}

// NewHandler requires `Authorization: Bearer <token>` and, when allow is
// non-empty, a remote address inside one of the prefixes.
func NewHandler(srv *Server, token string, allow []netip.Prefix) *Handler {
	return &Handler{srv: srv, tokenHash: sha256.Sum256([]byte(token)), allow: allow}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.allowed(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" {
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if !h.authorized(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ccdash"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete: // no sessions to end
		w.WriteHeader(http.StatusNoContent)
		return
	default: // GET: no server-sent stream
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	start := time.Now()
	result, rerr := h.handle(r, req)
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = struct{}{}
	}
	writeJSON(w, resp)
	if req.Method == "tools/call" {
		log.Printf("handoff-mcp: %s from %s in %s", callSummary(req.Params, result), r.RemoteAddr, time.Since(start).Round(time.Millisecond))
	}
}

func (h *Handler) handle(r *http.Request, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocolVersion
		if knownVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ccdash-handoff", "version": h.srv.version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": h.srv.tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		out, err := h.srv.call(r.Context(), p.Name, p.Arguments)
		if err != nil {
			// Tool failures are results the model can read, not protocol errors.
			return map[string]any{"isError": true, "content": []any{text(err.Error())}}, nil
		}
		return map[string]any{"content": []any{text(out)}}, nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) authorized(header string) bool {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], h.tokenHash[:]) == 1
}

func (h *Handler) allowed(remote string) bool {
	if len(h.allow) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range h.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// callSummary is the log line of a tools/call: the tool, the session it
// names and whether it failed — never the text it carried.
func callSummary(params json.RawMessage, result any) string {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Session string `json:"session"`
			Request string `json:"request"`
			Text    string `json:"text"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	out := "tool=" + p.Name
	if p.Arguments.Session != "" {
		out += " session=" + p.Arguments.Session
	}
	if n := len(p.Arguments.Request) + len(p.Arguments.Text); n > 0 {
		out += " bytes=" + strconv.Itoa(n)
	}
	if m, ok := result.(map[string]any); ok && m["isError"] == true {
		out += " error=true"
	}
	return out
}
