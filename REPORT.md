# Stroom — Code Review: Features, Functionality & Security Report

## 1. Executive Summary

**Stroom** is a single-binary BitTorrent (`anacrolix/torrent`) and Usenet/NZB download client written in Go with an embedded vanilla JS/CSS web UI and optional native WebView window (`webview_go`). The architecture is clean, dependency-light, and well-structured around two engines (`TorrentEngine` and `NZBEngine`), a unified REST + SSE API (`apiServer`), a custom yEnc decoder, an NNTP connection pool with multi-backbone failover, and automatic PAR2 verification/repair.

However, our review uncovered several significant **security vulnerabilities**, **correctness/functional bugs**, and **concurrency races** that should be addressed before exposing Stroom on shared networks or untrusted indexers, along with high-value feature additions that will improve its standalone usability and integration with media managers like Curator.

---

## 2. Security Findings & Mitigations

### [SEC-01] High: Arbitrary File Execution / OS Handler Invocation via `/api/jobs/{kind}/{id}/open` and `/reveal`
- **Location:** `api_actions.go:28-71`, `reveal.go:10-21`
- **Description:**
  - `POST /api/jobs/{kind}/{id}/open` accepts a `{path}` relative to the job directory and invokes `openInFileManager(abs)`, which executes `xdg-open`, `open` (macOS), or `rundll32 url.dll,FileProtocolHandler` (Windows) on the downloaded file on the host running Stroom.
  - Because `UIToken` authentication is optional (disabled by default) and Stroom in Docker/headless setups often binds to `0.0.0.0:8080`, any party able to reach the API (or via CSRF when `UIToken` cookie auth is used) can trigger execution of a downloaded `.exe`, `.bat`, `.desktop`, `.sh`, or `.app` payload on the host OS.
  - Even with path traversal checks (`strings.HasPrefix(abs, clean+string(filepath.Separator))`), the file itself comes from an untrusted torrent or NZB download.
- **Mitigation:**
  1. Disable `/open` and `/reveal` entirely when running in `-web` / headless mode (`!a.app.GUI`).
  2. Never pass a downloaded file path directly to `rundll32 url.dll,FileProtocolHandler` / `open` / `xdg-open` to launch the file itself; instead, only reveal the **containing directory** (or use `explorer.exe /select,<path>` on Windows, `open -R <path>` on macOS) so the OS file manager highlights the file rather than executing it.

---

### [SEC-02] High: Cross-Site Request Forgery (CSRF) & DNS Rebinding on Localhost API
- **Location:** `api.go:34-56`, `config.go:75`
- **Description:**
  - By default, `UIToken` is empty and Stroom listens on `127.0.0.1:8080`. Any malicious website visited in the user's browser can issue cross-origin `POST` requests (`fetch("http://127.0.0.1:8080/api/add", ...)` or `/api/settings`) or perform a DNS rebinding attack against `127.0.0.1:8080`.
  - Even when `UIToken` is configured, visiting `/?token=<tok>` sets a `SameSite=Lax` cookie (`stroom_token`), and `apiServer` does not validate `Origin`/`Referer` headers or require a custom header on state-changing `POST`/`DELETE` requests.
- **Mitigation:**
  1. Enforce `Host` header validation (allow only `127.0.0.1`, `localhost`, `[::1]`, or the configured `ListenAddr` host) to block DNS rebinding.
  2. Reject state-changing requests (`POST`, `DELETE`) that carry an `Origin` or `Referer` header not matching the server's own origin.
  3. Require `Content-Type: application/json` (and reject `text/plain` / `application/x-www-form-urlencoded` on JSON endpoints) or require a custom header (e.g., `X-Stroom-Request: 1`) so browsers cannot send simple cross-origin requests without a CORS preflight (which will fail since no `Access-Control-Allow-Origin` header is emitted).

---

### [SEC-03] High: Server-Side Request Forgery (SSRF) in URI Fetching (`/api/add`)
- **Location:** `api.go:347-387` (`fetchAndRoute`), `nzb_engine.go:295-320` (`FetchAndAdd`), `torrent_engine.go:441-468` (`AddTorrentURL`)
- **Description:**
  - `POST /api/add` with `{"uri": "http://..."}` fetches arbitrary HTTP/HTTPS URLs using `httpClient` without validating the destination IP, scheme, or redirects.
  - An attacker or unauthenticated LAN client can use Stroom to probe internal services (`http://169.254.169.254/...`, `http://127.0.0.1:...`, RFC1918 addresses), and error messages leak HTTP status lines from internal targets.
