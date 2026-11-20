package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- job actions ----------

func (a *apiServer) handleJobAction(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	id := r.PathValue("id")
	action := r.PathValue("action")

	var deleteFiles bool
	var body struct {
		DeleteFiles bool `json:"deleteFiles"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	deleteFiles = body.DeleteFiles

	var err error
	switch kind {
	case "torrent":
		err = a.torrentAction(id, action, deleteFiles)
	case "nzb":
		err = a.nzbAction(id, action, deleteFiles)
	default:
		err = errors.New("unknown kind")
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

func (a *apiServer) torrentAction(id, action string, del bool) error {
	switch action {
	case "pause":
		return a.app.Torrent.Pause(id)
	case "resume":
		return a.app.Torrent.Resume(id)
	case "remove":
		return a.app.Torrent.Remove(id, del)
	}
	return errors.New("unknown action")
}

func (a *apiServer) nzbAction(id, action string, del bool) error {
	switch action {
	case "pause":
		return a.app.NZB.Pause(id)
	case "resume":
		return a.app.NZB.Resume(id)
	case "remove":
		return a.app.NZB.Remove(id, del)
	}
	return errors.New("unknown action")
}

// ---------- files ----------

func (a *apiServer) handleJobFiles(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	id := r.PathValue("id")
	var files []map[string]any
	var err error
	switch kind {
	case "torrent":
		files, err = a.app.Torrent.Files(id)
	case "nzb":
		files, err = a.app.NZB.Files(id)
	default:
		err = errors.New("unknown kind")
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"files": files})
}

func (a *apiServer) handleFilePrio(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("kind") != "torrent" {
		writeErr(w, http.StatusBadRequest, "priority only for torrents")
		return
	}
	var req struct {
		Path     string `json:"path"`
		Priority int    `json:"priority"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.app.Torrent.SetFilePriority(r.PathValue("id"), req.Path, req.Priority); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// handleFileDownload streams a completed file to the client.
func (a *apiServer) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	id := r.PathValue("id")
	rel := strings.TrimPrefix(r.PathValue("path"), "/")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "no path")
		return
	}
	var diskPath string
	var ok bool
	switch kind {
	case "torrent":
		diskPath, ok = a.app.Torrent.FileDiskPath(id, rel)
	case "nzb":
		diskPath, ok = a.app.NZB.FileDiskPath(id, rel)
	default:
		writeErr(w, http.StatusBadRequest, "unknown kind")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "file not available")
		return
	}
	abs, err := filepath.Abs(diskPath)
	if err != nil || !strings.HasPrefix(abs, filepath.Clean(a.app.downloadDir())+string(os.PathSeparator)) {
		writeErr(w, http.StatusForbidden, "bad path")
		return
	}
	// For torrents only serve fully-downloaded files.
	if kind == "torrent" {
		files, ferr := a.app.Torrent.Files(id)
		if ferr == nil {
			complete := false
			for _, f := range files {
				if f["path"] == rel && f["size"] == f["done"] {
					complete = true
					break
				}
			}
			if !complete {
				writeErr(w, http.StatusConflict, "file not fully downloaded")
				return
			}
		}
	}
	if _, err := os.Stat(abs); err != nil {
		writeErr(w, http.StatusNotFound, "file not on disk yet")
		return
	}
	http.ServeFile(w, r, abs)
}

// ---------- servers ----------

func (a *apiServer) handleServersGet(w http.ResponseWriter, r *http.Request) {
	writeJSONResp(w, map[string]any{"servers": publicServers(a.app.servers())})
}

