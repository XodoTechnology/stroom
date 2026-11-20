package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// apiServer wires the REST + SSE API onto an *App.
type apiServer struct {
	app    *App
	mux    *http.ServeMux
	server *http.Server
}

func newAPIServer(app *App, addr string) *apiServer {
	a := &apiServer{app: app, mux: http.NewServeMux()}
	a.routes()
	a.server = &http.Server{
		Addr:              addr,
		Handler:           a,
		ReadHeaderTimeout: 15 * time.Second,
	}
	return a
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if tok := a.app.uiToken(); tok != "" {
		ok := r.URL.Query().Get("token") == tok ||
			r.Header.Get("Authorization") == "Bearer "+tok ||
			cookieToken(r) == tok
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// First visit carries ?token= — persist it as a cookie so static
		// assets (css/js) and later API calls authenticate without the param.
		if r.URL.Query().Get("token") == tok && cookieToken(r) != tok {
			http.SetCookie(w, &http.Cookie{
				Name:     "stroom_token",
				Value:    tok,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
	}
	a.mux.ServeHTTP(w, r)
}

func cookieToken(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == "stroom_token" {
			return c.Value
		}
	}
	return ""
}

func (a *apiServer) routes() {
	m := a.mux
	m.HandleFunc("GET /api/state", a.handleState)
	m.HandleFunc("GET /api/events", a.handleEvents)
	m.HandleFunc("POST /api/add", a.handleAdd)
	m.HandleFunc("POST /api/jobs/{kind}/{id}/{action}", a.handleJobAction)
	m.HandleFunc("GET /api/jobs/{kind}/{id}/files", a.handleJobFiles)
	m.HandleFunc("POST /api/jobs/{kind}/{id}/files/prio", a.handleFilePrio)
	m.HandleFunc("GET /api/files/{kind}/{id}/{path...}", a.handleFileDownload)
	m.HandleFunc("POST /api/jobs/{kind}/{id}/reveal", a.handleReveal)
	m.HandleFunc("POST /api/jobs/{kind}/{id}/open", a.handleOpenFile)
	m.HandleFunc("GET /api/jobs/torrent/{id}/torrent", a.handleTorrentFile)
	m.HandleFunc("GET /api/jobs/torrent/{id}/magnet", a.handleMagnet)
	m.HandleFunc("POST /api/jobs/torrent/{id}/trackers", a.handleAddTrackers)
	m.HandleFunc("POST /api/jobs/torrent/{id}/recheck", a.handleRecheck)
	m.HandleFunc("POST /api/jobs/torrent/{id}/rename", a.handleRename)
	m.HandleFunc("POST /api/jobs/torrent/{id}/maxconns", a.handleMaxConns)
	m.HandleFunc("GET /api/jobs/torrent/{id}/details", a.handleDetails)
	m.HandleFunc("GET /api/servers", a.handleServersGet)
	m.HandleFunc("POST /api/servers", a.handleServersSet)
	m.HandleFunc("POST /api/servers/{id}/test", a.handleServerTest)
	m.HandleFunc("DELETE /api/servers/{id}", a.handleServerDelete)
	m.HandleFunc("GET /api/update", a.handleUpdateGet)
	m.HandleFunc("POST /api/update/check", a.handleUpdateCheck)
	m.HandleFunc("POST /api/update/apply", a.handleUpdateApply)
	m.HandleFunc("GET /api/settings", a.handleSettingsGet)
	m.HandleFunc("POST /api/settings", a.handleSettingsSet)
	m.HandleFunc("/", a.handleStatic)
}

type publicServer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         bool   `json:"tls"`
	Username    string `json:"username,omitempty"`
	HasPass     bool   `json:"hasPass"`
	Connections int    `json:"connections"`
}

func publicServers(cfg []NNTPServer) []publicServer {
	var out []publicServer
	for _, s := range cfg {
		out = append(out, publicServer{
			ID: s.ID, Name: s.Name, Host: s.Host, Port: s.Port,
			TLS: s.TLS, Username: s.Username, HasPass: s.Password != "",
			Connections: s.Connections,
		})
	}
	return out
}

type publicConfig struct {
	ListenAddr        string `json:"listenAddr"`
	DownloadDir       string `json:"downloadDir"`
	BTListenPort      int    `json:"btListenPort"`
	EnableDHT         bool   `json:"enableDHT"`
	EnableUPnP        bool   `json:"enableUpnp"`
	EnablePEX         bool   `json:"enablePex"`
	DownloadLimit     int64  `json:"downloadLimit"`
	UploadLimit       int64  `json:"uploadLimit"`
	NZBMaxConnections int    `json:"nzbMaxConnections"`
	HasToken          bool   `json:"hasToken"`
	DisableWebUI      bool   `json:"disableWebUI"`
	WatchDir          string `json:"watchDir,omitempty"`
}

func (a *apiServer) pubConfig() publicConfig {
	c := a.app.configSnapshot()
	return publicConfig{
		ListenAddr: c.ListenAddr, DownloadDir: c.DownloadDir,
		BTListenPort: c.BTListenPort, EnableDHT: c.EnableDHT,
		EnableUPnP: c.EnableUPnP, EnablePEX: c.EnablePEX,
		DownloadLimit: c.DownloadLimit, UploadLimit: c.UploadLimit,
		NZBMaxConnections: c.NZBMaxConnections,
		HasToken:          c.UIToken != "",
		DisableWebUI:      c.DisableWebUI,
		WatchDir:          c.WatchDir,
	}
}

type snapshot struct {
	Version  string         `json:"version"`
	Update   updateInfo     `json:"update"`
	Global   GlobalStats    `json:"global"`
	Config   publicConfig   `json:"config"`
	Servers  []publicServer `json:"servers"`
	Torrents []TorrentJob   `json:"torrents"`
	NZB      []*NZBJob      `json:"nzb"`
	Time     time.Time      `json:"time"`
}

func (a *apiServer) buildSnapshot() snapshot {
	tjobs := a.app.Torrent.List()
	gs := a.app.Torrent.GlobalStats()
	gs.NZBJobs = a.app.NZB.ActiveCount()
	gs.DownloadSpeed += a.app.NZB.GlobalDownSpeed()
	return snapshot{
		Version:  a.app.version,
		Update:   *a.app.updateStatus(),
		Global:   gs,
		Config:   a.pubConfig(),
		Servers:  publicServers(a.app.servers()),
		Torrents: tjobs,
		NZB:      a.app.NZB.List(),
		Time:     time.Now(),
	}
}

func (a *apiServer) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSONResp(w, a.buildSnapshot())
}