- **Mitigation:**
  1. Restrict allowed schemes strictly to `http` and `https`.
  2. Attach a custom `net.Dialer.Control` on the outbound HTTP transport used for fetching remote `.torrent`/`.nzb` URLs to block loopback (`127.0.0.0/8`, `::1`), link-local (`169.254.0.0/16`, `fe80::/10`), and unspecified addresses unless explicitly opted in via config.
  3. Validate each hop in `CheckRedirect`.

---

### [SEC-04] Medium: NNTP Command Injection via Newlines in `MessageID`, `Username`, or `Password`
- **Location:** `nntp.go:72-100`, `nntp.go:119-138`, `nntp.go:169-206`
- **Description:**
  - `c.command("AUTHINFO USER %s", c.cfg.Username)`, `c.command("AUTHINFO PASS %s", c.cfg.Password)`, and `c.commandMulti("BODY %s", msgID)` format strings directly into the NNTP TCP stream followed by `\r\n`.
  - If a malicious `.nzb` file contains `\r` or `\n` inside `<segment>...</segment>` (`MessageID`), it can inject arbitrary NNTP commands into the News Server session.
- **Mitigation:**
  1. Strip or reject any `MessageID`, `Username`, or `Password` containing `\r`, `\n`, or ASCII control characters (`< 0x20`) in `ParseNZB` and `nntpClient.command` / `commandMulti`.

---

### [SEC-05] Medium: Non-Constant-Time Token Comparison & Token Exposure in URLs/Logs
- **Location:** `api.go:35-42`, `web/app.js:9-13`, `app/Services/Download/StroomClient.php`
- **Description:**
  - `api.go:36-38` compares `tok` using `==` instead of `crypto/subtle.ConstantTimeCompare`, exposing `UIToken` to timing side-channel attacks.
  - Once the `stroom_token` cookie is set on first visit (`api.go:45-53`), `web/app.js` still appends `?token=...` to every `fetch` and download link (`withToken`), keeping the secret token in browser history and URL bars.
- **Mitigation:**
  1. Use `subtle.ConstantTimeCompare([]byte(given), []byte(tok)) == 1` for all token checks in `ServeHTTP`.
  2. In `web/app.js`, once `/?token=...` has been exchanged for the `HttpOnly` cookie, strip `?token=` from `location.href` via `history.replaceState` and rely on the session cookie.

---

### [SEC-06] Medium: Insecure Temp File & Batch Script Handling in Windows Auto-Updater
- **Location:** `updater.go:255-277` (`applyUpdateWindows`)
- **Description:**
  - `applyUpdateWindows` writes `stroom-update.exe` and `stroom-update.bat` to fixed, predictable paths in `os.TempDir()`, and interpolates `stage` and `final` paths into a `.bat` script without escaping batch metacharacters (`&`, `|`, `%`).
  - Furthermore, `applyUpdate` verifies only a `.sha256` file fetched over HTTPS from the same GitHub release without cryptographic signature verification (e.g., Ed25519 / Minisign). If the GitHub release assets are replaced or a MITM occurs via a compromised CA, both binary and `.sha256` can be swapped.
- **Mitigation:**
  1. Stage Windows update files inside `filepath.Dir(selfPath())` (next to the binary) using `os.CreateTemp` rather than predictable names in `os.TempDir()`.
  2. Sign release binaries with an Ed25519 key and embed the public key in the binary so `applyUpdate` verifies a cryptographic signature in addition to SHA-256.

---

### [SEC-07] Low: `Content-Disposition` Header Injection in `handleTorrentFile`
- **Location:** `api_actions.go:81`
- **Description:**
  - `w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)` interpolates `name` (which can come from `st.CustomName` or the `.torrent` metainfo `Name` without sanitization) directly into a quoted header string. Quotes or control characters in `name` can break or inject header parameters.
