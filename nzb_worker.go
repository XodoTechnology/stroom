package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	nzbMaxAttempts    = 3
	nzbPartsDirName   = ".parts"
	nzbDefaultWorkers = 12
)

// segTask is one article download unit.
type segTask struct {
	fileIdx int
	segPos  int
	seg     NZBSegment
}

// startJob launches the feeder + workers for a job.
func (e *NZBEngine) startJob(j *NZBJob) {
	srv := e.serverByID(j.ServerID)
	if srv == nil {
		j.mu.Lock()
		j.Status = "error"
		j.Error = "news server no longer configured"
		j.mu.Unlock()
		e.persistSoon()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &nzbRuntime{cancel: cancel, done: make(chan struct{})}

	e.mu.Lock()
	e.runtimes[j.ID] = rt
	e.mu.Unlock()

	j.mu.Lock()
	j.Status = "downloading"
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now()
	}
	// Pre-create parts dirs.
	for i := range j.Files {
		_ = ensureDir(e.partsDir(j, i))
	}
	j.mu.Unlock()

	workers := srv.Connections
	if workers <= 0 {
		workers = e.app.configSnapshot().NZBMaxConnections
	}
	if workers <= 0 {
		workers = nzbDefaultWorkers
	}
	workers = clampInt(workers, 1, 64)

	taskCh := make(chan segTask, workers*4)
	var wg sync.WaitGroup

	// Feeder: enumerate pending segments, honour ctx for pause.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(taskCh)
		e.feedSegments(ctx, j, taskCh)
	}()

	// Workers.
	pool := e.poolFor(srv)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.segWorker(ctx, j, rt, pool, taskCh)
		}()
	}

	// Monitor: when all workers + assemblies exit, finalize status.
	go func() {
		wg.Wait()
		rt.asmWg.Wait()
		e.finalizeJob(j, rt)
	}()
}

// resumeJob restarts a paused/queued/failed job (clears failed segs).
func (e *NZBEngine) resumeJob(j *NZBJob) {
	// Stop any stale runtime first.
	e.mu.Lock()
	if rt := e.runtimes[j.ID]; rt != nil {
		rt.cancel()
	}
	e.mu.Unlock()

	// Rebuild done-state from disk so missing parts get retried.
	e.rebuildDoneFromDisk(j)

	j.mu.Lock()
	// Clear failed marks so feeder retries them.
	for _, f := range j.Files {
		f.Failed = f.Failed[:0]
		if m := j.failedSet[f.Name]; m != nil {
			for k := range m {
				delete(m, k)
			}
		}
	}
	j.Error = ""
	j.Status = "queued"
	j.mu.Unlock()

	e.startJob(j)
}

// feedSegments pushes all pending segment tasks into ch until exhausted or ctx done.
// Never holds j.mu while sending (workers need it to commit).
func (e *NZBEngine) feedSegments(ctx context.Context, j *NZBJob, ch chan<- segTask) {
	for fi := range j.Files {
		f := j.Files[fi]
		for pos, seg := range f.SegList {
			if ctx.Err() != nil {
				return
			}
			j.mu.Lock()
			skip := f.Complete || j.doneSet[f.Name][seg.Number] || j.failedSet[f.Name][seg.Number]
			j.mu.Unlock()
			if skip {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- segTask{fileIdx: fi, segPos: pos, seg: seg}:
			}
		}
	}
}

// segWorker pulls tasks and downloads them.
func (e *NZBEngine) segWorker(ctx context.Context, j *NZBJob, rt *nzbRuntime, pool *nntpPool, ch <-chan segTask) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ch:
			if !ok {
				return
			}
			e.doSegment(ctx, j, rt, pool, t)
		}
	}
}

