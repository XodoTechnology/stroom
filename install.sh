#!/bin/sh
# Stroom installer — Linux & macOS.
#
#   ./install.sh [path-to-stroom-binary]
#   curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/install.sh | sh
#
# Env:
#   STROOM_REPO   GitHub "owner/name" to download releases from (required
#                 unless installing a local binary)
#   STROOM_VERSION  release tag to install (default: latest)
#   STROOM_PREFIX   install prefix (default: ~/.local)
set -eu

say()  { printf '==> %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

OS=$(uname -s | tr 'A-Z' 'a-z')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported arch: $ARCH" ;;
esac
case "$OS" in
  linux|darwin) ;;
  *) die "unsupported OS: $OS (use install.ps1 on Windows)" ;;
esac

PREFIX=${STROOM_PREFIX:-$HOME/.local}
BINDIR=$PREFIX/bin
mkdir -p "$BINDIR"

# --- pick source: local file arg or GitHub release ---
if [ $# -gt 0 ] && [ -f "$1" ]; then
  SRC=$1
  say "installing local binary: $SRC"
  cp "$SRC" "$BINDIR/stroom"
else
  REPO=${STROOM_REPO:-}
  [ -n "$REPO" ] || die "set STROOM_REPO=owner/name or pass a local binary path"
  TAG=${STROOM_VERSION:-latest}
  if [ "$TAG" = latest ]; then
    say "resolving latest release of $REPO"
    TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$TAG" ] || die "could not resolve latest release"
  fi
  BASE="stroom-$OS-$ARCH"
  URL="https://github.com/$REPO/releases/download/$TAG/$BASE"
  SUMURL="$URL.sha256"
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  say "downloading $URL"
  curl -fsSL "$URL" -o "$TMP/stroom" || die "download failed"
  chmod +x "$TMP/stroom"
  if curl -fsSL "$SUMURL" -o "$TMP/stroom.sha256" 2>/dev/null; then
    WANT=$(awk '{print $1}' "$TMP/stroom.sha256")
    if command -v sha256sum >/dev/null 2>&1; then
      GOT=$(sha256sum "$TMP/stroom" | awk '{print $1}')
    else
      GOT=$(shasum -a 256 "$TMP/stroom" | awk '{print $1}')
    fi
    [ "$WANT" = "$GOT" ] || die "checksum mismatch"
    say "checksum verified"
  else
    say "warning: no checksum asset found, skipping verification"
  fi
  mv "$TMP/stroom" "$BINDIR/stroom"
fi
chmod +x "$BINDIR/stroom"
say "installed $BINDIR/stroom"

# --- PATH hint ---
case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) say "add to PATH:  export PATH=\"$BINDIR:\$PATH\"" ;;
esac

# --- Linux desktop integration ---
if [ "$OS" = linux ]; then
  APPDIR=${XDG_DATA_HOME:-$HOME/.local/share}/applications
  ICONDIR=${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/256x256/apps
  HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
  if [ -f "$HERE/stroom.desktop" ]; then
    mkdir -p "$APPDIR" "$ICONDIR"
    sed "s|@BINDIR@|$BINDIR|g" "$HERE/stroom.desktop" > "$APPDIR/stroom.desktop"
    chmod +x "$APPDIR/stroom.desktop" 2>/dev/null || true
    if [ -f "$HERE/icons/icon-256.png" ]; then
      cp "$HERE/icons/icon-256.png" "$ICONDIR/stroom.png"
    fi
    say "desktop entry + icon installed"
  fi
fi

say "done — run: stroom"