func (a *apiServer) handleServersSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servers []NNTPServer `json:"servers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	a.app.lockConfig()
	defer a.app.unlockConfig()
	old := map[string]NNTPServer{}
	for _, s := range a.app.Config.NNTPServers {
		old[s.ID] = s
	}
	for i := range req.Servers {
		s := &req.Servers[i]
		if s.ID == "" {
			s.ID = a.app.NZB.ids.next("srv")
		}
		if s.Port == 0 {
			if s.TLS {
				s.Port = 563
			} else {
				s.Port = 119
			}
		}
		if s.Connections <= 0 {
			s.Connections = a.app.Config.NZBMaxConnections
		}
		if s.Password == "" {
			if o, ok := old[s.ID]; ok {
				s.Password = o.Password
			}
		}
	}
	a.app.Config.NNTPServers = req.Servers
	if err := a.app.saveConfig(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.app.NZB.ResetPools()
	writeJSONResp(w, map[string]any{"ok": true})
}

// POST /api/servers/{id}/test — dial + greeting + auth roundtrip; reports
// latency so users can verify credentials/connectivity from the UI.
func (a *apiServer) handleServerTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var srv *NNTPServer
	for _, s := range a.app.servers() {
		if s.ID == id {
			cp := s
			srv = &cp
			break
		}
	}
	if srv == nil {
		writeErr(w, 404, "server not found")
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := dialNNTP(ctx, *srv)
	if err != nil {
		writeJSONResp(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = c.conn.Close()
	writeJSONResp(w, map[string]any{"ok": true, "ms": time.Since(start).Milliseconds()})
}

func (a *apiServer) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.app.lockConfig()
	defer a.app.unlockConfig()
	var out []NNTPServer
	for _, s := range a.app.Config.NNTPServers {
		if s.ID != id {
			out = append(out, s)
		}
	}
	a.app.Config.NNTPServers = out
	if err := a.app.saveConfig(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.app.NZB.ResetPools()
	writeJSONResp(w, map[string]any{"ok": true})
}

// ---------- settings ----------

func (a *apiServer) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSONResp(w, a.pubConfig())
}

func (a *apiServer) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ListenAddr         *string  `json:"listenAddr"`
		DownloadDir        *string  `json:"downloadDir"`
		BTListenPort       *int     `json:"btListenPort"`
		EnableDHT          *bool    `json:"enableDHT"`
		EnableUPnP         *bool    `json:"enableUpnp"`
		EnablePEX          *bool    `json:"enablePex"`
		DownloadLimit      *int64   `json:"downloadLimit"`
		UploadLimit        *int64   `json:"uploadLimit"`
		NZBMaxConnections  *int     `json:"nzbMaxConnections"`
		NZBMissingAbortPct *float64 `json:"nzbMissingAbortPct"`
		UIToken            *string  `json:"uiToken"`
		DisableWebUI       *bool    `json:"disableWebUI"`
		WatchDir           *string  `json:"watchDir"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	a.app.lockConfig()
	defer a.app.unlockConfig()
	cfg := &a.app.Config
	restart := []string{}
	setStr := func(dst *string, v *string, tag string) {
		if v != nil {
			*dst = *v
			restart = append(restart, tag)
		}
	}
	setStr(&cfg.ListenAddr, req.ListenAddr, "listenAddr")
	setStr(&cfg.DownloadDir, req.DownloadDir, "downloadDir")
	if req.BTListenPort != nil {
		cfg.BTListenPort = *req.BTListenPort
		restart = append(restart, "btListenPort")
	}
	if req.EnableDHT != nil {
		cfg.EnableDHT = *req.EnableDHT
		restart = append(restart, "enableDHT")
	}
	if req.EnableUPnP != nil {
		cfg.EnableUPnP = *req.EnableUPnP
		restart = append(restart, "enableUpnp")
	}
	if req.EnablePEX != nil {
		cfg.EnablePEX = *req.EnablePEX
		restart = append(restart, "enablePex")
	}
	if req.NZBMaxConnections != nil {
		cfg.NZBMaxConnections = *req.NZBMaxConnections
	}
	if req.NZBMissingAbortPct != nil {
		cfg.NZBMissingAbortPct = *req.NZBMissingAbortPct
	}
	if req.UIToken != nil {
		cfg.UIToken = *req.UIToken
	}
	if req.DisableWebUI != nil {
		cfg.DisableWebUI = *req.DisableWebUI
		restart = append(restart, "disableWebUI")
	}
	if req.WatchDir != nil {
		cfg.WatchDir = *req.WatchDir
	}
	if req.DownloadLimit != nil || req.UploadLimit != nil {
		if req.DownloadLimit != nil {
			cfg.DownloadLimit = *req.DownloadLimit
		}
		if req.UploadLimit != nil {
			cfg.UploadLimit = *req.UploadLimit
		}
		a.app.Torrent.SetRateLimits(cfg.DownloadLimit, cfg.UploadLimit)
	}
	if err := a.app.saveConfig(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{"ok": true}
	if len(restart) > 0 {
		resp["restartRequired"] = restart
	}
	writeJSONResp(w, resp)
}

// ---------- static ----------

func (a *apiServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	serveWebUI(w, r)
}

// ---------- helpers ----------

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