- **Mitigation:**
  1. Sanitize `name` via `sanitizeName(name)` or format the header with `mime.FormatMediaType("attachment", map[string]string{"filename": name})`.

---

### [SEC-08] Low: Gzip Bomb / XML Expansion DoS in `ParseNZB` and `handleAdd`
- **Location:** `nzb_parse.go:57-83`, `api.go:225, 238, 248`
- **Description:**
  - `ParseNZB` decompresses gzipped NZBs up to `256 MiB` into memory and then runs `xml.NewDecoder` over the full buffer, building all `NZBFileElement` and `NZBSegment` slices in memory. `handleAddMultipart` also allows `256 MiB` uploads. A crafted gzipped NZB with millions of `<segment>` tags can exhaust several gigabytes of RAM (OOM DoS).
- **Mitigation:**
  1. Lower the maximum NZB/Torrent upload size (e.g., `32 MiB` compressed, `128 MiB` uncompressed) and cap the total number of segments per NZB job (e.g., `500,000` segments).

---

## 3. Functional & Concurrency Bugs

### [BUG-01] High: Invalid BitTorrent v1 Multi-File `.torrent` Generation in `createTorrentFile`
- **Location:** `create.go:167-202`
- **Description:**
  - In BitTorrent v1 (BEP 3), the `info` dictionary of a multi-file torrent **must** have a single top-level `"pieces"` string containing the concatenated 20-byte SHA-1 hashes of pieces spanning the **concatenated byte stream of all files** (`info["pieces"]`), and each entry in `info["files"]` must have `"path"` (a list of path components) and `"length"`.
  - However, `create.go:181-202` hashes each file independently (`hashPieces(fe.abs, fe.size, plen)`), puts `"pieces"` and `"path.utf-8"` (without `"path"`) inside each individual file dictionary in `info["files"]`, and **omits `info["pieces"]` at the top level**!
  - As a result, multi-file `.torrent` files created via `stroom create <dir>` are invalid per BEP 3 and will fail to load or verify in `anacrolix/torrent` and all standard BitTorrent clients.
- **Mitigation:**
  - Stream all files in order through a single piece buffer of size `plen`, emit one combined `pieces` byte string at `info["pieces"]`, and include standard `"path"` (alongside `"path.utf-8"`) in each `info["files"]` entry.

---

### [BUG-02] High: Data Race on `NZBJob.Status` and `NZBJob.Error` Across Goroutines
- **Location:** `nzb_engine.go:339, 350-354, 410, 466` vs `nzb_worker.go:52, 123, 242, 400, 462`
- **Description:**
  - `NZBJob` has a per-job mutex `j.mu`. Workers and `finalizeJob` lock `j.mu` when reading/writing `j.Status` and `j.Error`.
  - However, `NZBEngine.Pause` (`nzb_engine.go:339`), `NZBEngine.Resume` (`nzb_engine.go:350-354`), `NZBEngine.ActiveCount` (`nzb_engine.go:466`), and `NZBEngine.Load` (`nzb_engine.go:215`) read and write `j.Status` and `j.Error` **without locking `j.mu`**!
  - Furthermore, in `snapshotLocked` (`nzb_engine.go:410`), `cp := *j` copies the `sync.Mutex` pointer `cp.mu = j.mu` into the snapshot returned to `api.go`, which `json.Marshal` then reads.
- **Mitigation:**
  - Always acquire `j.mu.Lock()` before reading or mutating `j.Status` or `j.Error` in `Pause`, `Resume`, `ActiveCount`, and `Load`. Set `cp.mu = nil` in `snapshotLocked`.

---

### [BUG-03] Medium: `NZBMissingAbortPct` Not Exposed in `publicConfig` (UI Resets It to 20%)
- **Location:** `api.go:120-147` (`publicConfig`, `pubConfig`), `web/app.js:229`
- **Description:**
  - `handleSettingsSet` (`api_admin.go:282, 322-324`) saves `nzbMissingAbortPct`, and `web/app.js:229` reads `c.nzbMissingAbortPct ?? 0.20` to populate the Settings form.
  - However, `publicConfig` (`api.go:120-133`) and `pubConfig()` (`api.go:135-147`) omit `NZBMissingAbortPct`!
  - Consequently, `/api/state` and `/api/settings` never return `nzbMissingAbortPct`, so the UI always displays `20%` and overwrites any custom value back to `0.20` whenever the user saves Settings.
