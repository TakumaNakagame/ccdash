package server

import (
	"net/http"
	"sync"
	"time"
)

// tokenBucket is a tiny token-bucket limiter. We don't bother with
// per-client buckets because all traffic is loopback under one UID — the
// limiter is here to bound runaway hook loops (a claude bug, a ccdash bug,
// or an operator script that floods POSTs) from saturating the SQLite
// writer. Sustained 50 QPS with a 100-token burst leaves plenty of
// headroom for normal claude use, which fires single-digit hooks/sec.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	refill   float64 // tokens added per second
	last     time.Time
}

func newTokenBucket(qps, burst int) *tokenBucket {
	return &tokenBucket{
		tokens:   float64(burst),
		capacity: float64(burst),
		refill:   float64(qps),
		last:     time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.refill
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimited wraps a handler so requests over the configured QPS receive a
// 429 with a Retry-After hint instead of touching the DB.
func (s *Server) rateLimited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.limiter == nil || s.limiter.allow() {
			h(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "ccdash: rate limit exceeded", http.StatusTooManyRequests)
	}
}
