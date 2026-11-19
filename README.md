<p align="center">
  <img src="icons/icon-256.png" width="96" alt="Stroom logo">
</p>

<h1 align="center">Stroom</h1>

<p align="center">
  A super lightweight torrent + NZB download client.<br>
  Single static binary, embedded web UI, no Electron, no frameworks.
</p>

## Run

```sh
./build.sh                  # builds ./stroom (native window when possible)
./stroom                    # opens the app window on http://127.0.0.1:8080
./stroom -web               # browser-only mode (headless servers, NAS, etc.)
```

### Native window

`./stroom` opens a native desktop window by default (webview: WebKitGTK on
Linux, WebView2 on Windows, WKWebView on macOS). Requirements:

- **Linux**: `libwebkit2gtk-4.0-dev` (or `-4.1-dev`) at build time
- **Windows/macOS arm64, Linux**: nothing extra
- **Windows arm64, macOS Intel**: browser UI fallback (no cgo cross-compile)

`build.sh` detects this automatically and falls back to the browser UI when
webkit is missing. On headless machines (no DISPLAY) it serves the web UI only.
Set `STROOM_NO_BROWSER=1` to skip opening anything.

To run **without any web UI** (native window only, nothing reachable on a
fixed address), enable "Web UI" off in Settings — the app then binds an
ephemeral loopback port just for the native window and `stroom add` still
works via the recorded address.

### Updates

Stroom checks GitHub releases for new versions (once a day, always against the
upstream `XodoTechnology/stroom` repo). When a newer release
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
and icon, and prints a PATH hint.

Release assets include proper installers: **`stroom-X.Y.Z-setup.exe`** (Windows,
per-user NSIS installer with Start Menu + desktop shortcuts and uninstaller),
**`stroom_X.Y.Z_amd64.deb`** (Debian/Ubuntu/Mint, installs to `/usr/bin` with
menu entry + icon), and `install.sh` for everything else. macOS: download the
binary and run it (Gatekeeper: right-click → Open the first time).

Data lives in `~/.stroom` (config, state), downloads in `~/Downloads/Stroom`.
Override with flags: `-addr`, `-data`, `-dir`, or env `STROOM_DATA`.

## Commands

```
stroom                 serve (default)
stroom gui             native window (same as default)
stroom -web            browser/server mode
stroom add <uri|file>  add magnet/.torrent/.nzb/link to running instance
stroom create <path>   create a .torrent from a file or directory
stroom update          check for + install updates
stroom version
```

## Features

- **BitTorrent**: magnets, .torrent files/links, DHT, PEX, UPnP, rate limits,
  per-file priorities, pause/resume, seeding, resume across restarts,
  right-click menu (trackers, recheck, rename, magnet copy, reveal folder).
- **NZB/Usenet**: .nzb files/links, multiple NNTP servers (TLS + auth),
  yEnc decode with CRC32 verify, concurrent segment fetch, retries,
  missing-article detection, resume/rebuild after restart.
- **Web UI**: dark uTorrent-style dashboard, native window by default,
  drag & drop, live SSE stats + speed graph, file lists with priorities,
  completion notifications, watch folder, context menus. Optional UI token auth.

## API

`GET /api/state` · `GET /api/events` (SSE) · `POST /api/add` ·
`POST /api/jobs/{torrent|nzb}/{id}/{pause|resume|remove}` ·
`GET /api/jobs/{kind}/{id}/files` · `POST /api/jobs/torrent/{id}/files/prio` ·
`GET /api/files/{kind}/{id}/{path}` · `/api/servers` · `/api/settings` ·
`GET /api/update` · `POST /api/update/{check|apply}`

## Tests

yEnc codec, NZB parser, NNTP fake-server, full NZB download, and a real
localhost BitTorrent seed→leech swarm:

```sh
go test ./...
```
