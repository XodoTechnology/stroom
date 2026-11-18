package main

import (
	"bytes"
	"context"
	"embed"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func serveWebUI(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.Error(w, "ui missing", 500)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	// Never serve dotfiles or traverse.
	if strings.Contains(path, "..") || strings.HasPrefix(path, ".") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(sub, path)
	if err != nil {
		// SPA fallback: serve index.html for unknown paths.
		if !strings.Contains(path, ".") {
			data, err = fs.ReadFile(sub, "index.html")
			path = "index.html"
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if strings.HasSuffix(path, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else if strings.HasSuffix(path, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(path, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	http.ServeContent(w, r, path, time.Now(), bytes.NewReader(data))
}

func runServe(args []string) error {
	app, err := newApp(args)
	if err != nil {
		return err
	}
	defer app.Close()

	// Ensure dirs exist.
	for _, d := range []string{app.DataDir, app.Config.DownloadDir, filepath.Join(app.Config.DownloadDir, "torrents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// Engines.
	te, err := NewTorrentEngine(app)
	if err != nil {
		return err
	}
	app.Torrent = te
	if err := te.Load(); err != nil {
		return err
	}
	ne, err := NewNZBEngine(app)
	if err != nil {
		return err
	}
	app.NZB = ne
	if err := ne.Load(); err != nil {
		return err
	}

	api := newAPIServer(app, app.Config.ListenAddr)
	go func() {
		if err := api.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logErr("http: %v", err)
		}
	}()
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go app.watchLoop(watchCtx)
	go app.maybeAutoUpdateCheck()
	logOut("Stroom listening on http://%s  (download dir: %s)", app.Config.ListenAddr, app.Config.DownloadDir)
	uiURL := "http://" + displayAddr(app.Config.ListenAddr)

	waitForSignal := func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}

	if app.GUI && !webviewBuilt {
		app.GUI = false // browser-only build → just serve the web UI
	}
	if app.GUI && headless() {
		logOut("no display detected — serving web UI only (use -web to silence this)")
		app.GUI = false
	}
	if app.GUI {
		// Native window blocks the main thread; save state on signal.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			app.Close()
			os.Exit(0)
		}()
		if !openNativeWindow(uiURL) {
			// No webview support in this build → browser fallback.
			openBrowser(uiURL)
			waitForSignal()
		}
		// Window closed → fall through to graceful shutdown.
	} else {
		if os.Getenv("STROOM_NO_BROWSER") == "" {
			go func() {
				time.Sleep(300 * time.Millisecond)
				openBrowser(uiURL)
			}()
		}
		waitForSignal()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = api.server.Shutdown(ctx)
	return nil
}

// headless reports whether we're on Linux with no display server —
// opening a GTK webview there would abort the process.
func headless() bool {
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

// displayAddr rewrites wildcard listen addrs to something a browser/webview
// can actually open.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
