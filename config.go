package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// NNTPServer describes a Usenet news server used for NZB downloads.
type NNTPServer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      bool   `json:"tls"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Max simultaneous connections to this server.
	Connections int `json:"connections"`
}

func (s NNTPServer) Addr() string {
	return net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
}

// Config is the user-editable configuration, persisted to config.json.
type Config struct {
	// Address the web UI + API listens on.
	ListenAddr string `json:"listenAddr"`
	// Optional token required to use the UI/API (empty = no auth).
	UIToken string `json:"uiToken,omitempty"`
	// Base directory for all downloads (torrents + NZB output).
	DownloadDir string `json:"downloadDir"`
	// BitTorrent listen port. 0 = random.
	BTListenPort int `json:"btListenPort"`
	// Enable DHT.
	EnableDHT bool `json:"enableDHT"`
	// Enable UPnP/NAT-PMP port forwarding.
	EnableUPnP bool `json:"enableUpnp"`
	// Enable peer exchange.
	EnablePEX bool `json:"enablePex"`
	// Global rate limits in bytes/sec. 0 = unlimited.
	DownloadLimit int64 `json:"downloadLimit"`
	UploadLimit   int64 `json:"uploadLimit"`
	// Default NZB worker connections per server.
	NZBMaxConnections int `json:"nzbMaxConnections"`
	// News servers for NZB downloads.
	NNTPServers []NNTPServer `json:"nntpServers"`
	// Watch folder: auto-add .torrent/.nzb files dropped here.
	WatchDir string `json:"watchDir,omitempty"`
	// Disable the web UI: the app then only serves the API on an
	// ephemeral loopback port for the native window (no browser UI).
	DisableWebUI bool `json:"disableWebUI,omitempty"`
	// Last time an update check ran (RFC3339), for daily throttling.
	LastUpdateCheck string `json:"lastUpdateCheck,omitempty"`
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	return Config{
		ListenAddr:        "127.0.0.1:8080",
		DownloadDir:       filepath.Join(home, "Downloads", "Stroom"),
		BTListenPort:      6881,
		EnableDHT:         true,
		EnableUPnP:        true,
		EnablePEX:         true,
		NZBMaxConnections: 12,
	}
}

// resolveDataDir picks the data directory: flag > STROOM_DATA > ~/.stroom
func resolveDataDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("STROOM_DATA"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".stroom")
}

// App bundles runtime state for one running instance.
type App struct {
	DataDir string
	Config  Config
	Torrent *TorrentEngine
	NZB     *NZBEngine
	GUI     bool
	version string
	cfgMu   sync.RWMutex // guards Config fields read on hot paths
	updMu   sync.Mutex   // guards upd
	upd     *updateInfo  // cached update check result
}

// uiToken returns the configured UI token (lock-safe).
func (a *App) uiToken() string {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config.UIToken
}

// watchDir returns the configured watch folder (lock-safe).
func (a *App) watchDir() string {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config.WatchDir
}

// downloadDir returns the configured download dir (lock-safe).
func (a *App) downloadDir() string {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config.DownloadDir
}

// servers returns a copy of the NNTP server list (lock-safe).
func (a *App) servers() []NNTPServer {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	out := make([]NNTPServer, len(a.Config.NNTPServers))
	copy(out, a.Config.NNTPServers)
	return out
}

// configSnapshot returns a shallow copy of Config for read-only display.
// Slice fields must be cloned separately via servers().
func (a *App) configSnapshot() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config
}

// lockConfig exposes the config mutex for handlers that mutate+save atomically.
func (a *App) lockConfig()   { a.cfgMu.Lock() }
func (a *App) unlockConfig() { a.cfgMu.Unlock() }

func loadConfig(dataDir string) (Config, error) {
	cfg := DefaultConfig()
	path := filepath.Join(dataDir, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

func (a *App) saveConfig() error {
	b, err := json.MarshalIndent(a.Config, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.DataDir, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (a *App) saveState() error {
	if a.Torrent != nil {
		if err := a.Torrent.Save(); err != nil {
			return err
		}
	}
	if a.NZB != nil {
		return a.NZB.Save()
	}
	return nil
}

var platformName = runtime.GOOS + "/" + runtime.GOARCH

func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }

// sanitizeName makes a string safe for use as a single path component.
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == 0:
			out = append(out, '_')
		case r < 32:
			// drop control chars
		default:
			out = append(out, r)
		}
	}
	res := string(out)
	if len(res) > 120 {
		res = res[:120]
	}
	res = filepath.Clean(res)
	if res == "." || res == ".." || res == "" {
		res = "unnamed"
	}
	return res
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var errNotFound = errors.New("not found")

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

// idGen produces short unique job ids.
type idGen struct{ n atomic.Int64 }

func (g *idGen) next(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), g.n.Add(1))
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
