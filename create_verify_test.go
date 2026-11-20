package main

import (
	"bytes"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestMultiFileTorrentValid(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.bin"), bytes.Repeat([]byte("a"), 300000), 0o644)
	os.WriteFile(filepath.Join(dir, "sub/b.bin"), bytes.Repeat([]byte("b"), 150000), 0o644)
	out := filepath.Join(t.TempDir(), "x.torrent")
	if err := createTorrentFile(dir, out, nil); err != nil {
		t.Fatal(err)
	}
	mi, err := metainfo.LoadFromFile(out)
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Pieces)%20 != 0 || len(info.Pieces) == 0 {
		t.Fatalf("info.pieces missing/bad: %d bytes", len(info.Pieces))
	}
	if len(info.Files) != 2 {
		t.Fatalf("files=%v", info.Files)
	}
	// Verify piece hashes against concatenated stream
	plen := info.PieceLength
	var concat []byte
	for _, f := range info.Files {
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		concat = append(concat, b...)
	}
	for off := 0; off < len(concat); off += int(plen) {
		end := off + int(plen)
		if end > len(concat) {
			end = len(concat)
		}
		want := sha1.Sum(concat[off:end])
		got := info.Pieces[off/int(plen)*20 : off/int(plen)*20+20]
		if !bytes.Equal(want[:], got) {
			t.Fatalf("piece %d hash mismatch", off/int(plen))
		}
	}
}
