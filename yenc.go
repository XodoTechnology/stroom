package main

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
)

// yEncResult is the outcome of decoding one article body.
type yEncResult struct {
	Data         []byte // decoded bytes (== input when !IsYEnc)
	IsYEnc       bool
	CRCOK        bool  // false only when a crc32 attr existed and mismatched
	DeclaredSize int64 // size= attr from =ybegin, -1 if absent
}

// decodeYEnc decodes a yEnc-encoded article body.
// Non-yEnc bodies return IsYEnc=false with Data=input (stored raw).
func decodeYEnc(body []byte) (*yEncResult, error) {
	idx := indexYEncBegin(body)
	if idx < 0 {
		return &yEncResult{Data: body, IsYEnc: false, CRCOK: true, DeclaredSize: -1}, nil
	}
	lineEnd := bytes.IndexByte(body[idx:], '\n')
	if lineEnd < 0 {
		return nil, fmt.Errorf("yenc: unterminated =ybegin")
	}
	head := string(body[idx : idx+lineEnd])
	res := &yEncResult{IsYEnc: true, CRCOK: true, DeclaredSize: -1}
	if v, ok := yencAttr(head, "size"); ok {
		res.DeclaredSize, _ = strconv.ParseInt(v, 10, 64)
	}
	dataStart := idx + lineEnd + 1

	// Bound decode region at =yend (also captures crc32 attr).
	end := len(body)
	var yendLine string
	if yend := indexYEncKeyword(body[dataStart:], "=yend"); yend >= 0 {
		end = dataStart + yend
		le := bytes.IndexByte(body[end:], '\n')
		if le >= 0 {
			yendLine = string(body[end : end+le])
		}
	}

	// Multi-part posts: =ybegin's size= is the WHOLE file's size; the part's
	// own range is on =ypart begin=X end=Y (1-based, inclusive). Scan the
	// leading control lines of the region for it.
	region := body[dataStart:end]
	partBegin, partEnd, multi := int64(-1), int64(-1), false
	for scan := region; len(scan) > 0; {
		nl := bytes.IndexByte(scan, '\n')
		var line []byte
		if nl < 0 {
			line = scan
		} else {
			line = scan[:nl]
		}
		if bytes.HasPrefix(line, []byte("=ypart")) {
			if v, ok := yencAttr(string(line), "begin"); ok {
				partBegin, _ = strconv.ParseInt(v, 10, 64)
			}
			if v, ok := yencAttr(string(line), "end"); ok {
				partEnd, _ = strconv.ParseInt(v, 10, 64)
			}
			multi = true
			break
		}
		if !isYEncControlLine(line) || nl < 0 {
			break // data (or EOF) reached: no =ypart present
		}
		scan = scan[nl+1:]
	}

	// Decode the region as a byte stream. Raw \n / \r\n are line
	// terminators (encoded LF/CR are always escaped, so they never appear
	// raw). '=' escapes may legally contain \n or \r as the second byte,
	// so we can't decode strictly line-by-line. =y* keywords are only
	// recognized at line starts and skipped.
	out := make([]byte, 0, len(region))
	out = appendYEncStream(out, region)
	res.Data = out

	// Expected decoded size: the part range for multi-part posts (tolerate
	// 0-based begin encoders), else the =ybegin size= for single-part ones.
	want := res.DeclaredSize
	if multi && partEnd > partBegin && partBegin >= 0 {
		want = partEnd - partBegin + 1
	}
	if want >= 0 && int64(len(out)) != want && !(multi && int64(len(out)) == want-1) {
		// Size mismatch → treat as corrupt; caller will retry.
		return res, fmt.Errorf("yenc size mismatch: got %d want %d", len(out), want)
	}
	if yendLine != "" {
		// pcrc32 = CRC of THIS part (multi-part posts). crc32 on =yend of a
		// multi-part post is the whole file's CRC — comparing it against one
		// part's bytes would always mismatch, so only honour it for
		// single-part articles.
		crcStr, hasCRC := yencAttr(yendLine, "pcrc32")
		if !hasCRC && !multi {
			crcStr, hasCRC = yencAttr(yendLine, "crc32")
		}
		if hasCRC {
			if w, err := strconv.ParseUint(crcStr, 16, 32); err == nil {
				got := crc32.ChecksumIEEE(out)
				res.CRCOK = uint32(w) == got
				if !res.CRCOK {
					return res, fmt.Errorf("yenc crc32 mismatch")
				}
			}
		}
	}
	return res, nil
}

