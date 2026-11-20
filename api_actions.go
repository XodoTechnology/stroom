package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent"
)

// resolveJobDir returns the on-disk dir for reveal, per kind.
func (a *apiServer) jobDir(kind, id string) (string, error) {
	switch kind {
	case "torrent":
		return a.app.Torrent.RevealDir(id)
	case "nzb":
		if d, ok := a.app.NZB.JobDir(id); ok {
			return d, nil
		}
		return "", errNotFound
	}
	return "", errNotFound
}

// POST /api/jobs/{kind}/{id}/reveal — open containing folder on the server.
func (a *apiServer) handleReveal(w http.ResponseWriter, r *http.Request) {
	if !a.app.GUI {
		writeErr(w, http.StatusForbidden, "desktop actions disabled in headless mode")
		return
	}
	dir, err := a.jobDir(r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	openInFileManager(dir)
	writeJSONResp(w, map[string]any{"ok": true, "dir": dir})
}

// POST /api/jobs/{kind}/{id}/open {path} — open a file with the OS default app.
func (a *apiServer) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if !a.app.GUI {
		writeErr(w, http.StatusForbidden, "desktop actions disabled in headless mode")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "need {path}")
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	var disk string
	var ok bool
	if kind == "torrent" {
		disk, ok = a.app.Torrent.OpenFilePath(id, req.Path)
	} else if kind == "nzb" {
		disk, ok = a.app.NZB.FileDiskPath(id, req.Path)
	} else {
		writeErr(w, http.StatusBadRequest, "unknown kind")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "file not available")
		return
	}
	abs, err := filepath.Abs(disk)
	clean := filepath.Clean(a.app.downloadDir())
	if err != nil || !strings.HasPrefix(abs, clean+string(filepath.Separator)) {
		writeErr(w, http.StatusForbidden, "bad path")
		return
	}
	// Never execute a downloaded file via the OS handler — only select it in
	// the containing folder so untrusted payloads cannot run on the host.
	revealInFileManager(abs)
	writeJSONResp(w, map[string]any{"ok": true})
}

// GET /api/jobs/torrent/{id}/torrent — download the .torrent file.
func (a *apiServer) handleTorrentFile(w http.ResponseWriter, r *http.Request) {
	raw, name, ok := a.app.Torrent.TorrentFile(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no .torrent data (magnet-only job)")
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	_, _ = w.Write(raw)
}

// GET /api/jobs/torrent/{id}/magnet — plain-text magnet URI.
func (a *apiServer) handleMagnet(w http.ResponseWriter, r *http.Request) {
	m, err := a.app.Torrent.MagnetURI(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(m))
}

// POST /api/jobs/torrent/{id}/trackers {trackers: ["http://…"]}
func (a *apiServer) handleAddTrackers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Trackers []string `json:"trackers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.app.Torrent.AddTrackers(r.PathValue("id"), req.Trackers); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// POST /api/jobs/torrent/{id}/recheck — force piece re-hash.
func (a *apiServer) handleRecheck(w http.ResponseWriter, r *http.Request) {
	if err := a.app.Torrent.Recheck(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// POST /api/jobs/torrent/{id}/rename {name}
func (a *apiServer) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.app.Torrent.Rename(r.PathValue("id"), req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// POST /api/jobs/torrent/{id}/maxconns {n}
func (a *apiServer) handleMaxConns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.app.Torrent.SetMaxConns(r.PathValue("id"), req.N); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// GET /api/jobs/torrent/{id}/details — extra info for the details dialog.
func (a *apiServer) handleDetails(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, st, err := a.app.Torrent.find(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	stats := t.Stats()
	swarm := t.KnownSwarm()
	info := map[string]any{
		"hash":     id,
		"name":     t.Name(),
		"size":     t.Length(),
		"dir":      a.app.Torrent.DirFor(id),
		"trackers": a.app.Torrent.TrackerList(id),
		"maxConns": 0,
		"pieces":   t.NumPieces(),
		"complete": t.Complete(),
		"seeding":  t.Seeding(),
		"stats": map[string]any{
			"totalPeers":     stats.TotalPeers,
			"activePeers":    stats.ActivePeers,
			"pendingPeers":   stats.PendingPeers,
			"connectedSeeds": stats.ConnectedSeeders,
			"halfOpen":       stats.HalfOpenPeers,
			"piecesComplete": stats.PiecesComplete,
		},
		"peers":     peerList(swarm),
		"peerCount": len(swarm),
	}
	if st != nil {
		info["customName"] = st.CustomName
		info["maxConns"] = st.MaxConns
		info["paused"] = st.Paused
		info["addedAt"] = st.AddedAt
	}
	writeJSONResp(w, info)
}

// peerList returns a deduplicated, capped list of known swarm peers.
func peerList(swarm []torrent.PeerInfo) []map[string]any {
	const maxPeers = 1000
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, p := range swarm {
		addr := p.Addr.String()
		pid := hex.EncodeToString(p.Id[:])
		key := addr + "|" + pid
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{
			"addr": addr,
			"id":   pid,
			"src":  p.Source,
			"enc":  p.SupportsEncryption,
		})
		if len(out) >= maxPeers {
			break
		}
	}
	return out
}
