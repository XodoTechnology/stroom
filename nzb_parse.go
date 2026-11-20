package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// NZBSegment is one article reference inside an NZB file element.
type NZBSegment struct {
	Number    int    `xml:"number,attr" json:"n"`
	Bytes     int64  `xml:"bytes,attr" json:"b,omitempty"`
	// NZB spec: <segment bytes="N" number="N">message-id@host</segment>
	// — the id is the element's character data, not a child element.
	MessageID string `xml:",chardata" json:"id"`
}

// NZBFileElement is a <file> element in the NZB.
type NZBFileElement struct {
	Poster   string       `xml:"poster,attr"`
	Date     string       `xml:"date,attr"`
	Subject  string       `xml:"subject,attr"`
	Bytes    int64        `xml:"bytes,attr"`
	Segments []NZBSegment `xml:"segments>segment"`
}

type nzbXML struct {
	XMLName xml.Name         `xml:"nzb"`
	Files   []NZBFileElement `xml:"file"`
}

// NZBFile is a parsed, ready-to-download file inside an NZB.
type NZBFile struct {
	Name       string       // output filename (sanitized, unique in job)
	Subject    string       // original subject
	TotalBytes int64        // expected decoded size (best effort)
	Segments   []NZBSegment // sorted by Number
}

// NZBDoc is a parsed NZB.
type NZBDoc struct {
	Name  string // suggested job name
	Files []*NZBFile
}

var reQuoted = regexp.MustCompile(`"([^"]+)"`)
var reYencName = regexp.MustCompile(`(?i)yenc\s*\(`)

// ParseNZB parses .nzb XML (plain or gzipped).
func ParseNZB(data []byte) (*NZBDoc, error) {
	// Sniff gzip.
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		data, err = io.ReadAll(io.LimitReader(zr, 256<<20))
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
	}
	// Some NZBs have a leading BOM / junk; find the <nzb root.
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("<")) {
		if i := bytes.Index(trimmed, []byte("<nzb")); i >= 0 {
			trimmed = trimmed[i:]
		}
	}
	var doc nzbXML
	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	// NZBs often declare iso-8859-1; pass bytes through (ASCII-safe).
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("nzb xml: %w", err)
	}
	if len(doc.Files) == 0 {
		return nil, fmt.Errorf("nzb contains no files")
	}

	out := &NZBDoc{}
	usedNames := map[string]int{}
	for i := range doc.Files {
		fe := &doc.Files[i]
		if len(fe.Segments) == 0 {
			continue
		}
		nf := &NZBFile{
			Name:     nzbFileName(fe.Subject, i),
			Subject:  fe.Subject,
			Segments: append([]NZBSegment{}, fe.Segments...),
		}
		// Sort segments by part number, then dedup: same number from
		// different posters carries identical data.
		sortSegments(nf.Segments)
		dedup := nf.Segments[:0]
		for i, s := range nf.Segments {
			if i > 0 && s.Number == nf.Segments[i-1].Number {
				continue
			}
			dedup = append(dedup, s)
		}
		nf.Segments = dedup
		var segSum int64
		for _, s := range nf.Segments {
			segSum += s.Bytes
		}
		if fe.Bytes > 0 {
			nf.TotalBytes = fe.Bytes
		} else {
			nf.TotalBytes = segSum
		}
		// Unique-ify name within job (base-name occurrence count).
		base := nf.Name
		occ := usedNames[base]
		usedNames[base] = occ + 1
		if occ > 0 {
			ext := fileExt(base)
			stem := strings.TrimSuffix(base, ext)
			nf.Name = fmt.Sprintf("%s (%d)%s", stem, occ, ext)
		}
		out.Files = append(out.Files, nf)
	}
	if len(out.Files) == 0 {
		return nil, fmt.Errorf("nzb contains no usable files")
	}
	// Job name: from first file subject or count.
	out.Name = fmt.Sprintf("NZB %d files", len(out.Files))
	if len(out.Files) == 1 {
		out.Name = out.Files[0].Name
	}
	return out, nil
}

func sortSegments(s []NZBSegment) {
	// insertion-free simple sort; segments are usually already ordered
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Number < s[j-1].Number; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// nzbFileName extracts a filename from an NZB <file subject>.
func nzbFileName(subject string, idx int) string {
	// Typical: [12/34] - "real.name.ext" yEnc (1/1)
	if m := reQuoted.FindStringSubmatch(subject); m != nil {
		name := sanitizeName(m[1])
		if name != "" && name != "." {
			return name
		}
	}
	// Fallback: last token that looks like a filename.
	fields := strings.Fields(subject)
	for i := len(fields) - 1; i >= 0; i-- {
		f := strings.Trim(fields[i], "\"'()[]")
		if strings.Contains(f, ".") && len(f) > 4 && !strings.Contains(f, "=") {
			return sanitizeName(f)
		}
	}
	return fmt.Sprintf("file-%03d.bin", idx+1)
}

func fileExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i:]
}

var _ = strconv.Itoa
var _ = reYencName
