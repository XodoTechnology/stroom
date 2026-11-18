# Stroom

A super lightweight torrent + NZB download client. Single static binary,
embedded web UI, no Electron, no frameworks.

## Run

```sh
./build.sh                  # builds ./stroom (native window when possible)
./stroom                    # opens the app window on http://127.0.0.1:8080
./stroom -web               # browser-only mode (headless servers, NAS, etc.)
```

### Logo & icons

`logo.svg` is the source mark; `icons/` has PNG renders (used by the desktop
entry, installer, and web UI favicon).

### Updates

Stroom checks GitHub releases for new versions (once a day, configurable via
`updateRepo` in config or Settings → "Update repo"). When a newer release
exists a banner appears in the UI; **Update** downloads the platform binary,
verifies its SHA-256 against the release's `.sha256` asset, and swaps it in —
restart to apply. CLI equivalent:

```sh
stroom update --check   # see if an update exists
stroom update           # download + install it
```

Releases are built by `.github/workflows/release.yml` on every `v*` tag —
six platform binaries plus `.sha256` checksums.

### Install

```sh
# from a release (set your repo first):
STROOM_REPO=you/stroom ./install.sh

# or from a local build:
./install.sh ./stroom
```

`install.sh` puts the binary in `~/.local/bin`, adds a Linux `.desktop` entry
and icon, and prints a PATH hint. Windows: download the `.exe` release asset.

### Native window

`./stroom` opens a native desktop window by default (webview: WebKitGTK on
Linux, WebView2 on Windows, WKWebView on macOS). Requirements:

- **Linux**: `libwebkit2gtk-4.1-dev` (or `-4.0-dev`) at build time
- **Windows/macOS**: nothing extra

`build.sh` detects this automatically and falls back to the browser UI when
webkit is missing. On headless machines (no DISPLAY) it serves the web UI only.
Set `STROOM_NO_BROWSER=1` to skip opening anything.

### Logo & icons

`logo.svg` is the source mark; `icons/` has PNG renders (used by the desktop
entry, installer, and web UI favicon).

### Updates

Stroom checks GitHub releases for new versions (once a day, configurable via
`updateRepo` in config or Settings → "Update repo"). When a newer release
exists a banner appears in the UI; **Update** downloads the platform binary,
verifies its SHA-256 against the release's `.sha256` asset, and swaps it in —
restart to apply. CLI equivalent:

```sh
stroom update --check   # see if an update exists
stroom update           # download + install it
```

Releases are built by `.github/workflows/release.yml` on every `v*` tag —
six platform binaries plus `.sha256` checksums.

### Install

```sh
# from a release (set your repo first):
STROOM_REPO=you/stroom ./install.sh

# or from a local build:
./install.sh ./stroom
```

`install.sh` puts the binary in `~/.local/bin`, adds a Linux `.desktop` entry
and icon, and prints a PATH hint. Windows: download the `.exe` release asset.

### Native window

The default build serves the UI in your browser. For a real desktop window:

```sh
# Linux: needs webkit2gtk dev package
sudo apt install libwebkit2gtk-4.1-dev   # or libwebkit2gtk-4.0-dev
go build -tags webview -o stroom .
./stroom gui
```

macOS/Windows work out of the box with `-tags webview`. Without the tag,
`stroom gui` prints a notice and falls back to the browser.

Data lives in `~/.stroom` (config, state), downloads in `~/Downloads/Stroom`.
Override with flags: `-addr`, `-data`, `-dir`, or env `STROOM_DATA`.

## Commands

```
stroom                 serve (default)
stroom add <uri|file>  add magnet/.torrent/.nzb/link to running instance
stroom create <path>   create a .torrent from a file or directory
stroom version
```

## Features

- **BitTorrent**: magnets, .torrent files/links, DHT, PEX, UPnP, rate limits,
  per-file priorities, pause/resume, seeding, resume across restarts.
- **NZB/Usenet**: .nzb files/links, multiple NNTP servers (TLS + auth),
  yEnc decode with CRC32 verify, concurrent segment fetch, retries,
  missing-article detection, resume/rebuild after restart.
- **Web UI**: dark uTorrent-style dashboard, live SSE stats, add dialog,
  file lists, server + settings management. Optional UI token auth.

## API

`GET /api/state` · `GET /api/events` (SSE) · `POST /api/add` ·
`POST /api/jobs/{torrent|nzb}/{id}/{pause|resume|remove}` ·
`GET /api/jobs/{kind}/{id}/files` · `POST /api/jobs/torrent/{id}/files/prio` ·
`GET /api/files/{kind}/{id}/{path}` · `/api/servers` · `/api/settings`

## Tests

yEnc codec, NZB parser, NNTP fake-server, full NZB download, and a real
localhost BitTorrent seed→leech swarm:

```sh
go test ./...
```
