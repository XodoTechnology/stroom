package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

// torrentStateEntry is what we persist per torrent in state.json.
type torrentStateEntry struct {
	IH         string         `json:"ih"`
	IH2        string         `json:"ih2,omitempty"`
	Magnet     string         `json:"magnet,omitempty"`
	TorrentB64 string         `json:"torrent,omitempty"` // base64 bencoded .torrent
	Name       string         `json:"name,omitempty"`
	Trackers   [][]string     `json:"trackers,omitempty"`
	Webseeds   []string       `json:"webseeds,omitempty"`
	Sources    []string       `json:"sources,omitempty"`
	DhtNodes   []string       `json:"dht,omitempty"`
	Paused     bool           `json:"paused"`
	CustomName string         `json:"customname,omitempty"`
	MaxConns   int            `json:"maxconns,omitempty"`
	Dir        string         `json:"dir"`
	FilePrio   map[string]int `json:"fileprio,omitempty"`
	AddedAt    time.Time      `json:"addedAt"`
}

type rateSample struct {
	read, written int64
	ts            time.Time
}

// TorrentEngine wraps torrent.Client with persistence and an API-friendly facade.
type TorrentEngine struct {
	app       *App
	client    *torrent.Client
	storage   storage.ClientImplCloser
	mu        sync.Mutex
	states    map[string]*torrentStateEntry // by infohash hex
	order     []string                      // infohash hexes in add order
	dirs      map[string]string             // infohash hex -> absolute torrent dir
	rates     map[string]*rateSample
	lastRates map[string]lastRate
	ids       idGen
	persistMu sync.Mutex
	persistT  *time.Timer
	dlLimiter *rate.Limiter
	ulLimiter *rate.Limiter
	dlLim     int64
	ulLim     int64
}

type lastRate struct{ down, up int64 }

func NewTorrentEngine(app *App) (*TorrentEngine, error) {
	e := &TorrentEngine{
		app:       app,
		states:    make(map[string]*torrentStateEntry),
		dirs:      make(map[string]string),
		rates:     make(map[string]*rateSample),
		lastRates: make(map[string]lastRate),
	}

	base := filepath.Join(app.Config.DownloadDir, "torrents")
	if err := ensureDir(base); err != nil {
		return nil, err
	}
	pc, err := storage.NewBoltPieceCompletion(base)
	if err != nil {
		pc = storage.NewMapPieceCompletion()
	}

	claimed := make(map[string]string) // dirName -> infohash hex
	var claimMu sync.Mutex
	e.storage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   base,
		PieceCompletion: pc,
		UsePartFiles:    g.Some(true),
		TorrentDirMaker: func(baseDir string, info *metainfo.Info, ih metainfo.Hash) string {
			name := ""
			if info != nil {
				name = info.BestName()
			}
			if name == "" || name == metainfo.NoName {
				name = ih.HexString()
			}
			dirName := sanitizeName(name)
			claimMu.Lock()
			if other, ok := claimed[dirName]; ok && other != ih.HexString() {
				dirName = fmt.Sprintf("%s-%s", dirName, ih.HexString()[:8])
				if o2, ok2 := claimed[dirName]; ok2 && o2 != ih.HexString() {
					dirName = fmt.Sprintf("%s-%d", dirName, time.Now().UnixNano()%100000)
				}
			}
			claimed[dirName] = ih.HexString()
			claimMu.Unlock()
			e.mu.Lock()
			e.dirs[ih.HexString()] = filepath.Join(baseDir, dirName)
			e.mu.Unlock()
			return filepath.Join(baseDir, dirName)
		},
	})

	cfg := torrent.NewDefaultClientConfig()
	cfg.ClientDhtConfig.NoDHT = !app.Config.EnableDHT
	cfg.DataDir = base
	cfg.DefaultStorage = e.storage
	cfg.ListenPort = app.Config.BTListenPort
	cfg.DisablePEX = !app.Config.EnablePEX
	cfg.NoDefaultPortForwarding = !app.Config.EnableUPnP
	cfg.HTTPUserAgent = "Stroom/" + app.version
	cfg.EstablishedConnsPerTorrent = 60
	cfg.HalfOpenConnsPerTorrent = 40
	cfg.TotalHalfOpenConns = 100
	cfg.TorrentPeersLowWater = 30
	cfg.TorrentPeersHighWater = 200
	cfg.Seed = true // keep completed torrents seeding + accepting peers
	if os.Getenv("STROOM_DISABLE_UTP") != "" {
		cfg.DisableUTP = true
	}
	if os.Getenv("STROOM_DEBUG") != "" {
		cfg.Debug = true
		cfg.Slogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	// Unique peer ID per client instance (avoids self-connection drops).
	var pid [20]byte
	if _, err := rand.Read(pid[:]); err == nil {
		pid[0] = '-'
		pid[1] = 'S'
		pid[2] = 'T'
		cfg.PeerID = string(pid[:])
	}
	// The client requires non-nil limiters; rate.Inf = unlimited.
	dlLim := rate.Inf
	if app.Config.DownloadLimit > 0 {
		dlLim = rate.Limit(app.Config.DownloadLimit)
	}
	ulLim := rate.Inf
	if app.Config.UploadLimit > 0 {
		ulLim = rate.Limit(app.Config.UploadLimit)
	}
	cfg.DownloadRateLimiter = rate.NewLimiter(dlLim, 4<<20)
	cfg.UploadRateLimiter = rate.NewLimiter(ulLim, 4<<20)
	e.dlLimiter = cfg.DownloadRateLimiter
	e.ulLimiter = cfg.UploadRateLimiter
	e.dlLim = app.Config.DownloadLimit
	e.ulLim = app.Config.UploadLimit

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("torrent client: %w", err)
	}
	e.client = cl
	return e, nil
}