// doSegment fetches, decodes and stores one segment with retries.
func (e *NZBEngine) doSegment(ctx context.Context, j *NZBJob, rt *nzbRuntime, pool *nntpPool, t segTask) {
	f := j.Files[t.fileIdx]
	for attempt := 0; attempt < nzbMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			}
		}
		data, err := e.fetchDecode(ctx, pool, t.seg.MessageID)
		if errors.Is(err, errNoSuchArticle) {
			// Primary provider lacks it — another backbone may carry it.
			data, err = e.fetchFallback(ctx, j.ServerID, t.seg.MessageID)
		}
		if err != nil {
			if errors.Is(err, errNoSuchArticle) {
				break // article missing on every provider; retrying won't help
			}
			continue
		}
		if len(data) == 0 {
			continue
		}
		// Write part file.
		pp := e.partPath(j, t.fileIdx, t.segPos)
		if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(pp, data, 0o644); err != nil {
			continue
		}
		// Commit.
		j.mu.Lock()
		f.SegDone++
		f.DoneBytes += int64(len(data))
		j.DoneBytes += int64(len(data))
		j.doneSet[f.Name][t.seg.Number] = true
		j.mu.Unlock()

		// File complete? → assemble.
		j.mu.Lock()
		complete := f.SegDone >= f.Segments && !f.Complete && !f.assembling
		if complete {
			f.assembling = true
		}
		j.mu.Unlock()
		if complete {
			rt.asmWg.Add(1)
			go func() {
				defer rt.asmWg.Done()
				e.assembleFile(j, t.fileIdx)
			}()
		}
		return
	}
	// Failed after retries — nothing was committed, just record it.
	j.mu.Lock()
	if !j.failedSet[f.Name][t.seg.Number] {
		f.Failed = append(f.Failed, t.seg.Number)
		j.failedSet[f.Name][t.seg.Number] = true
	}
	j.mu.Unlock()
}

// fetchDecode gets article body and yEnc-decodes it.
func (e *NZBEngine) fetchDecode(ctx context.Context, pool *nntpPool, msgID string) ([]byte, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	body, err := c.fetchBody(msgID)
	bad := err != nil
	pool.Release(c, bad)
	if err != nil {
		return nil, err
	}
	res, derr := decodeYEnc(body)
	if derr != nil {
		return nil, derr
	}
	return res.Data, nil
}