// appendYEncStream decodes a yEnc data region byte-by-byte.
//   - '=' + next byte: escape, emit next-106 (mod 256). The next byte may be
//     \n or \r — it is consumed literally, not treated as a line ending.
//   - \r\n or \n: line terminator, skipped.
//   - At line start, "=ybegin"/"=ypart"/"=yend"/"=yenc" lines are skipped.
//   - Everything else: emit byte-42 (mod 256).
func appendYEncStream(out []byte, region []byte) []byte {
	lineStart := true
	for i := 0; i < len(region); {
		// At a line start, check for control keywords and skip the line.
		if lineStart && region[i] == '=' {
			if isYEncControlAt(region, i) {
				// skip to next \n
				nl := bytes.IndexByte(region[i:], '\n')
				if nl < 0 {
					return out
				}
				i += nl + 1
				lineStart = true
				continue
			}
		}
		b := region[i]
		switch {
		case b == '\n':
			i++
			lineStart = true
			continue
		case b == '\r':
			// \r\n or lone \r: skip terminator.
			i++
			if i < len(region) && region[i] == '\n' {
				i++
			}
			lineStart = true
			continue
		case b == '=' && i+1 < len(region):
			// Escape: operand is the next byte verbatim (may be \n/\r).
			out = append(out, byte(int(region[i+1])-106))
			i += 2
			lineStart = false
			continue
		default:
			out = append(out, byte(int(b)-42))
			i++
			lineStart = false
		}
	}
	return out
}

// isYEncControlAt reports whether a yEnc control keyword starts at pos.
func isYEncControlAt(b []byte, pos int) bool {
	for _, kw := range []string{"=ybegin", "=ypart", "=yend", "=yenc"} {
		if pos+len(kw) > len(b) {
			continue
		}
		if string(b[pos:pos+len(kw)]) != kw {
			continue
		}
		// keyword must be followed by EOL or a space/=attr
		if pos+len(kw) == len(b) {
			return true
		}
		c := b[pos+len(kw)]
		if c == ' ' || c == '=' || c == '\t' || c == '\r' || c == '\n' {
			return true
		}
	}
	return false
}

// isYEncControlLine reports whether a line (at col 0) is a yEnc keyword.
func isYEncControlLine(line []byte) bool {
	if len(line) < 2 || line[0] != '=' || line[1] != 'y' {
		return false
	}
	for _, kw := range []string{"=ybegin", "=ypart", "=yend", "=yenc"} {
		if len(line) >= len(kw) && string(line[:len(kw)]) == kw {
			if len(line) == len(kw) {
				return true
			}
			c := line[len(kw)]
			if c == ' ' || c == '=' || c == '\t' {
				return true
			}
		}
	}
	return false
}

// indexYEncBegin finds "=ybegin" at a line start within the first 8KB.
func indexYEncBegin(body []byte) int {
	return indexYEncKeywordLimit(body, "=ybegin", 8<<10)
}

func indexYEncKeyword(body []byte, kw string) int {
	return indexYEncKeywordLimit(body, kw, len(body))
}

func indexYEncKeywordLimit(body []byte, kw string, limit int) int {
	if limit > len(body) {
		limit = len(body)
	}
	kb := []byte(kw)
	for i := 0; i+len(kb) <= limit; i++ {
		if i > 0 && body[i-1] != '\n' {
			continue // keyword must start a line
		}
		if bytes.Equal(body[i:i+len(kb)], kb) {
			return i
		}
	}
	return -1
}

// yencAttr extracts a key="value" or key=value attribute from a header line.
func yencAttr(line, key string) (string, bool) {
	idx := strings.Index(line, key+"=")
	if idx < 0 {
		return "", false
	}
	rest := line[idx+len(key)+1:]
	if strings.HasPrefix(rest, "\"") {
		rest = rest[1:]
		end := strings.Index(rest, "\"")
		if end < 0 {
			return "", false
		}
		return rest[:end], true
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		return rest, true
	}
	return rest[:end], true
}