func (e *TorrentEngine) Close() {
	e.Save()
	if e.client != nil {
		e.client.Close()
	}
	if e.storage != nil {
		e.storage.Close()
	}
}

// ---------- persistence ----------

func (e *TorrentEngine) statePath() string {
	return filepath.Join(e.app.DataDir, "state.json")
}

type torrentOnlyState struct {
	Torrents []torrentStateEntry `json:"torrents"`
}

func (e *TorrentEngine) Save() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ts := torrentOnlyState{}
	for _, ih := range e.order {
		if s, ok := e.states[ih]; ok {
			ts.Torrents = append(ts.Torrents, *s)
		}
	}
	return writeJSON(e.statePath(), ts)
}

func (e *TorrentEngine) Load() error {
	var ts torrentOnlyState
	if err := readJSON(e.statePath(), &ts); err != nil {
		return err
	}
	for i := range ts.Torrents {
		s := ts.Torrents[i]
		e.mu.Lock()
		e.states[s.IH] = &s
		e.order = append(e.order, s.IH)
		e.mu.Unlock()
		go e.restoreTorrent(&s)
	}
	return nil
}

// restoreTorrent re-adds a persisted torrent to the client.
func (e *TorrentEngine) restoreTorrent(s *torrentStateEntry) {
	var spec *torrent.TorrentSpec
	var err error
	if s.TorrentB64 != "" {
		raw, derr := base64.StdEncoding.DecodeString(s.TorrentB64)
		if derr == nil {
			mi, merr := metainfo.Load(bytes.NewReader(raw))
			if merr == nil {
				spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
			}
		}
	}
	if spec == nil && s.Magnet != "" {
		spec, err = torrent.TorrentSpecFromMagnetUri(s.Magnet)
	}
	if spec == nil {
		spec, err = magnetSpecFromHash(s)
	}
	if err != nil || spec == nil {
		return
	}
	if len(s.Trackers) > 0 {
		spec.Trackers = mergeTrackers(spec.Trackers, s.Trackers)
	}
	if len(s.Webseeds) > 0 {
		spec.Webseeds = append(spec.Webseeds, s.Webseeds...)
	}
	if len(s.Sources) > 0 {
		spec.Sources = append(spec.Sources, s.Sources...)
	}
	if len(s.DhtNodes) > 0 {
		spec.DhtNodes = append(spec.DhtNodes, s.DhtNodes...)
	}
	if s.Name != "" && spec.DisplayName == "" {
		spec.DisplayName = s.Name
	}
	t, _, aerr := e.client.AddTorrentSpec(spec)
	if aerr != nil {
		return
	}
	e.onTorrentReady(t, s)
}