// fetchFallback asks every other configured server for an article the
// primary reported missing (430). Returns the first hit. errNoSuchArticle
// means every reachable server said 430; a transient error is returned as-is
// so the caller retries rather than giving up on a temporarily-down server.
func (e *NZBEngine) fetchFallback(ctx context.Context, primaryID, msgID string) ([]byte, error) {
	var lastErr error
	for _, srv := range e.app.servers() {
		if srv.ID == primaryID {
			continue
		}
		data, err := e.fetchDecode(ctx, e.poolFor(&srv), msgID)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, errNoSuchArticle) {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errNoSuchArticle
}

// partsDir returns the per-file parts directory.
func (e *NZBEngine) partsDir(j *NZBJob, fileIdx int) string {
	return filepath.Join(j.Dir, nzbPartsDirName, fmt.Sprintf("f%03d", fileIdx))
}

func (e *NZBEngine) partPath(j *NZBJob, fileIdx, segPos int) string {
	return filepath.Join(e.partsDir(j, fileIdx), fmt.Sprintf("seg-%06d", segPos))
}

// assembleFile concatenates a completed file's parts into the final name.
func (e *NZBEngine) assembleFile(j *NZBJob, fileIdx int) {
	defer func() {
		j.mu.Lock()
		j.Files[fileIdx].assembling = false
		j.mu.Unlock()
		e.persistSoon()
	}()

	f := j.Files[fileIdx]
	outPath := filepath.Join(j.Dir, f.Name)
	tmpPath := outPath + ".assembling"
	partsDir := e.partsDir(j, fileIdx)

	out, err := os.Create(tmpPath)
	if err != nil {
		j.mu.Lock()
		j.Error = fmt.Sprintf("assemble %s: %v", f.Name, err)
		j.mu.Unlock()
		return
	}
	fail := func(err error) {
		out.Close()
		os.Remove(tmpPath)
		j.mu.Lock()
		j.Error = fmt.Sprintf("assemble %s: %v", f.Name, err)
		j.mu.Unlock()
	}
	missing := false
	for pos := range f.SegList {
		pp := filepath.Join(partsDir, fmt.Sprintf("seg-%06d", pos))
		pf, rerr := os.Open(pp)
		if rerr != nil {
			missing = true
			break
		}
		_, werr := io.Copy(out, pf)
		pf.Close()
		if werr != nil {
			fail(werr)
			return
		}
	}
	out.Close()
	if missing {
		os.Remove(tmpPath)
		// A part vanished (e.g. manual cleanup). Unmark it so a resume retries.
		j.mu.Lock()
		for pos, seg := range f.SegList {
			pp := filepath.Join(partsDir, fmt.Sprintf("seg-%06d", pos))
			if _, serr := os.Stat(pp); serr != nil {
				if j.doneSet[f.Name][seg.Number] {
					delete(j.doneSet[f.Name], seg.Number)
					if f.SegDone > 0 {
						f.SegDone--
					}
				}
			}
		}
		j.mu.Unlock()
		return
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		j.mu.Lock()
		j.Error = fmt.Sprintf("assemble %s: %v", f.Name, err)
		j.mu.Unlock()
		return
	}
	// Remove parts dir.
	_ = os.RemoveAll(partsDir)

	j.mu.Lock()
	f.Complete = true
	j.mu.Unlock()

	// Job complete?
	j.mu.Lock()
	allDone := true
	for _, fj := range j.Files {
		if !fj.Complete {
			allDone = false
			break
		}
	}
	if allDone {
		j.Status = "done"
		j.FinishedAt = time.Now()
		if rt := e.runtimes[j.ID]; rt != nil {
			rt.cancel()
		}
	}
	j.mu.Unlock()
	e.persistSoon()
}

// rebuildDoneFromDisk reconstructs doneSet/SegDone/DoneBytes from .parts files.
// Called on load and resume; parts on disk are the source of truth.
func (e *NZBEngine) rebuildDoneFromDisk(j *NZBJob) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for fi, f := range j.Files {
		if f.Complete {
			continue
		}
		dir := e.partsDir(j, fi)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		done := map[int]bool{}
		var doneBytes int64
		for _, en := range entries {
			var pos int
			if _, err := fmt.Sscanf(en.Name(), "seg-%d", &pos); err != nil {
				continue
			}
			if pos < 0 || pos >= len(f.SegList) {
				continue
			}
			seg := f.SegList[pos]
			info, serr := en.Info()
			if serr != nil {
				continue
			}
			done[seg.Number] = true
			doneBytes += info.Size()
		}
		// Rebuild sets.
		j.doneSet[f.Name] = done
		f.SegDone = len(done)
		f.DoneBytes = doneBytes
	}
	// Recompute job DoneBytes.
	var total int64
	for _, f := range j.Files {
		total += f.DoneBytes
	}
	j.DoneBytes = total
}

// finalizeJob runs after feeder+workers exit; sets terminal status.
// When PAR2 recovery files are present (and the par2 binary exists), the job
// goes through "verifying" — par2 repairs missing/corrupt data, then the job
// resolves to done or error.
func (e *NZBEngine) finalizeJob(j *NZBJob, rt *nzbRuntime) {
	close(rt.done)
	j.mu.Lock()
	if j.Status == "done" || j.Status == "paused" {
		j.mu.Unlock()
		return
	}
	incomplete := false
	for _, f := range j.Files {
		if !f.Complete {
			incomplete = true
			break
		}
	}
	verify := false
	switch {
	case j.Status == "paused":
		// keep paused state
	case !incomplete && par2ControlFile(j.Dir) != "" && par2Available():
		verify = true // verify integrity; repairs only if needed
		j.Status = "verifying"
	case !incomplete:
		j.Status = "done"
		j.FinishedAt = time.Now()
	case incomplete && par2ControlFile(j.Dir) != "" && par2Available():
		verify = true // repair missing segments from recovery blocks
		j.Status = "verifying"
	case incomplete && par2ControlFile(j.Dir) != "":
		j.Status = "error"
		j.Error = "repair needed but par2 not installed"
	default:
		j.Status = "error"
		if j.Error == "" {
			j.Error = "download incomplete"
		}
	}
	j.mu.Unlock()
	e.persistSoon()
	if verify {
		go e.repairWithPar(j)
	}
}