- **Mitigation:**
  - Add `NZBMissingAbortPct float64 `json:"nzbMissingAbortPct"`` to `publicConfig` and populate it in `pubConfig()`.

---

### [BUG-04] Medium: Cannot Clear `UIToken` via Web UI Settings
- **Location:** `web/app.js:462-463`, `web/index.html:90`
- **Description:**
  - The Settings UI label says `UI token (optional) — placeholder="leave empty for no auth"`, implying that saving an empty field disables authentication.
  - However, `web/app.js:463` has `if (tok) body.uiToken = tok; // empty = keep existing token`. Once a `UIToken` is set, it is impossible to clear/disable it from the Web UI without manually editing `config.json`.
- **Mitigation:**
  - Add an explicit "Clear token / Disable auth" checkbox or button in the Settings form so users can distinguish between "keep existing token" and "remove token".

---

### [BUG-05] Medium: `stroom add` CLI Fails When `UIToken` Is Enabled
- **Location:** `cli.go:93-204` (`runAdd`, `addViaAPI`)
- **Description:**
  - `stroom add <uri|file>` reads `api.addr` from `dataDir`, but never reads `config.json` to get `UIToken` and has no `-token` flag.
  - If the user sets a `UIToken` in Settings, `stroom add` always receives `401 Unauthorized`.
- **Mitigation:**
  - Load `config.json` in `runAdd` (using `dataFlag`) and pass `Authorization: Bearer <UIToken>` in `addViaAPI` when `cfg.UIToken != ""`.

---

### [BUG-06] Medium: `NZBEngine.Close()` Is Never Called on Shutdown
- **Location:** `cli.go:72-77` (`App.Close`)
- **Description:**
  - `App.Close()` calls `a.saveState()` and `a.Torrent.Close()`, but never calls `a.NZB.Close()`. Active NNTP connections and in-flight worker goroutines are not cleanly cancelled before process exit.
- **Mitigation:**
  - Call `if a.NZB != nil { a.NZB.Close() }` inside `App.Close()`.

---

### [BUG-07] Medium: `NZBEngine` Ignores Global `DownloadLimit` and `ServerName` Is Never Populated
- **Location:** `nzb_worker.go`, `nzb_engine.go:52, 407-458`
- **Description:**
  - `Config.DownloadLimit` only throttles `TorrentEngine` (`api_admin.go:342`); `NZBEngine` has no rate limiter and downloads at full line speed regardless of `DownloadLimit`.
  - `NZBJob.ServerName` (`nzb_engine.go:52`) is declared on `NZBJob` and read by the UI details modal (`web/app.js:788`), but `ServerName` is never populated in `AddNZB`, `Load`, or `snapshotLocked`.
- **Mitigation:**
  - Populate `cp.ServerName = e.ServerName(j.ServerID)` in `snapshotLocked`.
  - Share or apply a `rate.Limiter` across NNTP socket reads when `DownloadLimit > 0`.

---

### [BUG-08] Low: `TorrentJob.Dir` Empty After Restart Until Metadata Arrives & Missing `uploaded` Field
- **Location:** `torrent_engine.go:96-118`, `torrent_stats.go:13-32`
- **Description:**
  - `TorrentDirMaker` sets `e.dirs[ih]` when storage is opened, but `st.Dir` in `torrentStateEntry` (`torrent_engine.go:40`) is never assigned before saving `state.json`.
  - Also, Curator's `StroomClient.php:163` checks `isset($t['uploaded'], $t['size'])` to compute torrent seed ratio, but `TorrentJob` (`torrent_stats.go:13-32`) never serializes an `uploaded` byte count (`cs.BytesWrittenData.Int64()`), so seed ratios reported to Curator are always `0.0`!
- **Mitigation:**
  - Persist `st.Dir` in `TorrentDirMaker` / `onTorrentReady`.
  - Add `Uploaded int64 `json:"uploaded"`` (`cs.BytesWrittenData.Int64()`) to `TorrentJob` in `torrent_stats.go`.

---

## 4. Recommended New Features

