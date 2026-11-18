package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNZBEndToEnd runs a full NZB job against the fake NNTP server:
// parse → queue → fetch yEnc articles → assemble file on disk.
func TestNZBEndToEnd(t *testing.T) {
	// Payload split across two segments.
	part1 := []byte("the quick brown fox jumps over the lazy dog. ")
	part2 := []byte("pack my box with five dozen liquor jugs!!")

	articles := map[string]string{
		"seg1@news": string(encodeYEncSpec(part1, 1, 2)),
		"seg2@news": string(encodeYEncSpec(part2, 2, 2)),
	}
	fake := startFakeNNTP(t, articles, "", "")
	host, portStr, _ := net.SplitHostPort(fake.addr())

	cfg := testConfig(t.TempDir(), 0)
	cfg.NNTPServers = []NNTPServer{{
		ID: "s1", Name: "fake", Host: host, Port: mustAtoi(portStr),
		Connections: 2,
	}}
	app := &App{DataDir: t.TempDir(), Config: cfg, version: "test"}
	eng, err := NewNZBEngine(app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)

	nzb := fmt.Sprintf(`<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="a@b" date="1" subject="[1/1] - &quot;e2e.txt&quot; yEnc (1/1)" bytes="%d">
  <segments>
   <segment bytes="%d" number="1"><nzb>seg1@news</nzb></segment>
   <segment bytes="%d" number="2"><nzb>seg2@news</nzb></segment>
  </segments>
 </file>
</nzb>`, len(part1)+len(part2), len(part1), len(part2))

	id, err := eng.ParseAndAdd([]byte(nzb), "e2e-test", "s1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// Poll for completion.
	deadline := time.Now().Add(20 * time.Second)
	var j *NZBJob
	for time.Now().Before(deadline) {
		j = eng.find(id)
		if j != nil {
			j.mu.Lock()
			st := j.Status
			j.mu.Unlock()
			if st == "done" || st == "error" {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	j = eng.find(id)
	if j == nil {
		t.Fatal("job vanished")
	}
	j.mu.Lock()
	st, jerr := j.Status, j.Error
	j.mu.Unlock()
	if st != "done" {
		t.Fatalf("job status=%s err=%s", st, jerr)
	}

	// File should be assembled on disk.
	p, ok := eng.FileDiskPath(id, "e2e.txt")
	if !ok {
		t.Fatal("FileDiskPath failed")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read assembled file: %v (path %s)", err, p)
	}
	want := append(append([]byte{}, part1...), part2...)
	if string(got) != string(want) {
		t.Fatalf("content mismatch:\n got %q\nwant %q", got, want)
	}
	t.Logf("assembled %s (%d bytes)", filepath.Base(p), len(got))
}
