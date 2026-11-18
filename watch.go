package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// watchLoop polls the configured watch folder and auto-adds .torrent/.nzb
// files dropped into it. Added files are moved to <dir>/.added/.
func (a *App) watchLoop(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			dir := a.watchDir()
			if dir == "" {
				continue
			}
			if err := ensureDir(dir); err != nil {
				continue
			}
			a.scanWatchDir(dir, filepath.Join(dir, ".added"))
		}
	}
}

func (a *App) scanWatchDir(dir, addedDir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		if en.IsDir() || strings.HasPrefix(en.Name(), ".") {
			continue
		}
		low := strings.ToLower(en.Name())
		isTorrent := strings.HasSuffix(low, ".torrent")
		isNZB := strings.HasSuffix(low, ".nzb")
		if !isTorrent && !isNZB {
			continue
		}
		path := filepath.Join(dir, en.Name())
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 || len(data) > 256<<20 {
			continue
		}
		var addErr error
		if isTorrent {
			_, addErr = a.Torrent.AddTorrent(AddInput{
				TorrentB64: base64.StdEncoding.EncodeToString(data),
				Name:       strings.TrimSuffix(en.Name(), filepath.Ext(en.Name())),
			})
		} else {
			_, addErr = a.NZB.ParseAndAdd(data, strings.TrimSuffix(en.Name(), ".nzb"), "")
		}
		if addErr != nil {
			slog.Warn("watch folder: failed to add", "file", en.Name(), "err", addErr)
			continue
		}
		// Move aside so it isn't re-added.
		_ = ensureDir(addedDir)
		_ = os.Rename(path, filepath.Join(addedDir, en.Name()))
		slog.Info("watch folder: added", "file", en.Name())
	}
}
