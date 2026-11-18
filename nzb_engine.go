package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

func httpNewRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, "GET", url, nil)
}

func filenameFromDisposition(cd string) string {
	if i := strings.Index(cd, "filename="); i >= 0 {
		name := strings.Trim(cd[i+9:], "\"; ")
		name = strings.TrimSuffix(name, ".nzb")
		return name
	}
	return ""
}

// NZBFileState is the persisted+API view of one file inside an NZB job.
type NZBFileState struct {
	Name       string       `json:"name"`
	Subject    string       `json:"subject,omitempty"`
	TotalBytes int64        `json:"totalBytes"`
	DoneBytes  int64        `json:"doneBytes"`
	Segments   int          `json:"segments"`
	SegDone    int          `json:"segDone"`
	Failed     []int        `json:"failed,omitempty"` // segment numbers that failed
	Complete   bool         `json:"complete"`
	Encoding   string       `json:"encoding,omitempty"`
	SegList    []NZBSegment `json:"segs,omitempty"` // full segment list (needed after restart)

	assembling bool `json:"-"` // runtime: assembly in progress
}

// NZBJob is the API representation of one NZB download.
type NZBJob struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"` // "nzb"
	Name           string          `json:"name"`
	ServerID       string          `json:"serverID"`
	ServerName     string          `json:"serverName,omitempty"`
	Dir            string          `json:"dir"`    // absolute output dir
	Status         string          `json:"status"` // queued|downloading|paused|done|error
	Error          string          `json:"error,omitempty"`
	TotalBytes     int64           `json:"totalBytes"`
	DoneBytes      int64           `json:"doneBytes"`
	Progress       float64         `json:"progress"`
	DownSpeed      int64           `json:"downSpeed"`
	SegmentsTotal  int             `json:"segmentsTotal"`
	SegmentsDone   int             `json:"segmentsDone"`
	SegmentsFailed int             `json:"segmentsFailed"`
	Files          []*NZBFileState `json:"files"`
	AddedAt        time.Time       `json:"addedAt"`
	StartedAt      time.Time       `json:"startedAt,omitempty"`
	FinishedAt     time.Time       `json:"finishedAt,omitempty"`

	// runtime-only (not persisted)
	doneSet   map[string]map[int]bool
	failedSet map[string]map[int]bool
	mu        *sync.Mutex // guards Files fields, counters, doneSet, failedSet
}

// NZBEngine manages NZB downloads over NNTP.
type NZBEngine struct {
	app       *App
	mu        sync.Mutex
	jobs      map[string]*NZBJob
	order     []string
	pools     map[string]*nntpPool
	ids       idGen
	persistMu sync.Mutex
	persistT  *time.Timer
	runtimes  map[string]*nzbRuntime
	nzbDir    string // <downloadDir>/nzb
}

type nzbRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
	asmWg  sync.WaitGroup // tracks in-flight file assemblies
	// speed sampling
	mu       sync.Mutex
	lastDone int64
	lastTS   time.Time
	down     int64
	up       int64
}

func NewNZBEngine(app *App) (*NZBEngine, error) {
	e := &NZBEngine{
		app:      app,
		jobs:     make(map[string]*NZBJob),
		pools:    make(map[string]*nntpPool),
		runtimes: make(map[string]*nzbRuntime),
		nzbDir:   filepath.Join(app.Config.DownloadDir, "nzb"),
	}
	if err := ensureDir(e.nzbDir); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *NZBEngine) Close() {
	e.mu.Lock()
	for _, rt := range e.runtimes {
		rt.cancel()
	}
	pools := e.pools
	e.pools = map[string]*nntpPool{}
	e.mu.Unlock()
	for _, p := range pools {
		p.Close()
	}
	e.Save()
}

// ---------- persistence ----------

type nzbStateFile struct {
	Jobs []persistedNZBJob `json:"jobs"`
}

