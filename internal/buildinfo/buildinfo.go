// Package buildinfo carries the version label and a content-derived build
// fingerprint. The fingerprint hashes the running binary so that two
// independent builds of the same source tree produce the same identifier
// and any code change shifts it — useful in the TUI badge so we can tell
// at a glance whether a dev binary has been rebuilt.
//
// Version is injected at link time via
// `-ldflags "-X github.com/takumanakagame/ccmanage/internal/buildinfo.Version=vX.Y.Z"`.
// "dev" is the placeholder for go install / go build.
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
	"time"
)

// Version is the human-facing release label. Overwritten at link time for
// release builds; defaults to "dev" for local development.
var Version = "dev"

// IsDev reports whether the running binary is a dev build (no -ldflags
// version override).
func IsDev() bool { return Version == "dev" }

var (
	once     sync.Once
	hashHex  string
	builtAt  time.Time
	hashErr  error
	statErr  error
)

// Hash returns the first 8 hex chars of SHA-256 over the running binary.
// Same binary bytes produce the same value across runs; rebuilding any
// source file shifts it. Empty on error (we never block startup on this).
func Hash() string {
	resolve()
	return hashHex
}

// BuiltAt returns the binary's mtime — the closest stand-in for "when this
// build was produced" we have without baking a timestamp at link time.
// Zero on error.
func BuiltAt() time.Time {
	resolve()
	return builtAt
}

func resolve() {
	once.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			hashErr = err
			return
		}
		fi, err := os.Stat(exe)
		if err == nil {
			builtAt = fi.ModTime()
		} else {
			statErr = err
		}
		f, err := os.Open(exe)
		if err != nil {
			hashErr = err
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			hashErr = err
			return
		}
		sum := h.Sum(nil)
		hashHex = hex.EncodeToString(sum)[:8]
	})
}
