package main

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

func testConfig(dir string, port int) Config {
	return Config{
		ListenAddr:        "127.0.0.1:0",
		DownloadDir:       dir,
		BTListenPort:      port,
		EnableDHT:         false,
		EnableUPnP:        false,
		EnablePEX:         true,
		NZBMaxConnections: 4,
	}
}

func newTestEngine(t *testing.T, dataDir, dlDir string, port int) *TorrentEngine {
	t.Helper()
	app := &App{DataDir: dataDir, Config: testConfig(dlDir, port), version: "test"}
	e, err := NewTorrentEngine(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// TestTorrentSeedLeech does a real localhost swarm: engine A seeds a file,
// engine B downloads it via a direct peer add.
func TestTorrentSeedLeech(t *testing.T) {
	root := t.TempDir()

	// 1. Source payload (~600KB → several pieces).
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 600*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "test.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Create .torrent.
	torPath := filepath.Join(root, "test.torrent")
	if err := createTorrentFile(srcFile, torPath, nil); err != nil {
		t.Fatal(err)
	}
	torBytes, err := os.ReadFile(torPath)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(torBytes)

	// 3. Pre-populate seeder storage: dir = <dl>/torrents/<torrent name>.
	// The engine's TorrentDirMaker uses the sanitized torrent name.
	seedData := filepath.Join(root, "seeddata")
	seedDir := filepath.Join(seedData, "torrents", "test.bin")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "test.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Seeder engine.
	seed := newTestEngine(t, filepath.Join(root, "seedstate"), seedData, 16961)
	ih, err := seed.AddTorrentBytes(torBytes)
	if err != nil {
		t.Fatalf("seed add: %v", err)
	}

	// Find seeder TCP addr.
	var seedAddr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tt, _, err := seed.find(ih); err == nil && tt != nil {
			for _, a := range seed.client.ListenAddrs() {
				seedAddr = a.String()
				if h, p, err := net.SplitHostPort(seedAddr); err == nil && (h == "0.0.0.0" || h == "::") {
					seedAddr = net.JoinHostPort("127.0.0.1", p)
				}
				break
			}
			if seedAddr != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seedAddr == "" {
		t.Fatal("no seeder listen addr")
	}
	// Wait until the seeder's initial scan marks all pieces, otherwise it
	// rejects incoming conns (haveAnyPieces == false).
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if tt, _, err := seed.find(ih); err == nil {
			if tt.Stats().PiecesComplete >= 38 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("seeder at %s, torrent %s", seedAddr, ih)

	// 5. Leecher engine.
	leechData := filepath.Join(root, "leechdata")
	leech := newTestEngine(t, filepath.Join(root, "leechstate"), leechData, 16962)
	lih, err := leech.AddTorrentBytes(torBytes)
	if err != nil {
		t.Fatalf("leech add: %v", err)
	}
	if lih != ih {
		t.Fatalf("infohash mismatch %s vs %s", lih, ih)
	}
	// Inject peer once torrent object exists.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tt, _, err := leech.find(lih)
		if err == nil && tt != nil {
			tt.AddPeers([]torrent.PeerInfo{{Addr: peerAddr(seedAddr)}})
			tt.DownloadAll()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 6. Wait for completion.
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		jobs := leech.List()
		if len(jobs) == 1 && jobs[0].Done >= jobs[0].Size && jobs[0].Size > 0 {
			t.Logf("downloaded %d/%d bytes", jobs[0].Done, jobs[0].Size)
			break
		}
		if lt, _, err := leech.find(lih); err == nil {
			lst := lt.Stats()
			t.Logf("leech: done=%d size=%d pieces=%d peers=%d down=%d", jobs[0].Done, jobs[0].Size, lst.PiecesComplete, lst.TotalPeers, jobs[0].DownSpeed)
		}
		if st2, _, err := seed.find(ih); err == nil {
			sst := st2.Stats()
			t.Logf("seed: pieces=%d peers=%d", sst.PiecesComplete, sst.TotalPeers)
		}
		time.Sleep(2 * time.Second)
	}
	jobs := leech.List()
	if len(jobs) != 1 {
		t.Fatalf("jobs: %+v", jobs)
	}
	j := jobs[0]
	if j.Done < j.Size {
		t.Fatalf("incomplete: %d/%d status=%s err=%s", j.Done, j.Size, j.Status, j.Error)
	}
	// Verify file on disk.
	lp, ok := leech.FileDiskPath(lih, "test.bin")
	if !ok {
		t.Fatal("no leech file path")
	}
	got, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("size mismatch %d vs %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("content mismatch at %d", i)
		}
	}
	_ = b64
}

// peerAddr wraps a "host:port" string as a PeerRemoteAddr.
type peerAddr string

func (p peerAddr) String() string { return string(p) }