// persistedNZBJob strips runtime fields for JSON.
type persistedNZBJob struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ServerID   string          `json:"serverID"`
	Dir        string          `json:"dir"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	TotalBytes int64           `json:"totalBytes"`
	DoneBytes  int64           `json:"doneBytes"`
	AddedAt    time.Time       `json:"addedAt"`
	Files      []*NZBFileState `json:"files"`
}

func (e *NZBEngine) statePath() string {
	return filepath.Join(e.app.DataDir, "nzb-state.json")
}

func (e *NZBEngine) Save() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out nzbStateFile
	for _, id := range e.order {
		j := e.jobs[id]
		if j == nil {
			continue
		}
		j.mu.Lock()
		files := make([]*NZBFileState, len(j.Files))
		for i, f := range j.Files {
			fc := *f
			fc.Failed = append([]int{}, f.Failed...)
			files[i] = &fc
		}
		out.Jobs = append(out.Jobs, persistedNZBJob{
			ID: j.ID, Name: j.Name, ServerID: j.ServerID, Dir: j.Dir,
			Status: j.Status, Error: j.Error,
			TotalBytes: j.TotalBytes, DoneBytes: j.DoneBytes,
			AddedAt: j.AddedAt, Files: files,
		})
		j.mu.Unlock()
	}
	return writeJSON(e.statePath(), out)
}

func (e *NZBEngine) Load() error {
	var sf nzbStateFile
	if err := readJSON(e.statePath(), &sf); err != nil {
		return err
	}
	for i := range sf.Jobs {
		p := sf.Jobs[i]
		j := &NZBJob{
			ID: p.ID, Kind: "nzb", Name: p.Name, ServerID: p.ServerID,
			Dir: p.Dir, Status: p.Status, Error: p.Error,
			TotalBytes: p.TotalBytes, DoneBytes: p.DoneBytes,
			AddedAt: p.AddedAt, Files: p.Files,
			doneSet: map[string]map[int]bool{}, failedSet: map[string]map[int]bool{},
			mu: new(sync.Mutex),
		}
		for _, f := range j.Files {
			if f.Failed == nil {
				f.Failed = []int{}
			}
			j.doneSet[f.Name] = map[int]bool{}
			j.failedSet[f.Name] = map[int]bool{}
			for _, n := range f.Failed {
				j.failedSet[f.Name][n] = true
			}
		}
		e.mu.Lock()
		e.jobs[j.ID] = j
		e.order = append(e.order, j.ID)
		e.mu.Unlock()
		// Rebuild done-state from parts on disk (source of truth), then
		// auto-resume incomplete jobs.
		go func(job *NZBJob) {
			e.rebuildDoneFromDisk(job)
			if job.Status == "downloading" || job.Status == "queued" {
				e.resumeJob(job)
			}
		}(j)
	}
	return nil
}

// ---------- adding ----------

// AddNZB registers a parsed NZB for download. serverID may be "" → first server.
func (e *NZBEngine) AddNZB(doc *NZBDoc, nzbName, serverID string) (string, error) {
	if len(doc.Files) == 0 {
		return "", fmt.Errorf("nzb has no files")
	}
	if serverID == "" {
		if srv := e.defaultServer(); srv != nil {
			serverID = srv.ID
		} else {
			return "", fmt.Errorf("no news server configured — add one in Settings")
		}
	}
	if e.serverByID(serverID) == nil {
		return "", fmt.Errorf("unknown server %q", serverID)
	}

	id := e.ids.next("nzb")
	dirName := sanitizeName(nzbName)
	if dirName == "" || dirName == "nzb" {
		dirName = fmt.Sprintf("nzb-%s", id[len(id)-6:])
	}
	dir := filepath.Join(e.nzbDir, dirName+"-"+shortHash(id))
	if err := ensureDir(dir); err != nil {
		return "", err
	}

	j := &NZBJob{
		ID: id, Kind: "nzb", Name: nzbName, ServerID: serverID,
		Dir: dir, Status: "queued",
		AddedAt:   time.Now(),
		doneSet:   map[string]map[int]bool{},
		failedSet: map[string]map[int]bool{},
		mu:        new(sync.Mutex),
	}
	for _, nf := range doc.Files {
		fs := &NZBFileState{
			Name: nf.Name, Subject: nf.Subject,
			TotalBytes: nf.TotalBytes, Segments: len(nf.Segments),
			Failed:  []int{},
			SegList: append([]NZBSegment{}, nf.Segments...),
		}
		j.Files = append(j.Files, fs)
		j.doneSet[nf.Name] = map[int]bool{}
		j.failedSet[nf.Name] = map[int]bool{}
		j.TotalBytes += nf.TotalBytes
		j.SegmentsTotal += len(nf.Segments)
	}

	e.mu.Lock()
	e.jobs[id] = j
	e.order = append(e.order, id)
	e.mu.Unlock()
	e.persistSoon()
	go e.startJob(j)
	return id, nil
}

// ParseAndAdd handles raw .nzb bytes (already gunzipped-or-not) + optional name.
func (e *NZBEngine) ParseAndAdd(data []byte, nzbName, serverID string) (string, error) {
	doc, err := ParseNZB(data)
	if err != nil {
		return "", err
	}
	if nzbName == "" {
		nzbName = doc.Name
	}
	return e.AddNZB(doc, nzbName, serverID)
}

// FetchAndAdd downloads an .nzb over HTTP(S) then adds it.
func (e *NZBEngine) FetchAndAdd(ctx context.Context, url, serverID string) (string, error) {
	req, err := httpNewRequest(ctx, url)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	name := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		name = filenameFromDisposition(cd)
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(url), ".nzb")
	}
	return e.ParseAndAdd(data, name, serverID)
}

// ---------- control ----------

func (e *NZBEngine) find(id string) *NZBJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.jobs[id]
}

func (e *NZBEngine) Pause(id string) error {
	j := e.find(id)
	if j == nil {
		return errNotFound
	}
	e.mu.Lock()
	if rt := e.runtimes[id]; rt != nil {
		rt.cancel()
	}
	j.Status = "paused"
	e.mu.Unlock()
	e.persistSoon()
	return nil
}

func (e *NZBEngine) Resume(id string) error {
	j := e.find(id)
	if j == nil {
		return errNotFound
	}
	if j.Status == "done" {
		return nil
	}
	j.Status = "queued"
	j.Error = ""
	e.persistSoon()
	go e.resumeJob(j)
	return nil
}

func (e *NZBEngine) Remove(id string, deleteFiles bool) error {
	j := e.find(id)
	if j == nil {
		return errNotFound
	}
	e.mu.Lock()
	if rt := e.runtimes[id]; rt != nil {
		rt.cancel()
	}
	delete(e.jobs, id)
	delete(e.runtimes, id)
	e.mu.Unlock()
	e.rebuildOrder()
	if deleteFiles && j.Dir != "" {
		_ = os.RemoveAll(j.Dir)
	}
	e.persistSoon()
	return nil
}

func (e *NZBEngine) rebuildOrder() {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, id := range e.order {
		if _, ok := e.jobs[id]; ok {
			out = append(out, id)
		}
	}
	e.order = out
}

// ---------- listing / stats ----------

func (e *NZBEngine) List() []*NZBJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*NZBJob, 0, len(e.order))
	for _, id := range e.order {
		if j := e.jobs[id]; j != nil {
			out = append(out, e.snapshotLocked(j))
		}
	}
	return out
}

// snapshotLocked builds an API-safe copy + refreshes progress/speed.
func (e *NZBEngine) snapshotLocked(j *NZBJob) *NZBJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := *j
	cp.Files = make([]*NZBFileState, len(j.Files))
	for i, f := range j.Files {
		fc := *f
		fc.SegList = nil // don't ship segment lists in snapshots
		fc.Failed = append([]int{}, f.Failed...)
		cp.Files[i] = &fc
	}
	var total, done int64
	var segs, segDone, segFail int
	for _, f := range j.Files {
		total += f.TotalBytes
		done += f.DoneBytes
		segs += f.Segments
		segDone += f.SegDone
		segFail += len(f.Failed)
	}
	cp.TotalBytes = total
	cp.DoneBytes = done
	cp.SegmentsTotal = segs
	cp.SegmentsDone = segDone
	cp.SegmentsFailed = segFail
	if total > 0 {
		cp.Progress = float64(done) / float64(total)
		if cp.Progress > 1 {
			cp.Progress = 1
		}
	}
	// speed from runtime
	if rt := e.runtimes[j.ID]; rt != nil {
		rt.mu.Lock()
		now := time.Now()
		if !rt.lastTS.IsZero() {
			if dt := now.Sub(rt.lastTS).Seconds(); dt > 0 {
				rt.down = int64(float64(j.DoneBytes-rt.lastDone) / dt)
			}
		}
		rt.lastDone = j.DoneBytes
		rt.lastTS = now
		cp.DownSpeed = rt.down
		rt.mu.Unlock()
	} else {
		cp.DownSpeed = 0
	}
	return &cp
}

func (e *NZBEngine) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, j := range e.jobs {
		if j.Status == "downloading" || j.Status == "queued" {
			n++
		}
	}
	return n
}

// GlobalDownSpeed sums active NZB download speeds.
func (e *NZBEngine) GlobalDownSpeed() int64 {
	var sum int64
	for _, j := range e.List() {
		sum += j.DownSpeed
	}
	return sum
}

// FileDiskPath returns absolute path for an NZB output file.
func (e *NZBEngine) FileDiskPath(jobID, name string) (string, bool) {
	j := e.find(jobID)
	if j == nil {
		return "", false
	}
	// Guard against traversal.
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", false
	}
	p := filepath.Join(j.Dir, clean)
	return p, true
}

// persistSoon debounces state writes.
func (e *NZBEngine) persistSoon() {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	if e.persistT != nil {
		e.persistT.Reset(2 * time.Second)
		return
	}
	e.persistT = time.AfterFunc(2*time.Second, func() { _ = e.Save() })
}

// ---------- server helpers ----------

func (e *NZBEngine) serverByID(id string) *NNTPServer {
	srvs := e.app.servers()
	for i := range srvs {
		if srvs[i].ID == id {
			return &srvs[i]
		}
	}
	return nil
}

func (e *NZBEngine) defaultServer() *NNTPServer {
	srvs := e.app.servers()
	if len(srvs) == 0 {
		return nil
	}
	return &srvs[0]
}

func (e *NZBEngine) poolFor(srv *NNTPServer) *nntpPool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.pools[srv.ID]; ok {
		return p
	}
	p := newNNTPPool(*srv)
	e.pools[srv.ID] = p
	return p
}

// ResetPools drops all NNTP pools (call after server config changes).
func (e *NZBEngine) ResetPools() {
	e.mu.Lock()
	pools := e.pools
	e.pools = map[string]*nntpPool{}
	e.mu.Unlock()
	for _, p := range pools {
		p.Close()
	}
}

// ServerName returns display name for a server id.
func (e *NZBEngine) ServerName(id string) string {
	if s := e.serverByID(id); s != nil {
		if s.Name != "" {
			return s.Name
		}
		return s.Host
	}
	return ""
}

// Files returns API view of an NZB job's files.
func (e *NZBEngine) Files(id string) ([]map[string]any, error) {
	j := e.find(id)
	if j == nil {
		return nil, errNotFound
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]map[string]any, 0, len(j.Files))
	for _, f := range j.Files {
		out = append(out, map[string]any{
			"name":     f.Name,
			"size":     f.TotalBytes,
			"done":     f.DoneBytes,
			"segments": f.Segments,
			"segDone":  f.SegDone,
			"failed":   len(f.Failed),
			"complete": f.Complete,
			"subject":  f.Subject,
		})
	}
	return out, nil
}

// JobDir returns the output directory for an NZB job.
func (e *NZBEngine) JobDir(id string) (string, bool) {
	j := e.find(id)
	if j == nil || j.Dir == "" {
		return "", false
	}
	return j.Dir, true
}
