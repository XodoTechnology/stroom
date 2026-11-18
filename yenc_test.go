package main

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"testing"
)

// encodeYEncSpec produces a spec-correct yEnc body for tests:
// encoded octet = (b+42)%256; critical octets escaped as '='+(e+64)%256.
func encodeYEncSpec(data []byte, part, parts int) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "=ybegin line=128 size=%d name=test.bin\n", len(data))
	if parts > 1 {
		fmt.Fprintf(&out, "=ypart begin=1 end=%d\n", len(data))
	}
	line := make([]byte, 0, 128)
	flush := func() {
		if len(line) > 0 {
			out.Write(line)
			out.WriteByte('\n')
			line = line[:0]
		}
	}
	for _, b := range data {
		e := byte(int(b) + 42)
		if e == '=' || e == 0 || e == '\n' || e == '\r' || e < 32 || e > 126 {
			line = append(line, '=')
			line = append(line, byte(int(e)+64))
		} else {
			line = append(line, e)
		}
		if len(line) >= 128 {
			flush()
		}
	}
	flush()
	crc := crc32.ChecksumIEEE(data)
	fmt.Fprintf(&out, "=yend size=%d part=%d/%d crc32=%08X\n", len(data), part, parts, crc)
	return out.Bytes()
}

func TestYEncRoundTrip(t *testing.T) {
	// Data exercising escapes: bytes that map to '=', NUL, LF, CR, >126.
	data := []byte{0, 1, 19, 42, 61, 100, 200, 255, 'h', 'e', 'l', 'l', 'o', '\n', '\r', 0x3d}
	// Add byte values whose encoded form hits critical chars: d=19→e=61('=')
	for i := 0; i < 256; i++ {
		data = append(data, byte(i))
	}
	body := encodeYEncSpec(data, 1, 1)
	res, err := decodeYEnc(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.IsYEnc {
		t.Fatal("expected IsYEnc")
	}
	if !res.CRCOK {
		t.Fatal("CRC mismatch")
	}
	if !bytes.Equal(res.Data, data) {
		t.Fatalf("data mismatch: got %d bytes want %d", len(res.Data), len(data))
	}
}

func TestYEncCorruptCRC(t *testing.T) {
	data := []byte("hello world, this is a yenc test payload!!")
	body := encodeYEncSpec(data, 1, 1)
	// Flip one hex digit of the real crc32 value.
	crc := fmt.Sprintf("%08X", crc32.ChecksumIEEE(data))
	bad := "0" + crc[1:]
	if bad == crc {
		bad = "1" + crc[1:]
	}
	body = bytes.Replace(body, []byte(crc), []byte(bad), 1)
	res, err := decodeYEnc(body)
	if err == nil {
		t.Fatal("expected crc mismatch error")
	}
	if res == nil || res.CRCOK {
		t.Fatal("expected CRCOK=false")
	}
}

func TestYEncCorruptSize(t *testing.T) {
	data := []byte("size test")
	body := encodeYEncSpec(data, 1, 1)
	body = bytes.Replace(body, []byte(fmt.Sprintf("size=%d", len(data))), []byte("size=999"), 1)
	// First occurrence is in =ybegin; also in =yend — replace all.
	_, err := decodeYEnc(body)
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
}

func TestYEncNonYEncPassthrough(t *testing.T) {
	raw := []byte("this is not yenc at all\njust bytes\n")
	res, err := decodeYEnc(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsYEnc {
		t.Fatal("should not be yenc")
	}
	if !bytes.Equal(res.Data, raw) {
		t.Fatal("passthrough data changed")
	}
}

func TestYEncMultipart(t *testing.T) {
	part1 := []byte("first part of the file ")
	part2 := []byte("second part!")
	b1 := encodeYEncSpec(part1, 1, 2)
	b2 := encodeYEncSpec(part2, 2, 2)
	r1, err := decodeYEnc(b1)
	if err != nil || !r1.IsYEnc {
		t.Fatalf("part1: %v", err)
	}
	r2, err := decodeYEnc(b2)
	if err != nil || !r2.IsYEnc {
		t.Fatalf("part2: %v", err)
	}
	if !bytes.Equal(append(r1.Data, r2.Data...), append(part1, part2...)) {
		t.Fatal("multipart reassembly mismatch")
	}
}
