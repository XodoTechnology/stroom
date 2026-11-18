package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updateInfo is the cached result of an update check.
type updateInfo struct {
	Configured  bool      `json:"configured"`
	Current     string    `json:"current"`
	Latest      string    `json:"latest,omitempty"`
	Available   bool      `json:"available"`
	Repo        string    `json:"repo,omitempty"`
	ReleaseURL  string    `json:"releaseUrl,omitempty"`
	Asset       string    `json:"asset,omitempty"`
	DownloadURL string    `json:"downloadUrl,omitempty"`
	ChecksumURL string    `json:"checksumUrl,omitempty"`
	CheckedAt   time.Time `json:"checkedAt,omitempty"`
	Error       string    `json:"error,omitempty"`
	Applying    bool      `json:"applying"`
}

// ghAPIBase is overridable in tests.
var ghAPIBase = "https://api.github.com"

// releaseAsset is the subset of a GitHub release asset we need.
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []releaseAsset `json:"assets"`
}

// assetBaseName is the platform-specific binary name used in releases.
func assetBaseName() string {
	os := runtime.GOOS
	if os == "windows" {
		return "stroom-" + os + "-" + runtime.GOARCH + ".exe"
	}
	return "stroom-" + os + "-" + runtime.GOARCH
}

// versionParts parses "v1.2.3" / "1.2.3" into ints (pre-release ignored).
func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// compareVersions returns -1/0/1 comparing dotted numeric versions.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	if pa == nil || pb == nil {
		return 0
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// checkForUpdate queries GitHub for the latest release of repo ("owner/name").
func checkForUpdate(ctx context.Context, repo, current string) (*updateInfo, error) {
	info := &updateInfo{Configured: repo != "", Current: current, Repo: repo, CheckedAt: time.Now()}
	if repo == "" {
		return info, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		ghAPIBase+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Stroom/"+current)
	resp, err := httpClient.Do(req)
	if err != nil {
		info.Error = err.Error()
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		info.Error = "no releases found (or repo is private)"
		return info, fmt.Errorf("github: %s", resp.Status)
	}
	if resp.StatusCode != 200 {
		info.Error = fmt.Sprintf("github api: %s", resp.Status)
		return info, fmt.Errorf("github api: %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		info.Error = err.Error()
		return info, err
	}
	info.Latest = rel.TagName
	info.ReleaseURL = rel.HTMLURL
	want := assetBaseName()
	for _, a := range rel.Assets {
		if a.Name == want {
			info.Asset = a.Name
			info.DownloadURL = a.BrowserDownloadURL
		} else if a.Name == want+".sha256" {
			info.ChecksumURL = a.BrowserDownloadURL
		}
	}
	info.Available = info.DownloadURL != "" && compareVersions(rel.TagName, current) > 0
	return info, nil
}

// applyUpdate downloads the new binary and swaps it in place.
// Returns a message describing what happened; caller should tell user to restart.
func applyUpdate(ctx context.Context, info *updateInfo) (string, error) {
	if info == nil || info.DownloadURL == "" {
		return "", errors.New("no update available")
	}
	if info.ChecksumURL == "" {
		return "", errors.New("release has no checksum asset — refusing to update")
	}
	dir := filepath.Dir(selfPath())
	tmp, err := os.CreateTemp(dir, ".stroom-update-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// Download binary.
	if err := downloadTo(ctx, info.DownloadURL, tmp); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download: %w", err)
	}
	tmp.Close()

	// Verify checksum.
	sum, err := fetchSHA256(ctx, info.ChecksumURL)
	if err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = io.Copy(h, f)
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sum) {
		return "", errors.New("checksum mismatch — download corrupted or tampered")
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", err
	}

	if runtime.GOOS == "windows" {
		return applyUpdateWindows(tmpPath, info.Latest)
	}
	// Unix: rename running exe aside, move new one in. The running process
	// keeps the old inode; next launch gets the new binary.
	self := selfPath()
	old := self + ".old"
	_ = os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		return "", fmt.Errorf("cannot replace %s: %w", self, err)
	}
	if err := os.Rename(tmpPath, self); err != nil {
		_ = os.Rename(old, self) // roll back
		return "", err
	}
	_ = os.Remove(old)
	return fmt.Sprintf("updated to %s — restart stroom to apply", info.Latest), nil
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "stroom"
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

func downloadTo(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Stroom-updater/"+version)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", resp.Status)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 256<<20))
	return err
}

// fetchSHA256 downloads a .sha256 asset and returns the hex digest.
func fetchSHA256(ctx context.Context, url string) (string, error) {
	var sb strings.Builder
	if err := downloadTo(ctx, url, &sb); err != nil {
		return "", err
	}
	fields := strings.Fields(sb.String())
	if len(fields) == 0 {
		return "", errors.New("empty checksum file")
	}
	return fields[0], nil
}

// applyUpdateWindows can't overwrite a running .exe — stage the new binary
// plus a helper batch that swaps after we exit, then relaunch.
func applyUpdateWindows(newExe, tag string) (string, error) {
	dir := filepath.Dir(selfPath())
	final := filepath.Join(dir, "stroom.exe")
	stage := filepath.Join(os.TempDir(), "stroom-update.exe")
	if err := copyFile(newExe, stage); err != nil {
		return "", err
	}
	bat := filepath.Join(os.TempDir(), "stroom-update.bat")
	script := fmt.Sprintf(`@echo off
ping -n 3 127.0.0.1 >nul
move /y "%s" "%s" >nul
start "" "%s"
del "%%~f0"
`, stage, final, final)
	if err := os.WriteFile(bat, []byte(script), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("cmd", "/c", "start", "", "/min", bat)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	return fmt.Sprintf("updated to %s — restart stroom to apply", tag), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// repoForUpdate resolves the configured update repo (config > env).
func repoForUpdate(cfg Config) string {
	if cfg.UpdateRepo != "" {
		return cfg.UpdateRepo
	}
	return os.Getenv("STROOM_REPO")
}
