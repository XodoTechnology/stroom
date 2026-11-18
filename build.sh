#!/bin/sh
# Builds ./stroom. Prefers the native-window build (webview tag);
# falls back to the browser-only build when webkit2gtk isn't installed.
set -e
cd "$(dirname "$0")"

if go build -tags webview -ldflags="-s -w" -o stroom . 2>/dev/null; then
	echo "built ./stroom — native window enabled"
else
	echo "webkit2gtk dev files missing — building browser-only UI"
	echo "(install libwebkit2gtk-4.1-dev for the native window)"
	go build -ldflags="-s -w" -o stroom .
fi