func mergeTrackers(a, b [][]string) [][]string {
	seen := map[string]bool{}
	var out [][]string
	for _, tier := range append(append([][]string{}, a...), b...) {
		var kept []string
		for _, u := range tier {
			if !seen[u] {
				seen[u] = true
				kept = append(kept, u)
			}
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

func magnetSpecFromHash(s *torrentStateEntry) (*torrent.TorrentSpec, error) {
	h := metainfo.NewHashFromHex(s.IH)
	if h.IsZero() {
		return nil, errors.New("bad infohash")
	}
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{InfoHash: h},
		Trackers:       s.Trackers,
		DisplayName:    s.Name,
	}, nil
}

// onTorrentReady applies file priorities and pause state once metadata arrives.
func (e *TorrentEngine) onTorrentReady(t *torrent.Torrent, s *torrentStateEntry) {
	select {
	case <-t.GotInfo():
	case <-t.Closed():
		return
	}
	e.mu.Lock()
	st := e.states[t.InfoHash().HexString()]
	e.mu.Unlock()
	if st == nil {
		st = s
	}
	if t.Info() == nil {
		return
	}
	for _, f := range t.Files() {
		prio := torrent.PiecePriorityNormal
		if st.FilePrio != nil {
			if v, ok := st.FilePrio[f.Path()]; ok {
				prio = torrent.PiecePriority(v)
			}
		}
		f.SetPriority(prio)
	}
	anyWanted := false
	for _, f := range t.Files() {
		if f.Priority() != torrent.PiecePriorityNone {
			anyWanted = true
			break
		}
	}
	if !anyWanted {
		t.DownloadAll()
	}
	if st.Paused {
		t.DisallowDataDownload()
		t.DisallowDataUpload()
	} else {
		t.AllowDataDownload()
		t.AllowDataUpload()
	}
	if st.MaxConns > 0 {
		t.SetMaxEstablishedConns(st.MaxConns)
	}
	if st.CustomName != "" {
		t.SetDisplayName(st.CustomName)
	}
	e.persistSoon()
}

// ---------- adding ----------

type AddInput struct {
	Magnet     string
	TorrentB64 string // base64 .torrent bytes
	Name       string
}

// AddTorrent adds a magnet or .torrent. Returns the job infohash hex.
func (e *TorrentEngine) AddTorrent(in AddInput) (string, error) {
	var spec *torrent.TorrentSpec
	var err error
	var mi *metainfo.MetaInfo

	switch {
	case in.TorrentB64 != "":
		raw, derr := base64.StdEncoding.DecodeString(in.TorrentB64)
		if derr != nil {
			return "", fmt.Errorf("bad torrent data: %w", derr)
		}
		mi, err = metainfo.Load(bytes.NewReader(raw))
		if err != nil {
			return "", fmt.Errorf("parsing .torrent: %w", err)
		}
		spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			return "", fmt.Errorf("bad torrent: %w", err)
		}
	case in.Magnet != "":
		spec, err = torrent.TorrentSpecFromMagnetUri(in.Magnet)
		if err != nil {
			return "", fmt.Errorf("bad magnet: %w", err)
		}
	default:
		return "", errors.New("nothing to add")
	}

	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return "", err
	}
	ih := t.InfoHash().HexString()

	st := &torrentStateEntry{
		IH:       ih,
		Magnet:   in.Magnet,
		Name:     in.Name,
		AddedAt:  time.Now(),
		FilePrio: map[string]int{},
	}
	if in.TorrentB64 != "" {
		st.TorrentB64 = in.TorrentB64
		if mi != nil {
			if info, ierr := mi.UnmarshalInfo(); ierr == nil {
				if n := info.BestName(); n != "" && n != metainfo.NoName {
					st.Name = n
				}
			}
			st.Trackers = mi.UpvertedAnnounceList()
			st.Webseeds = mi.UrlList
		}
	}
	if st.Name == "" {
		st.Name = spec.DisplayName
	}
	st.Trackers = mergeTrackers(st.Trackers, spec.Trackers)
	st.Webseeds = append(st.Webseeds, spec.Webseeds...)
	st.Sources = append(st.Sources, spec.Sources...)
	st.DhtNodes = append(st.DhtNodes, spec.DhtNodes...)

	e.mu.Lock()
	if old, exists := e.states[ih]; exists {
		// Already tracked — keep existing state (pause/priorities preserved).
		st = old
	} else {
		e.order = append(e.order, ih)
		e.states[ih] = st
	}
	e.mu.Unlock()

	go e.onTorrentReady(t, st)
	e.persistSoon()
	return ih, nil
}

