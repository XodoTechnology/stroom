package main

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

const testNZB = `<?xml version="1.0" encoding="iso-8859-1" ?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="a@b" date="123" subject="[1/1] - &quot;movie.mkv&quot; yEnc (1/1)" bytes="1000">
  <groups><group>alt.binaries</group></groups>
  <segments>
   <segment bytes="500" number="1">msg1@news</segment>
   <segment bytes="500" number="2">msg2@news</segment>
   <segment bytes="500" number="2">msg2dup@news</segment>
   <segment bytes="500" number="1">msg1dup@news</segment>
  </segments>
 </file>
 <file poster="a@b" date="123" subject="[1/1] - &quot;subs.srt&quot; yEnc (1/1)" bytes="200">
  <segments>
   <segment bytes="200" number="1">msg3@news</segment>
  </segments>
 </file>
</nzb>`

func TestParseNZB(t *testing.T) {
	doc, err := ParseNZB([]byte(testNZB))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Files) != 2 {
		t.Fatalf("want 2 files got %d", len(doc.Files))
	}
	f0 := doc.Files[0]
	if f0.Name != "movie.mkv" {
		t.Fatalf("name: %q", f0.Name)
	}
	if f0.TotalBytes != 1000 {
		t.Fatalf("bytes: %d", f0.TotalBytes)
	}
	if len(f0.Segments) != 2 {
		t.Fatalf("want 2 deduped segments got %d", len(f0.Segments))
	}
	if f0.Segments[0].Number != 1 || f0.Segments[1].Number != 2 {
		t.Fatalf("segments not sorted/deduped: %+v", f0.Segments)
	}
	if f0.Segments[0].MessageID != "msg1@news" {
		t.Fatalf("message-id not parsed from chardata: %q", f0.Segments[0].MessageID)
	}
	if doc.Files[1].Name != "subs.srt" {
		t.Fatalf("file2 name: %q", doc.Files[1].Name)
	}
}

func TestParseNZBDuplicateNames(t *testing.T) {
	nzb := strings.Replace(testNZB, "subs.srt", "movie.mkv", 1)
	doc, err := ParseNZB([]byte(nzb))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Files[1].Name == doc.Files[0].Name {
		t.Fatalf("duplicate names not uniquified: %q", doc.Files[1].Name)
	}
}

func TestParseNZBGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(testNZB))
	zw.Close()
	doc, err := ParseNZB(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Files) != 2 {
		t.Fatal("gzip parse failed")
	}
}

func TestParseNZBBad(t *testing.T) {
	if _, err := ParseNZB([]byte("not xml at all")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ParseNZB([]byte("<nzb></nzb>")); err == nil {
		t.Fatal("expected no-files error")
	}
}