// repairWithPar runs par2 verify/repair in the job dir, then resolves the job.
func (e *NZBEngine) repairWithPar(j *NZBJob) {
	// Materialize incomplete files with zero-filled gaps so par2 treats them
	// as damaged (repairable) rather than fully missing (not repairable
	// unless recovery data covers the whole file).
	for i := range j.Files {
		if !j.Files[i].Complete {
			if err := e.assembleGapped(j, i); err != nil {
				j.mu.Lock()
				j.Status = "error"
				j.Error = fmt.Sprintf("assemble %s: %v", j.Files[i].Name, err)
				j.mu.Unlock()
				e.persistSoon()
				return
			}
		}
	}
	ok, errStr := runPar2Repair(j.Dir)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status != "verifying" {
		return // paused/removed meanwhile
	}
	if ok {
		for i := range j.Files {
			j.Files[i].Complete = true
			_ = os.RemoveAll(e.partsDir(j, i))
		}
		j.Status = "done"
		j.FinishedAt = time.Now()
		j.Error = ""
	} else {
		j.Status = "error"
		j.Error = errStr
	}
	e.persistSoon()
}

// assembleGapped writes an incomplete file for par2 repair: present parts in
// order, missing segments zero-filled. Gap size = decoded size implied by the
// encoded/decoded ratio of present parts; the final segment's gap is clamped
// so the file lands exactly at TotalBytes.
func (e *NZBEngine) assembleGapped(j *NZBJob, fileIdx int) error {
	f := j.Files[fileIdx]

	// Learn encoded→decoded ratio from present parts (uniform per post).
	var encSum, decSum int64
	for pos := range f.SegList {
		if st, err := os.Stat(e.partPath(j, fileIdx, pos)); err == nil {
			decSum += st.Size()
			encSum += f.SegList[pos].Bytes
		}
	}
	gapFor := func(seg NZBSegment, pos int, written int64) int64 {
		if pos == len(f.SegList)-1 && f.TotalBytes > written {
			return f.TotalBytes - written
		}
		if encSum > 0 && seg.Bytes > 0 {
			if g := seg.Bytes * decSum / encSum; g > 0 {
				return g
			}
		}
		if n := int64(len(f.SegList)); n > 0 && f.TotalBytes > 0 {
			return f.TotalBytes / n
		}
		return 0
	}

	tmpPath := filepath.Join(j.Dir, f.Name+".assembling")
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	var written int64
	zeros := make([]byte, 1<<20)
	fail := func(e2 error) error { out.Close(); return e2 }
	for pos, seg := range f.SegList {
		pp := e.partPath(j, fileIdx, pos)
		pf, rerr := os.Open(pp)
		if rerr == nil {
			n, werr := io.Copy(out, pf)
			pf.Close()
			if werr != nil {
				return fail(werr)
			}
			written += n
			continue
		}
		for gap := gapFor(seg, pos, written); gap > 0; {
			chunk := gap
			if chunk > int64(len(zeros)) {
				chunk = int64(len(zeros))
			}
			if _, werr := out.Write(zeros[:chunk]); werr != nil {
				return fail(werr)
			}
			gap -= chunk
			written += chunk
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(j.Dir, f.Name))
}

// par2Available reports whether a par2cmdline binary is on PATH.
func par2Available() bool {
	_, err := exec.LookPath("par2")
	return err == nil
}

// par2ControlFile finds a .par2 control file (non-volume preferred) in dir.
func par2ControlFile(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	volume := ""
	for _, en := range ents {
		n := strings.ToLower(en.Name())
		if en.IsDir() || !strings.HasSuffix(n, ".par2") {
			continue
		}
		if !strings.Contains(n, ".vol") {
			return en.Name() // index/recovery control file
		}
		volume = en.Name()
	}
	return volume // only volume files found — still usable
}

// runPar2Repair executes `par2 repair` on the control file. par2 verifies
// first and only rebuilds what's missing/corrupt; exit 0 = complete set.
func runPar2Repair(dir string) (bool, string) {
	ctrl := par2ControlFile(dir)
	if ctrl == "" {
		return true, ""
	}
	cmd := exec.Command("par2", "repair", "-q", "-q", ctrl)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return false, "par2: " + msg
	}
	return true, ""
}

var (
	_ = io.EOF
	_ = sort.Ints
	_ = sync.Once{}
)