func (e *TorrentEngine) AddMagnetURI(uri string) (string, error) {
	return e.AddTorrent(AddInput{Magnet: uri})
}

func (e *TorrentEngine) AddTorrentBytes(raw []byte) (string, error) {
	return e.AddTorrent(AddInput{TorrentB64: base64.StdEncoding.EncodeToString(raw)})
}

// AddTorrentURL fetches a .torrent over HTTP(S) and adds it.
func (e *TorrentEngine) AddTorrentURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Stroom/"+e.app.version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	name := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if i := strings.Index(cd, "filename="); i >= 0 {
			name = strings.Trim(cd[i+9:], "\"; ")
			name = strings.TrimSuffix(name, ".torrent")
		}
	}
	ih, err := e.AddTorrent(AddInput{TorrentB64: base64.StdEncoding.EncodeToString(raw), Name: name})
	return ih, err
}

// ---------- control ----------

func (e *TorrentEngine) find(ih string) (*torrent.Torrent, *torrentStateEntry, error) {
	h := metainfo.NewHashFromHex(ih)
	if t, ok := e.client.Torrent(h); ok {
		e.mu.Lock()
		st := e.states[ih]
		e.mu.Unlock()
		return t, st, nil
	}
	return nil, nil, errNotFound
}

func (e *TorrentEngine) Pause(ih string) error {
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	if st != nil {
		st.Paused = true
	}
	t.DisallowDataDownload()
	t.DisallowDataUpload()
	e.persistSoon()
	return nil
}

func (e *TorrentEngine) Resume(ih string) error {
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	if t.Info() != nil {
		for _, f := range t.Files() {
			if st != nil && st.FilePrio != nil {
				if v, ok := st.FilePrio[f.Path()]; ok {
					f.SetPriority(torrent.PiecePriority(v))
					continue
				}
			}
			if f.Priority() == torrent.PiecePriorityNone {
				f.SetPriority(torrent.PiecePriorityNormal)
			}
		}
	}
	if st != nil {
		st.Paused = false
	}
	t.AllowDataDownload()
	t.AllowDataUpload()
	e.persistSoon()
	return nil
}

func (e *TorrentEngine) Remove(ih string, deleteFiles bool) error {
	t, _, err := e.find(ih)
	e.mu.Lock()
	dir := e.dirs[ih]
	delete(e.states, ih)
	delete(e.dirs, ih)
	delete(e.rates, ih)
	delete(e.lastRates, ih)
	e.mu.Unlock()
	e.rebuildOrder()
	if err == nil {
		t.Drop()
	}
	if deleteFiles && dir != "" {
		_ = os.RemoveAll(dir)
	}
	e.persistSoon()
	return nil
}

func (e *TorrentEngine) rebuildOrder() {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ih := range e.order {
		if _, ok := e.states[ih]; ok {
			out = append(out, ih)
		}
	}
	e.order = out
}

func (e *TorrentEngine) SetFilePriority(ih string, filePath string, prio int) error {
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	if t.Info() == nil {
		return errors.New("metadata not available yet")
	}
	for _, f := range t.Files() {
		if f.Path() == filePath || f.DisplayPath() == filePath {
			f.SetPriority(torrent.PiecePriority(prio))
			if st != nil && st.FilePrio != nil {
				st.FilePrio[filePath] = prio
			}
			e.persistSoon()
			return nil
		}
	}
	return errNotFound
}

// persistSoon debounces state writes.
func (e *TorrentEngine) persistSoon() {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	if e.persistT != nil {
		e.persistT.Reset(2 * time.Second)
		return
	}
	e.persistT = time.AfterFunc(2*time.Second, func() { _ = e.Save() })
}

// SetRateLimits applies new global rate limits live (0 = unlimited).
func (e *TorrentEngine) SetRateLimits(down, up int64) {
	e.mu.Lock()
	e.dlLim, e.ulLim = down, up
	e.mu.Unlock()
	if e.dlLimiter != nil {
		if down <= 0 {
			e.dlLimiter.SetLimit(rate.Inf)
		} else {
			e.dlLimiter.SetLimit(rate.Limit(down))
		}
	}
	if e.ulLimiter != nil {
		if up <= 0 {
			e.ulLimiter.SetLimit(rate.Inf)
		} else {
			e.ulLimiter.SetLimit(rate.Limit(up))
		}
	}
}