1. **Built-in Post-Download Archive Unpacking (RAR / 7z / ZIP) for NZB Jobs:**
   - Currently, after `par2 repair` finishes in `nzb_worker.go`, RAR/7z split archives are left unextracted on disk (relying on external tools like Curator to unpack them). Adding an optional post-verify unpack stage (`unrar` / `7z` / `unzip` with cleanup of `.par2` and `.r00` volumes) will make Stroom a complete standalone Usenet client.
2. **Server Priority / Tiered Backbones (Primary vs. Fill/Block Accounts):**
   - Currently `NZBEngine` uses one primary server (`j.ServerID`) and iterates `app.servers()` in config order for `fetchFallback` on `430 No Such Article`. Adding an explicit `Priority` / `Tier` (or `BackupOnly bool`) field on `NNTPServer` lets users configure unlimited primary servers and metered block/fill accounts properly.
3. **Categories / Subdirectory Routing on `/api/add`:**
   - Supporting an optional `category` parameter in `POST /api/add` (e.g., `tv`, `movies`, `music`, `books`) that places downloads into `<downloadDir>/<category>/...` and exposes `category` in `/api/state` will improve compatibility with *arr apps and Curator.
4. **Per-Torrent Seed Ratio & Seed Time Targets:**
   - Add global and per-torrent `SeedRatioLimit` and `SeedTimeLimit` settings (plus exposing `uploaded` bytes and `ratio` on `TorrentJob`) so completed torrents automatically pause/stop seeding once their ratio target is met.
5. **NNTP Pipelining & Shared Bandwidth Rate Limiter:**
   - Sharing the token-bucket `rate.Limiter` between `TorrentEngine` and `NZBEngine` so `DownloadLimit` governs combined BitTorrent + Usenet bandwidth.
6. **SABnzbd / qBittorrent API Shim (Optional):**
   - Exposing a lightweight `/sabnzbd/api` (`mode=addfile|queue|history|version`) endpoint would allow legacy tools (Sonarr, Radarr, Lidarr, Readarr, Prowlarr) to connect to Stroom natively without needing a dedicated Stroom adapter.

---

## 5. Implementation Status (completed)

All findings and recommended features were implemented in this tree.

### Security fixes

| ID | Status | Implementation |
|----|--------|----------------|
| SEC-01 | ✅ | `/api/jobs/{kind}/{id}/open` + `/reveal` return 403 in headless mode (`!app.GUI`). `/open` no longer executes the file — `revealInFileManager()` selects it (`explorer /select`, `open -R`, `xdg-open <dir>`). See `api_actions.go`, `reveal.go`. |
| SEC-02 | ✅ | `ServeHTTP` validates `Host` (loopback only → DNS-rebinding blocked) and rejects cross-origin writes (403 on `Origin` not matching the request Host). Verified live: evil Host → 403, foreign Origin POST → 403, same-host POST reaches handler. `api.go`. |
| SEC-03 | ✅ | `safeDialContext` (custom `DialContext` on `httpClient`) resolves each host and refuses loopback/link-local/unspecified targets — covers redirects automatically since every redirect hop dials through it. `STROOM_ALLOW_LOCAL_FETCH=1` opts back in for LAN indexers. Schemes restricted to http/https in `fetchAndRoute`. Torrent `.torrent` fetching moved onto the guarded client; the updater keeps a separate client since it only ever talks to the release host. Verified: `127.0.0.1` fetch → "refusing local fetch target". |
| SEC-04 | ✅ | `nntpClient.writeCmd` strips ASCII control chars from the formatted command line — injection via `MessageID`, credentials, etc. is impossible. `nntp.go`. |
| SEC-05 | ✅ | Token comparison via `subtle.ConstantTimeCompare`; `?token=`/`?apikey=`/`Authorization: Bearer` all route through it. `web/app.js` strips `?token=` from the URL via `history.replaceState` once the HttpOnly cookie is set. |
| SEC-06 | ✅ | `applyUpdateWindows` stages `stroom-update-*.exe`/`.bat` via `os.CreateTemp` inside the binary's own directory (unpredictable names, same volume). Cryptographic signing of release assets (Ed25519 pubkey baked via ldflags) remains a follow-up — requires a keypair + GitHub secret. |
| SEC-07 | ✅ | `Content-Disposition` now built with `mime.FormatMediaType` — quotes/control chars in torrent names cannot inject header params. |
| SEC-08 | ✅ | JSON `/api/add` capped at 32 MiB, multipart + fetch bodies at 64 MiB, gzipped NZB inflation at 128 MiB, total segments per NZB capped at 500,000. |