func (a *apiServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	var last string
	for {
		b, _ := json.Marshal(a.buildSnapshot())
		if string(b) != last {
			last = string(b)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type addRequest struct {
	URI        string `json:"uri"`
	TorrentB64 string `json:"torrentB64"`
	NZBB64     string `json:"nzbB64"`
	Name       string `json:"name"`
	ServerID   string `json:"serverID"`
	Type       string `json:"type"`
}

func (a *apiServer) handleAdd(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		a.handleAddMultipart(w, r)
		return
	}
	var req addRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	id, kind, err := a.dispatchAdd(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"id": id, "kind": kind})
}

func (a *apiServer) handleAddMultipart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart: "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file field")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read: "+err.Error())
		return
	}
	name := hdr.Filename
	serverID := r.FormValue("serverID")
	req := addRequest{
		Name:     strings.TrimSuffix(name, filepath.Ext(name)),
		ServerID: serverID,
	}
	lower := strings.ToLower(name)
	sniff := sniffKind(data)
	switch {
	case strings.HasSuffix(lower, ".torrent") || sniff == "torrent":
		req.Type = "torrent"
		req.TorrentB64 = base64.StdEncoding.EncodeToString(data)
	case strings.HasSuffix(lower, ".nzb") || sniff == "nzb":
		req.Type = "nzb"
		req.NZBB64 = base64.StdEncoding.EncodeToString(data)
	default:
		writeErr(w, http.StatusBadRequest, "unrecognized file type (want .torrent or .nzb)")
		return
	}
	id, kind, err := a.dispatchAdd(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"id": id, "kind": kind})
}

func sniffKind(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	trim := strings.TrimSpace(string(data[:minInt(len(data), 512)]))
	if strings.HasPrefix(trim, "magnet:") {
		return "torrent"
	}
	if strings.HasPrefix(trim, "d") || strings.HasPrefix(trim, "l") {
		return "torrent"
	}
	if strings.HasPrefix(trim, "<") {
		return "nzb"
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *apiServer) dispatchAdd(req addRequest) (id, kind string, err error) {
	ctx := context.Background()
	if req.Type == "nzb" || req.NZBB64 != "" {
		var data []byte
		if req.NZBB64 != "" {
			data, err = base64.StdEncoding.DecodeString(req.NZBB64)
			if err != nil {
				return "", "", fmt.Errorf("bad nzb data: %w", err)
			}
		} else if req.URI != "" {
			id, err = a.app.NZB.FetchAndAdd(ctx, req.URI, req.ServerID)
			return id, "nzb", err
		} else {
			return "", "", errors.New("nzb: nothing to add")
		}
		id, err = a.app.NZB.ParseAndAdd(data, req.Name, req.ServerID)
		return id, "nzb", err
	}
	if req.Type == "torrent" || req.TorrentB64 != "" {
		if req.TorrentB64 != "" {
			data, derr := base64.StdEncoding.DecodeString(req.TorrentB64)
			if derr != nil {
				return "", "", fmt.Errorf("bad torrent data: %w", derr)
			}
			id, err := a.app.Torrent.AddTorrentBytes(data)
			return id, "torrent", err
		}
	}
	if req.URI != "" {
		u := req.URI
		if strings.HasPrefix(u, "magnet:") {
			id, err := a.app.Torrent.AddMagnetURI(u)
			return id, "torrent", err
		}
		if strings.Contains(strings.ToLower(u), ".nzb") {
			id, err = a.app.NZB.FetchAndAdd(ctx, u, req.ServerID)
			return id, "nzb", err
		}
		return a.fetchAndRoute(ctx, u, req)
	}
	return "", "", errors.New("nothing to add")
}

func (a *apiServer) fetchAndRoute(ctx context.Context, url string, req addRequest) (string, string, error) {
	hreq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := httpClient.Do(hreq)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", "", err
	}
	kind := sniffKind(data)
	if kind == "" {
		low := strings.ToLower(resp.Request.URL.Path)
		if strings.HasSuffix(low, ".torrent") {
			kind = "torrent"
		} else if strings.HasSuffix(low, ".nzb") {
			kind = "nzb"
		}
	}
	name := req.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(resp.Request.URL.Path), filepath.Ext(resp.Request.URL.Path))
	}
	switch kind {
	case "nzb":
		id, err := a.app.NZB.ParseAndAdd(data, name, req.ServerID)
		return id, "nzb", err
	case "torrent":
		id, err := a.app.Torrent.AddTorrentBytes(data)
		return id, "torrent", err
	default:
		return "", "", errors.New("could not determine file type")
	}
}
