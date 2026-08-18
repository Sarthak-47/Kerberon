#!/usr/bin/env sh
# Kerberon installer.
#
#   curl -L https://kerberon.sh/install | sh
#
# Downloads the release binary for this platform, verifies it against the
# published checksums, and installs it. POSIX sh rather than bash, because the
# machines that most need a pager are often the ones with the fewest shells.

set -eu

REPO="Sarthak-47/Kerberon"
BIN="kerberon"
INSTALL_DIR="${KERBERON_INSTALL_DIR:-/usr/local/bin}"
VERSION="${KERBERON_VERSION:-latest}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

need uname
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
    die "either curl or wget is required"

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    else
        wget -qO "$2" "$1"
    fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$os" in
    linux|darwin) ;;
    *) die "unsupported OS: $os. Windows users: download the .exe from the releases page." ;;
esac

case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
esac

if [ "$VERSION" = "latest" ]; then
    base="https://github.com/$REPO/releases/latest/download"
else
    base="https://github.com/$REPO/releases/download/$VERSION"
fi

asset="${BIN}_${os}_${arch}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $asset..."
fetch "$base/$asset" "$tmp/$BIN" || die "download failed; check that $VERSION exists"

# Verify before installing. A pager is on the critical path for incident
# response, and running an unverified binary from the internet as part of
# setting one up would be a poor start.
if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
    if command -v sha256sum >/dev/null 2>&1; then
        expected=$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
        actual=$(sha256sum "$tmp/$BIN" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        expected=$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
        actual=$(shasum -a 256 "$tmp/$BIN" | awk '{print $1}')
    else
        expected=""
    fi

    if [ -n "${expected:-}" ]; then
        [ "$expected" = "$actual" ] || die "checksum mismatch; refusing to install"
        say "checksum verified"
    else
        say "warning: no sha256 tool found, skipping verification"
    fi
else
    say "warning: could not fetch checksums, skipping verification"
fi

chmod +x "$tmp/$BIN"

if [ -w "$INSTALL_DIR" ]; then
    mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
else
    say "$INSTALL_DIR is not writable; using sudo"
    need sudo
    sudo mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
fi

say ""
say "installed: $INSTALL_DIR/$BIN"
"$INSTALL_DIR/$BIN" version || true
say ""
say "next:"
say "  1. write a kerberon.yaml   (see https://github.com/$REPO/blob/main/examples/kerberon.yaml)"
say "  2. kerberon validate --config kerberon.yaml"
say "  3. kerberon serve --config kerberon.yaml"