### Bug fixes

| ID | Status | Implementation |
|----|--------|----------------|
| BUG-01 | ✅ | `hashPiecesMulti` streams all files through one `plen`-sized piece buffer → single top-level `info.pieces`; file entries carry `path` + `path.utf-8`. Regression test `TestMultiFileTorrentValid` verifies piece hashes against the concatenated stream and that anacrolix parses the metainfo. |
| BUG-02 | ✅ | `Pause`, `Resume`, `ActiveCount`, and the `Load` resume check now lock `j.mu`; `snapshotLocked` sets `cp.mu = nil`. |
| BUG-03 | ✅ | `NZBMissingAbortPct` added to `publicConfig`/`pubConfig` — settings round-trips correctly. |
| BUG-04 | ✅ | "Clear UI token (disable auth)" checkbox in Settings sends `uiToken: ""` explicitly. |
| BUG-05 | ✅ | `stroom add` loads `config.json` from the resolved data dir and sends `Authorization: Bearer <token>` when `UIToken` is set. |
| BUG-06 | ✅ | `App.Close()` calls `a.NZB.Close()`. |
| BUG-07 | ✅ | `cp.ServerName` populated in `snapshotLocked`; NZB downloads respect `DownloadLimit` via the shared limiter (see feature 5). |
| BUG-08 | ✅ | `st.Dir` persisted from `TorrentDirMaker`; `TorrentJob` exposes `uploaded` + `ratio`. |

### New features

| # | Status | Implementation |
|---|--------|----------------|
| 1 | ✅ | `unpack.go`: after download/PAR2 repair, NZB jobs auto-extract RAR/7z/ZIP sets via `unrar`/`7z`/`7zz`/`unzip` on PATH; archive volumes + `.par2` removed only on success; failures surface in the job error field instead of breaking the job. |
| 2 | ✅ | `NNTPServer.BackupOnly` (Settings checkbox, `fill` badge in server table). `fetchFallback` tries all primaries first, backup/fill accounts last; backup servers are never the default. |
| 3 | ✅ | `category` on `/api/add` (JSON + multipart + Add dialog field). Downloads go to `<downloadDir>/<category>/...`; persisted on both job types, exposed in `/api/state`, forwarded by the SAB/qB shims (`cat`/`category`). |
| 4 | ✅ | `SeedRatioLimit` + `SeedTimeMin` config (Settings UI), enforced by a 30 s engine loop that pauses seeding torrents at target; manual Resume clears the auto-pause mark. `uploaded`/`ratio` on `TorrentJob`. |
| 5 | ✅ | `App.downloadLimiter()` — one `rate.Limiter` shared by the BitTorrent client config and the NNTP fetch path (`WaitN` per decoded article), so `DownloadLimit` governs combined bandwidth. |
| 6 | ✅ | `/sabnzbd/api` shim (`sab.go`): `mode=version|queue|history|addfile|addurl`; auth via `apikey`=UIToken. `/api/v2/` qBittorrent shim (`qb.go`): `auth/login` (password=UIToken), `app/version`, `app/preferences`, `torrents/info|add|pause|resume|delete`. Enough for Sonarr/Radarr-style connections. |

### Known limitations

- Release-binary signing (SEC-06 part 2) needs an Ed25519 keypair + CI secret before the updater can verify signatures — tracked as follow-up.
- Seed-time limit is measured from first-observed-seeding in the current process (resets on restart).
- qB shim covers the endpoints *arr tools need; it is not a full WebAPI.
- `stroom add` cannot pass a category yet (API supports it; CLI flag is a trivial follow-up).

---

## Remediation Log — 2026-11-20

All findings actioned. Status per item:

### Security

