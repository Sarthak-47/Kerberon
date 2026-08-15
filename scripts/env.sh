#!/usr/bin/env bash
# Kerberon development environment (Linux / macOS / WSL).
#
# Source it before running any go command:
#
#     source ./scripts/env.sh
#
# Redirects every Go cache and config path into the project folder so nothing is
# written outside it (CLAUDE.md R1/R2).
#
# Uses .toolchain/go if a toolchain for this platform has been placed there;
# otherwise falls back to the system Go. .toolchain/ is git-ignored, so a fresh
# clone on Linux will use system Go unless you populate it yourself.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -x "$ROOT/.toolchain/go/bin/go" ]; then
    export GOROOT="$ROOT/.toolchain/go"
    export PATH="$GOROOT/bin:$PATH"
elif command -v go >/dev/null 2>&1; then
    unset GOROOT
    echo "note: using system Go ($(command -v go)); no toolchain in .toolchain/"
else
    echo "error: no Go toolchain found. Install Go 1.23+ or populate .toolchain/go" >&2
    return 1 2>/dev/null || exit 1
fi

export GOPATH="$ROOT/.gopath"
export GOMODCACHE="$ROOT/.gopath/pkg/mod"
export GOBIN="$ROOT/.gopath/bin"
export GOCACHE="$ROOT/.gocache"
export GOTMPDIR="$ROOT/.tmp"
export GOENV="$ROOT/.gopath/env"
export GOTOOLCHAIN="local"
export PATH="$GOBIN:$PATH"

mkdir -p "$GOPATH" "$GOMODCACHE" "$GOBIN" "$GOCACHE" "$GOTMPDIR"

# GOTELEMETRY and GOTELEMETRYDIR are non-settable; exporting them does nothing.
# The only control is `go telemetry off`, and the data lives under
# os.UserConfigDir(), outside the project folder. Warn rather than fail silently.
mode="$(go telemetry 2>/dev/null || true)"
if [ -n "$mode" ] && [ "$mode" != "off" ]; then
    echo "warning: Go telemetry is '$mode'; the toolchain writes counter files to" >&2
    echo "  $(go env GOTELEMETRYDIR)" >&2
    echo "which is outside the project folder (CLAUDE.md R1). Disable with: go telemetry off" >&2
fi

echo "Kerberon env ready"
echo "  go       $(go version)"
echo "  GOPATH   $GOPATH"
echo "  GOCACHE  $GOCACHE"
