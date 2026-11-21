package main

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/anacrolix/torrent"
)

// TorrentJob is the API representation of one torrent.
type TorrentJob struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // "torrent"
	Name        string    `json:"name"`
	Hash        string    `json:"hash"`
	Size        int64     `json:"size"`
	Done        int64     `json:"done"`
	Progress    float64   `json:"progress"`
	DownSpeed   int64     `json:"downSpeed"`
	UpSpeed     int64     `json:"upSpeed"`
	Peers       int       `json:"peers"`
	Seeds       int       `json:"seeds"`
	Status      string    `json:"status"` // downloading|seeding|paused|metadata|done|error
	ETA         int64     `json:"eta"`    // seconds, -1 unknown
	Dir         string    `json:"dir"`
	AddedAt     time.Time `json:"addedAt"`
	Error       string    `json:"error,omitempty"`
	Uploaded    int64     `json:"uploaded"`
	Ratio       float64   `json:"ratio"`
	Category    string    `json:"category,omitempty"`
	NumFiles    int       `json:"numFiles"`
	PieceStates string    `json:"pieceStates,omitempty"` // compact run string
}

func (e *TorrentEngine) List() []TorrentJob {
	out := []TorrentJob{}
	for _, t := range e.client.Torrents() {
		out = append(out, e.jobFor(t))
	}
	// Persisted entries whose client torrent isn't up yet (e.g. still restoring).
	e.mu.Lock()
	known := map[string]bool{}
	for _, t := range e.client.Torrents() {
		known[t.InfoHash().HexString()] = true
	}
	pending := []TorrentJob{}
	for _, ih := range e.order {
		if known[ih] {
			continue
		}
		if st := e.states[ih]; st != nil {
			pending = append(pending, TorrentJob{
				ID: ih, Kind: "torrent", Name: st.Name, Hash: ih,
				Status: "metadata", Dir: st.Dir, AddedAt: st.AddedAt,
			})
		}
	}
	e.mu.Unlock()
	out = append(out, pending...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

func (e *TorrentEngine) jobFor(t *torrent.Torrent) TorrentJob {
	ih := t.InfoHash().HexString()
	st := e.stateFor(ih)

	name := ""
	if st != nil && st.CustomName != "" {
		name = st.CustomName
	}
	if name == "" {
		name = t.Name()
	}
	if name == "" && st != nil {
		name = st.Name
	}
	if name == "" {
		name = ih
	}

	size := t.Length()
	done := t.BytesCompleted()
	var progress float64
	if size > 0 {
		progress = float64(done) / float64(size)
		if progress > 1 {
			progress = 1
		}
	}

	stats := t.Stats()
	cs := stats.AllConnStats.ConnStats
	down, up := e.ratesFor(ih, cs.BytesReadData.Int64(), cs.BytesWrittenData.Int64())
	uploaded := cs.BytesWrittenData.Int64()
	ratio := 0.0
	if size > 0 {
		ratio = float64(uploaded) / float64(size)
	}

	status := "downloading"
	switch {
	case st != nil && st.Paused:
		status = "paused"
	case t.Info() == nil:
		status = "metadata"
	case size > 0 && done >= size:
		if t.Seeding() {
			status = "seeding"
		} else {
			status = "done"
		}
	}

	eta := int64(-1)
	if status == "downloading" && down > 0 && size > done {
		eta = (size - done) / down
	}

	dir := e.DirFor(ih)
	if dir == "" && st != nil {
		dir = st.Dir
	}

	added := time.Now()
	if st != nil && !st.AddedAt.IsZero() {
		added = st.AddedAt
	}

	numFiles := 0
	if t.Info() != nil {
		numFiles = len(t.Files())
	}

	return TorrentJob{
		ID: ih, Kind: "torrent", Name: name, Hash: ih,
		Size: size, Done: done, Progress: progress,
		DownSpeed: down, UpSpeed: up,
		Peers: stats.TotalPeers, Seeds: stats.ConnectedSeeders,
		Status: status, ETA: eta,
		Dir: dir, AddedAt: added,
		Uploaded: uploaded, Ratio: ratio,
		Category: stCategory(st),
		NumFiles: numFiles,
	}
}

func (e *TorrentEngine) stateFor(ih string) *torrentStateEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.states[ih]
}

func (e *TorrentEngine) ratesFor(ih string, read, written int64) (down, up int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	prev := e.rates[ih]
	if prev == nil {
		e.rates[ih] = &rateSample{read: read, written: written, ts: now}
		return 0, 0
	}
	if dt := now.Sub(prev.ts).Seconds(); dt > 0 {
		down = int64(float64(read-prev.read) / dt)
		up = int64(float64(written-prev.written) / dt)
	}
	e.rates[ih] = &rateSample{read: read, written: written, ts: now}
	e.lastRates[ih] = lastRate{down: max(down, 0), up: max(up, 0)}
	return max(down, 0), max(up, 0)
}

// GlobalStats aggregates across torrents.
type GlobalStats struct {
	DownloadSpeed int64 `json:"downloadSpeed"`
	UploadSpeed   int64 `json:"uploadSpeed"`
	Peers         int   `json:"peers"`
	Torrents      int   `json:"torrents"`
	NZBJobs       int   `json:"nzbJobs"`
}

func (e *TorrentEngine) GlobalStats() GlobalStats {
	var g GlobalStats
	g.Torrents = len(e.client.Torrents())
	// Note: lastRates is refreshed by jobFor during List(); call List first
	// for fresh numbers (the API snapshot does exactly that).
	for _, t := range e.client.Torrents() {
		s := t.Stats()
		g.Peers += s.TotalPeers
		lr := e.lastRateFor(t.InfoHash().HexString())
		g.DownloadSpeed += lr.down
		g.UploadSpeed += lr.up
	}
	return g
}

func (e *TorrentEngine) lastRateFor(ih string) lastRate {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRates[ih]
}

func (e *TorrentEngine) Files(ih string) ([]map[string]any, error) {
	t, _, err := e.find(ih)
	if err != nil {
		return nil, err
	}
	if t.Info() == nil {
		return nil, errors.New("metadata not available yet")
	}
	out := []map[string]any{}
	for _, f := range t.Files() {
		out = append(out, map[string]any{
			"path":     f.Path(),
			"display":  f.DisplayPath(),
			"size":     f.Length(),
			"done":     f.BytesCompleted(),
			"priority": int(f.Priority()),
			"pieces":   f.EndPieceIndex() - f.BeginPieceIndex(),
		})
	}
	return out, nil
}

// FileDiskPath returns the absolute on-disk path for a torrent file.
func (e *TorrentEngine) FileDiskPath(ih, filePath string) (string, bool) {
	e.mu.Lock()
	dir, ok := e.dirs[ih]
	e.mu.Unlock()
	if !ok || dir == "" {
		return "", false
	}
	return filepath.Join(dir, filepath.FromSlash(filePath)), true
}

func (e *TorrentEngine) DirFor(ih string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dirs[ih]
}

// stCategory returns the stored category for a torrent state entry (nil-safe).
func stCategory(st *torrentStateEntry) string {
	if st == nil {
		return ""
	}
	return st.Category
}
