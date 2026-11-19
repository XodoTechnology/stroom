package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func logOut(format string, a ...any) { fmt.Printf(format+"\n", a...) }
func logErr(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

// newApp resolves flags + config and returns a ready App (engines not yet built).
func newApp(args []string) (*App, error) {
	var addrFlag, dataFlag, dirFlag string
	var webFlag bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-gui": // native window is the default; flag kept for compatibility
		case "-web":
			webFlag = true
		case "-addr":
			i++
			if i < len(args) {
				addrFlag = args[i]
			}
		case "-data":
			i++
			if i < len(args) {
				dataFlag = args[i]
			}
		case "-dir":
			i++
			if i < len(args) {
				dirFlag = args[i]
			}
		case "-h", "--help", "-help":
			usage()
			os.Exit(0)
		}
	}
	dataDir := resolveDataDir(dataFlag)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	cfg, err := loadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	if addrFlag != "" {
		cfg.ListenAddr = addrFlag
	}
	if dirFlag != "" {
		cfg.DownloadDir = dirFlag
	}
	app := &App{DataDir: dataDir, Config: cfg, GUI: !webFlag, version: version}
	if err := app.saveConfig(); err != nil {
		return nil, err
	}
	return app, nil
}

func (a *App) Close() {
	_ = a.saveState()
	if a.Torrent != nil {
		a.Torrent.Close()
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// runAdd talks to a running instance's API.
func runAdd(args []string) error {
	var addr, serverID, typ, dataFlag string
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-data":
			i++
			if i < len(args) {
				dataFlag = args[i]
			}
		case "-addr":
			i++
			if i < len(args) {
				addr = args[i]
			}
		case "-server":
			i++
			if i < len(args) {
				serverID = args[i]
			}
		case "-type":
			i++
			if i < len(args) {
				typ = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		usage()
		return fmt.Errorf("add: no uri or file given")
	}
	target := positional[0]
	if addr == "" {
		addr = defaultAPIAddr(dataFlag)
	}
	return addViaAPI(addr, target, serverID, typ)
}

// addViaAPI sends an add request to a running instance.
// defaultAPIAddr finds the running instance's API address: the api.addr
// file the server writes at startup (covers custom -addr and the
// ephemeral port used when the web UI is disabled), else :8080.
func defaultAPIAddr(dataDir string) string {
	if dataDir == "" {
		dataDir = resolveDataDir("")
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "api.addr")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "127.0.0.1:8080"
}

func addViaAPI(addr, target, serverID, typ string) error {
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	url := "http://" + addr + "/api/add"

	var body []byte
	var ctype string

	// Local file? Read it.
	if b, err := os.ReadFile(target); err == nil {
		req := map[string]any{}
		low := strings.ToLower(target)
		sniff := sniffKind(b)
		switch {
		case strings.HasSuffix(low, ".nzb") || sniff == "nzb":
			req["nzbB64"] = base64.StdEncoding.EncodeToString(b)
			req["type"] = "nzb"
		default:
			req["torrentB64"] = base64.StdEncoding.EncodeToString(b)
			req["type"] = "torrent"
		}
		if typ != "" {
			req["type"] = typ
		}
		if serverID != "" {
			req["serverID"] = serverID
		}
		body, _ = json.Marshal(req)
		ctype = "application/json"
	} else {
		// Treat as URI.
		req := map[string]any{"uri": target}
		if typ != "" {
			req["type"] = typ
		}
		if serverID != "" {
			req["serverID"] = serverID
		}
		body, _ = json.Marshal(req)
		ctype = "application/json"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, ctype, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cannot reach %s — is stroom running? (%v)", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("add failed: %s", strings.TrimSpace(string(b)))
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return nil
}

// runCreateTorrent builds a .torrent file from a path (file or dir).
func runCreateTorrent(args []string) error {
	var trackers []string
	var out string
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-tracker":
			i++
			if i < len(args) {
				trackers = append(trackers, args[i])
			}
		case "-o":
			i++
			if i < len(args) {
				out = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		usage()
		return fmt.Errorf("create: no path given")
	}
	return createTorrentFile(positional[0], out, trackers)
}

// runUpdate checks for a newer release and optionally applies it.
func runUpdate(args []string) error {
	checkOnly := false
	for _, a := range args {
		if a == "--check" || a == "-check" {
			checkOnly = true
		}
	}
	if _, err := newApp(nil); err != nil {
		return err
	}
	repo := updateRepo()
	fmt.Printf("checking %s for updates (current %s)...\n", repo, version)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := checkForUpdate(ctx, repo, version)
	if err != nil {
		return fmt.Errorf("update check failed: %v", infoErr(info, err))
	}
	if !info.Available {
		if info.Latest != "" {
			fmt.Printf("up to date (latest release: %s)\n", info.Latest)
		} else {
			fmt.Println("up to date")
		}
		return nil
	}
	fmt.Printf("update available: %s → %s\n", info.Current, info.Latest)
	fmt.Printf("  %s\n", info.ReleaseURL)
	if checkOnly {
		return nil
	}
	msg, err := applyUpdate(ctx, info)
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

func infoErr(info *updateInfo, err error) string {
	if info != nil && info.Error != "" {
		return info.Error
	}
	return err.Error()
}