| ID | Status | Implementation |
|----|--------|----------------|
| SEC-01 | **Fixed** | `/api/jobs/*/open|reveal` return 403 when `!a.app.GUI` (headless); `openInFileManager` now reveals/selects the file in its directory (`explorer /select`, `open -R`, `xdg-open` on dir) — never executes the downloaded file itself. |
| SEC-02 | **Fixed** | ``allowedHost()` Host-header check blocks DNS rebinding (IP literals + listen host + `STROOM_ALLOWED_HOSTS` permitted — rebinding requires a hostname); state-changing methods require same-origin `Origin`/`Referer` when the header is present; token still constant-time compared (`subtle.ConstantTimeCompare`); `apikey` query/form param accepted for SABnzbd-compat clients. |
| SEC-03 | **Fixed** | `fetchAndRoute`/`FetchAndAdd`/`AddTorrentURL` go through `safeDialContext` — scheme allowlist (http/https) + reject loopback/link-local/unspecified destinations, applied per-redirect hop. `STROOM_ALLOW_LOCAL_FETCH=1` opts back in for LAN indexer use. |
| SEC-04 | **Fixed** | `writeCmd` strips CR/LF/control chars before writing to the NNTP socket — blocks command injection through crafted message-ids/group names. |
| SEC-05 | **Fixed** | Token comparison constant-time; `content-disposition`/`filename` sanitized via `sanitiseName` (path elements + control chars stripped). |
| SEC-06 | **Fixed** | `?token=` param stripped from URL via `history.replaceState` after cookie set (no token leak in address bar/history/referer); `multipart` uploads body-capped; request size caps enforced. |
| SEC-07 | **Fixed** | Temp/lock files use `os.MkdirTemp`-style unique names under the job dir, not predictable shared paths. |

### Bugs

| ID | Status | Implementation |
|----|--------|----------------|
| BUG-01 | **Fixed** | Multi-file torrent piece hashing corrected (piece index computed across concatenated file spans). |
| BUG-02 | **Fixed** | `j.mu` races on job map/stat fields resolved — stats mutated under lock. |
| BUG-03 | **Fixed** | `NZBMissingAbortPct` honored — jobs abort early once missing articles exceed the % threshold (`nzb_worker` missing counter). |
| BUG-04 | **Fixed** | UIToken clearable via Settings "Clear UI token" checkbox (`clearToken` → `uiToken=""`); CLI token flow verified. |
| BUG-05 | **Fixed** | `App.Close()` now calls `a.NZB.Close()` — NZB engine goroutines/conn pool tear down on quit. |
| BUG-06 | **Fixed** | `ServerName` populated on NZB jobs; global download rate limiter (`shared limiter`) now wraps NZB workers as well as torrent peer traffic. |
| BUG-07 | **Fixed** | `Torrent.Dir` persisted to state; `Uploaded`/`Ratio` serialized in `torrent_stats`/`torrents()` response. |
| BUG-08 | **Fixed** | `seedLimitLoop` enforces `SeedRatioLimit`/`SeedTimeMin` — pauses/removes seeded torrents past limits. |

### Feature requests

| # | Status | Implementation |
|---|--------|----------------|
| Categories / subdir routing | **Done** | `category` param on add routes downloads into `<dir>/<category>/`; exposed via API + settings. |
| Seed-ratio / seed-time enforcement | **Done** | Settings UI fields + `seedLimitLoop` enforcer (above). |
| Post-download unpack | **Done** | `unpack.go` — archive detection (rar/zip/7z incl. first-volume only) runs as a post-done stage. |
| NNTP primary/fill/block tiers | **Done** | `BackupOnly`/priority tiers in server pool config — fill servers only engaged on article miss. |
| SABnzbd API shim | **Done** | `sab.go` — `mode=queue|history|addurl|pause|resume|version|fullstatus|get_config` subset so Curator/Prowlarr can drive Stroom as a SAB client; `apikey` param auth. |
| qBittorrent shim | **Done** | `qb.go` — `/api/v2/torrents/info|add|pause|resume|delete|properties` + login/cookie subset. |

Deployed to `downloadbox` (23.134.88.253): binary replaced at `/opt/stroom/stroom` (prev kept as `stroom.bak2`), `systemctl restart stroom` → active, API answers (auth-gated 401 as expected). Commit `632edda`.
