#!/bin/sh
# Install (or upgrade) ccdash. Detects the right release asset for the host
# OS/arch, downloads it from the GitHub release, and drops the binary into
# $CCDASH_INSTALL_DIR (default: $HOME/.local/bin). Run again any time to
# pull the latest release.
#
# Quick install:
#   curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh

set -eu

REPO="TakumaNakagame/ccdash"
INSTALL_DIR="${CCDASH_INSTALL_DIR:-$HOME/.local/bin}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) ;;
  *) echo "ccdash: unsupported OS '$OS'" >&2; exit 1 ;;
esac

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "ccdash: unsupported arch '$ARCH_RAW'" >&2; exit 1 ;;
esac

# Resolve the release tag. Operators can short-circuit the GitHub API
# round-trip by setting CCDASH_VERSION=v1.2.3 — useful when:
#   - the host is rate-limited on the GitHub anonymous API (60/hr)
#   - they want a specific historical release rather than latest
TAG="${CCDASH_VERSION:-}"
if [ -z "$TAG" ]; then
  API="https://api.github.com/repos/${REPO}/releases/latest"
  TAG="$(curl -fsSL "$API" \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]\+\)".*/\1/p' \
    | head -n1)"
  if [ -z "${TAG:-}" ]; then
    echo "ccdash: failed to resolve latest release tag from $API" >&2
    echo "ccdash: GitHub may have rate-limited you (anonymous limit is 60/hr)" >&2
    echo "ccdash: workaround — re-run with an explicit version, e.g.:" >&2
    echo "ccdash:   CCDASH_VERSION=v0.1.0 sh install.sh" >&2
    exit 1
  fi
fi

BIN="ccdash-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${BIN}"
SUMURL="${URL}.sha256"

echo "ccdash: installing ${TAG} (${OS}/${ARCH}) into ${INSTALL_DIR}"
mkdir -p "$INSTALL_DIR"

TMP="$(mktemp -t ccdash.XXXXXX)"
trap 'rm -f "$TMP" "$TMP.sha256"' EXIT

curl -fsSL -o "$TMP" "$URL"

# Optional checksum verification when the .sha256 sidecar exists.
if curl -fsSL -o "$TMP.sha256" "$SUMURL" 2>/dev/null; then
  EXPECTED="$(awk '{print $1}' "$TMP.sha256")"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMP" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$TMP" | awk '{print $1}')"
  else
    ACTUAL=""
  fi
  if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "ccdash: checksum mismatch (got $ACTUAL, expected $EXPECTED)" >&2
    exit 1
  fi
fi

# Set 0755 explicitly — `mktemp` creates files mode 0600, and `chmod +x`
# alone preserves the group/other "no read" bits, leaving the binary
# unreadable to anything outside the owner.
chmod 0755 "$TMP"
mv "$TMP" "$INSTALL_DIR/ccdash"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "ccdash: $INSTALL_DIR is not on your PATH — add it to your shell rc";;
esac

# Warn when a different ccdash will still win on PATH (e.g. an earlier
# go-install dev build). Operators usually want the freshly installed
# binary to be the one they invoke.
PATH_BIN="$(command -v ccdash 2>/dev/null || true)"
if [ -n "$PATH_BIN" ] && [ "$PATH_BIN" != "$INSTALL_DIR/ccdash" ]; then
  echo "ccdash: warning — another ccdash is earlier on PATH:"
  echo "  $PATH_BIN"
  echo "  $INSTALL_DIR/ccdash (just installed)"
  echo "ccdash: remove the older one, or reorder PATH so $INSTALL_DIR comes first."
fi

echo "ccdash: installed. Try: $INSTALL_DIR/ccdash --version"
