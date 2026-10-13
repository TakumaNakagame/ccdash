package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCollector serves healthz (PTY-capable) and /api/restart on addr. On a
// restart with code 200 it answers, closes the port for a moment, and binds
// it again — what `ccdash server` / `ccdash web` do when they re-exec.
type fakeCollector struct {
	t        *testing.T
	addr     string
	code     int // /api/restart status
	restarts atomic.Int32
	srv      atomic.Pointer[http.Server]
}

func (f *fakeCollector) serve() {
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		f.t.Errorf("listen %s: %v", f.addr, err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"pty":true}`))
	})
	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ccdash-Token") != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.code)
		if f.code != http.StatusOK {
			return
		}
		f.restarts.Add(1)
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = f.srv.Load().Shutdown(context.Background())
			time.Sleep(300 * time.Millisecond) // the re-exec
			f.serve()
		}()
	})
	srv := &http.Server{Handler: mux}
	f.srv.Store(srv)
	go func() { _ = srv.Serve(ln) }()
}

func newFakeCollector(t *testing.T, code int) *fakeCollector {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "ccdash"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ccdash", "token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	f := &fakeCollector{t: t, addr: addr, code: code}
	f.serve()
	t.Cleanup(func() { _ = f.srv.Load().Close() })
	return f
}

func TestRestartCollectorWaitsForTheReexec(t *testing.T) {
	f := newFakeCollector(t, http.StatusOK)
	if !restartCollector(f.addr) {
		t.Fatal("restartCollector = false, want true")
	}
	if f.restarts.Load() != 1 {
		t.Fatalf("restarts = %d, want 1", f.restarts.Load())
	}
	if alive, pty := serverCapabilities(f.addr); !alive || !pty {
		t.Fatal("the collector isn't back after restartCollector returned")
	}
}

func TestRestartCollectorFallsBack(t *testing.T) {
	for _, code := range []int{http.StatusNotImplemented, http.StatusNotFound} {
		f := newFakeCollector(t, code)
		if restartCollector(f.addr) {
			t.Errorf("%d: restartCollector = true, want false (stop it instead)", code)
		}
	}
	if restartCollector("127.0.0.1:1") {
		t.Error("nothing listening: restartCollector = true")
	}
}
