package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Minimal qBittorrent WebAPI v2 shim at /api/v2/ so *arr tools can point at
// Stroom as a torrent client. Covers: auth/login, app/version,
// app/preferences, torrents/info|add|pause|resume|delete|setCategory.
// Auth uses the UIToken as the qB password (any username).

func (a *apiServer) handleQB(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	write := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		switch t := v.(type) {
		case string:
			io.WriteString(w, t)
		default:
			_ = json.NewEncoder(w).Encode(t)
		}
	}

	switch path {
	case "auth/login":
		tok := a.app.uiToken()
		if tok == "" || r.FormValue("password") == tok {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "stroom", Path: "/"})
			io.WriteString(w, "Ok.")
		} else {
			io.WriteString(w, "Fails.")
		}
	case "app/version":
		io.WriteString(w, "v5.0.0")
	case "app/preferences":
		write(200, map[string]any{
			"save_path":         a.app.downloadDir(),
			"dl_limit":          a.app.configSnapshot().DownloadLimit,
			"up_limit":          a.app.configSnapshot().UploadLimit,
			"max_ratio_enabled": a.app.configSnapshot().SeedRatioLimit > 0,
			"max_ratio":         a.app.configSnapshot().SeedRatioLimit,
		})
	case "torrents/info":
		var out []any
		for _, j := range a.app.Torrent.List() {
			out = append(out, map[string]any{
				"hash":       j.Hash,
				"name":       j.Name,
				"size":       j.Size,
				"progress":   j.Progress,
				"dlspeed":    j.DownSpeed,
				"upspeed":    j.UpSpeed,
				"num_seeds":  j.Seeds,
				"num_leechs": j.Peers,
				"ratio":      j.Ratio,
				"uploaded":   j.Uploaded,
				"category":   j.Category,
				"save_path":  j.Dir,
				"state":      qbState(j.Status),
				"added_on":   j.AddedAt.Unix(),
			})
		}
		write(200, out)
	case "torrents/add":
		cat := r.FormValue("category")
		urls := r.FormValue("urls")
		if urls == "" {
			urls = r.FormValue("url")
		}
		if f, _, err := r.FormFile("torrents"); err == nil {
			data, err := io.ReadAll(io.LimitReader(f, 64<<20))
			f.Close()
			if err == nil {
				_, _ = a.app.Torrent.AddTorrent(AddInput{
					TorrentB64: base64.StdEncoding.EncodeToString(data), Category: cat})
			}
		}
		for _, u := range strings.Fields(urls) {
			if strings.HasPrefix(u, "magnet:") {
				_, _ = a.app.Torrent.AddTorrent(AddInput{Magnet: u, Category: cat})
			} else if strings.HasPrefix(u, "http") {
				_, _ = a.app.Torrent.AddTorrentURL(r.Context(), u)
			}
		}
		io.WriteString(w, "Ok.")
	case "torrents/pause", "torrents/stop":
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			_ = a.app.Torrent.Pause(h)
		}
		io.WriteString(w, "Ok.")
	case "torrents/resume", "torrents/start":
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			_ = a.app.Torrent.Resume(h)
		}
		io.WriteString(w, "Ok.")
	case "torrents/delete":
		del := r.FormValue("deleteFiles") == "true"
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			_ = a.app.Torrent.Remove(h, del)
		}
		io.WriteString(w, "Ok.")
	default:
		write(404, map[string]any{"error": "not implemented: " + path})
	}
}

func qbState(s string) string {
	switch s {
	case "downloading":
		return "downloading"
	case "seeding":
		return "uploading"
	case "paused":
		return "pausedDL"
	case "metadata":
		return "metaDL"
	case "done":
		return "pausedUP"
	case "error":
		return "error"
	default:
		return "unknown"
	}
}
