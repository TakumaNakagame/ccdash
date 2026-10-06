package hub

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Read tokens let a machine client (a bot, a script) read the hub's
// aggregated session data with `Authorization: Bearer <token>` instead of a
// browser login. They are deliberately narrow: GET only, on a fixed
// allowlist of read routes (readTokenRoutes). Every other /api/ route stays
// cookie-only and answers a bearer request with 403. See
// docs/decisions/0004-hub-read-tokens.md.

// minReadTokenLen keeps obviously weak tokens out (`openssl rand -hex 32`
// gives 64 characters).
const minReadTokenLen = 32

// maxReadTranscriptBytes caps a bearer transcript tail; the portal itself
// may ask for more, a bot has no reason to.
const maxReadTranscriptBytes = 1 << 20

var errShortReadToken = fmt.Errorf("read tokens must be at least %d characters (try `openssl rand -hex 32`)", minReadTokenLen)

// ParseReadTokens splits a comma- or newline-separated list (the env var /
// the token file), ignoring blanks and #-comment lines.
func ParseReadTokens(s string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(s, ",", "\n")))
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if len(t) < minReadTokenLen {
			return nil, errShortReadToken
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// ReadTokenFile reads tokens from a file (one per line, or comma-separated).
func ReadTokenFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	toks, err := ParseReadTokens(string(b))
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", path, err)
	}
	return toks, nil
}

// hashReadTokens keeps only SHA-256 digests in memory; comparing digests
// of a fixed length with subtle.ConstantTimeCompare leaks neither the
// token's content nor its length.
func hashReadTokens(toks []string) ([][32]byte, error) {
	out := make([][32]byte, 0, len(toks))
	for _, t := range toks {
		if len(t) < minReadTokenLen {
			return nil, errShortReadToken
		}
		out = append(out, sha256.Sum256([]byte(t)))
	}
	return out, nil
}

// readTokenOK reports whether tok is one of the configured read tokens. It
// checks every entry so the time taken doesn't depend on which one matched.
func (h *Hub) readTokenOK(tok string) bool {
	if tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	match := 0
	for _, want := range h.readTokens {
		match |= subtle.ConstantTimeCompare(sum[:], want[:])
	}
	return match == 1
}

// readTokenRoutes is the whole surface a read token reaches (GET only).
var readTokenRoutes = []*regexp.Regexp{
	regexp.MustCompile(`^/api/board$`),
	regexp.MustCompile(`^/api/active$`),
	regexp.MustCompile(`^/api/devices$`),
	regexp.MustCompile(`^/api/d/[^/]+/api/sessions$`),
	regexp.MustCompile(`^/api/d/[^/]+/api/sessions/[^/]+/transcript$`),
}

var readTranscriptRoute = regexp.MustCompile(`^/api/d/[^/]+/api/sessions/[^/]+/transcript$`)

// readTokenAllowed reports whether a bearer request may proceed, and may
// tighten its query (transcript size) in place.
func readTokenAllowed(r *http.Request) (bool, string) {
	if r.Method != http.MethodGet {
		return false, "read tokens are GET-only"
	}
	p := r.URL.Path
	ok := false
	for _, re := range readTokenRoutes {
		if re.MatchString(p) {
			ok = true
			break
		}
	}
	if !ok {
		return false, "not available to read tokens"
	}
	if readTranscriptRoute.MatchString(p) {
		q := r.URL.Query()
		switch q.Get("mode") {
		case "", "tail", "stat":
		default:
			return false, "read tokens may only tail a transcript (mode=tail|stat)"
		}
		n, err := strconv.ParseInt(q.Get("bytes"), 10, 64)
		if q.Get("bytes") != "" && (err != nil || n <= 0 || n > maxReadTranscriptBytes) {
			q.Set("bytes", strconv.Itoa(maxReadTranscriptBytes))
			r.URL.RawQuery = q.Encode()
		}
	}
	return true, ""
}

// bearerToken returns the Authorization bearer credential, if any.
func bearerToken(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", false
	}
	scheme, tok, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// apiAuth gates /api/: a bearer request (when read tokens are configured)
// is checked against the read tokens and the read allowlist; everything
// else goes through the browser login exactly as before.
func (h *Hub) apiAuth(api http.Handler) http.Handler {
	cookie := h.requireUser(api, false)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, isBearer := bearerToken(r)
		if !isBearer || len(h.readTokens) == 0 {
			cookie.ServeHTTP(w, r)
			return
		}
		if !h.readTokenOK(tok) {
			log.Printf("hub: rejected read token from %s (%s %s)", clientIP(r), r.Method, r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="ccdash-hub"`)
			http.Error(w, "invalid read token", http.StatusUnauthorized)
			return
		}
		if ok, why := readTokenAllowed(r); !ok {
			http.Error(w, why, http.StatusForbidden)
			return
		}
		// A read token is not a browser session: drop any cookie so no
		// handler can mistake the request for a logged-in user.
		r.Header.Del("Cookie")
		api.ServeHTTP(w, r)
	})
}
