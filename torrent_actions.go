package main

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// RevealDir returns the on-disk directory for a torrent job.
func (e *TorrentEngine) RevealDir(ih string) (string, error) {
	dir := e.DirFor(ih)
	if dir == "" {
		return "", errors.New("download directory not known yet (waiting for metadata)")
	}
	return dir, nil
}

// OpenFilePath resolves a torrent-relative path to an absolute disk path
// (same safety rules as the download endpoint).
func (e *TorrentEngine) OpenFilePath(ih, rel string) (string, bool) {
	return e.FileDiskPath(ih, rel)
}

// AddTrackers adds announce URLs to a live torrent and persists them.
func (e *TorrentEngine) AddTrackers(ih string, urls []string) error {
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	clean := []string{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "udp://") {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return errors.New("no valid tracker URLs (want http(s):// or udp://)")
	}
	t.AddTrackers([][]string{clean})
	if st != nil {
		e.mu.Lock()
		st.Trackers = mergeTrackers(st.Trackers, [][]string{clean})
		e.mu.Unlock()
	}
	e.persistSoon()
	return nil
}

// Recheck forces a full piece re-hash in the background.
func (e *TorrentEngine) Recheck(ih string) error {
	t, _, err := e.find(ih)
	if err != nil {
		return err
	}
	if t.Info() == nil {
		return errors.New("metadata not available yet")
	}
	go func() { _ = t.VerifyData() }()
	return nil
}

// Rename sets a custom display name (persisted, shown in UI).
func (e *TorrentEngine) Rename(ih, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	if st != nil {
		st.CustomName = name
	}
	t.SetDisplayName(name)
	e.persistSoon()
	return nil
}

// MagnetURI builds a magnet link for a torrent (infohash + name + trackers).
func (e *TorrentEngine) MagnetURI(ih string) (string, error) {
	t, st, err := e.find(ih)
	if err != nil {
		return "", err
	}
	var trackers []string
	if st != nil {
		for _, tier := range st.Trackers {
			trackers = append(trackers, tier...)
		}
	}
	name := ""
	if st != nil {
		name = st.CustomName
		if name == "" {
			name = st.Name
		}
	}
	if name == "" {
		name = t.Name()
	}
	m := "magnet:?xt=urn:btih:" + ih
	if name != "" {
		m += "&dn=" + url.QueryEscape(name)
	}
	for _, tr := range trackers {
		m += "&tr=" + url.QueryEscape(tr)
	}
	return m, nil
}

// TorrentFile returns the raw .torrent bytes for download, if we have them.
func (e *TorrentEngine) TorrentFile(ih string) ([]byte, string, bool) {
	e.mu.Lock()
	st := e.states[ih]
	e.mu.Unlock()
	if st == nil || st.TorrentB64 == "" {
		return nil, "", false
	}
	raw, err := base64.StdEncoding.DecodeString(st.TorrentB64)
	if err != nil {
		return nil, "", false
	}
	name := st.CustomName
	if name == "" {
		name = st.Name
	}
	if name == "" {
		name = ih
	}
	return raw, name + ".torrent", true
}

// SetMaxConns adjusts the per-torrent connection cap (0 = client default).
func (e *TorrentEngine) SetMaxConns(ih string, n int) error {
	t, st, err := e.find(ih)
	if err != nil {
		return err
	}
	if n < 0 {
		n = 0
	}
	if n > 0 {
		t.SetMaxEstablishedConns(n)
	}
	if st != nil {
		st.MaxConns = n
	}
	e.persistSoon()
	return nil
}

// TrackerList returns flattened tracker URLs for the details view.
func (e *TorrentEngine) TrackerList(ih string) []string {
	e.mu.Lock()
	st := e.states[ih]
	e.mu.Unlock()
	if st == nil {
		return nil
	}
	var out []string
	for _, tier := range st.Trackers {
		out = append(out, tier...)
	}
	return out
}
