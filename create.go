package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---- minimal bencode encoder ----

type ben interface{}
type bstr string
type bint int64
type blist []ben
type bdict map[string]ben

func bencode(v ben) []byte {
	var out []byte
	writeBen(&out, v)
	return out
}

func writeBen(out *[]byte, v ben) {
	switch t := v.(type) {
	case bstr:
		*out = append(*out, []byte(fmt.Sprintf("%d:", len(t)))...)
		*out = append(*out, []byte(t)...)
	case string:
		writeBen(out, bstr(t))
	case bint:
		*out = append(*out, []byte(fmt.Sprintf("i%de", t))...)
	case int:
		writeBen(out, bint(t))
	case int64:
		writeBen(out, bint(t))
	case blist:
		*out = append(*out, 'l')
		for _, e := range t {
			writeBen(out, e)
		}
		*out = append(*out, 'e')
	case bdict:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		*out = append(*out, 'd')
		for _, k := range keys {
			writeBen(out, bstr(k))
			writeBen(out, t[k])
		}
		*out = append(*out, 'e')
	default:
		panic(fmt.Sprintf("bencode: unsupported %T", v))
	}
}

// ---- torrent creation ----

func pieceLengthFor(total int64) int64 {
	switch {
	case total < 1<<20:
		return 16 << 10
	case total < 50<<20:
		return 128 << 10
	case total < 1<<30:
		return 256 << 10
	case total < 10<<30:
		return 1 << 20
	case total < 100<<30:
		return 2 << 20
	default:
		return 4 << 20
	}
}

func hashPieces(path string, size, plen int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	npieces := (size + plen - 1) / plen
	hashes := make([]byte, 0, npieces*20)
	buf := make([]byte, plen)
	for i := int64(0); i < npieces; i++ {
		want := plen
		if rem := size - i*plen; rem < plen {
			want = rem
		}
		h := sha1.New()
		read := int64(0)
		for read < want {
			n, err := f.Read(buf[:minInt64(want-read, int64(len(buf)))])
			if n > 0 {
				h.Write(buf[:n])
				read += int64(n)
			}
			if err != nil {
				break
			}
			if n == 0 {
				break
			}
		}
		hashes = append(hashes, h.Sum(nil)...)
	}
	return hashes, nil
}

// hashPiecesMulti hashes piece boundaries over the concatenated byte stream
// of all files in order, per BEP 3 multi-file semantics.
func hashPiecesMulti(files []fileEntry, plen int64) ([]byte, error) {
	h := sha1.New()
	var hashes []byte
	buf := make([]byte, 0, plen)
	chunk := make([]byte, 64<<10)
	flush := func() {
		if len(buf) > 0 {
			h.Reset()
			h.Write(buf)
			hashes = append(hashes, h.Sum(nil)...)
			buf = buf[:0]
		}
	}
	for _, fe := range files {
		f, err := os.Open(fe.abs)
		if err != nil {
			return nil, err
		}
		for {
			n, rerr := f.Read(chunk)
			data := chunk[:n]
			for len(data) > 0 {
				need := int(plen) - len(buf)
				take := n
				if len(data) < take {
					take = len(data)
				}
				if take > need {
					take = need
				}
				buf = append(buf, data[:take]...)
				data = data[take:]
				if int64(len(buf)) == plen {
					flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		f.Close()
	}
	flush()
	return hashes, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type fileEntry struct {
	rel  string // slash-separated path relative to torrent root
	abs  string
	size int64
}

func collectFiles(root string) (name string, files []fileEntry, total int64, err error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", nil, 0, err
	}
	if !st.IsDir() {
		return filepath.Base(root), []fileEntry{{rel: filepath.Base(root), abs: root, size: st.Size()}}, st.Size(), nil
	}
	name = filepath.Base(filepath.Clean(root))
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{rel: filepath.ToSlash(rel), abs: p, size: info.Size()})
		total += info.Size()
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return name, files, total, err
}

func createTorrentFile(srcPath, outPath string, trackers []string) error {
	name, files, total, err := collectFiles(srcPath)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("nothing to pack: %s", srcPath)
	}
	plen := pieceLengthFor(total)

	var info bdict
	if len(files) == 1 {
		fe := files[0]
		hashes, err := hashPieces(fe.abs, fe.size, plen)
		if err != nil {
			return err
		}
		info = bdict{
			"length":       bint(fe.size),
			"name":         bstr(name),
			"pieces":       bstr(hashes),
			"piece length": bint(plen),
		}
	} else {
		// BEP 3: one "pieces" string over the concatenated stream of all
		// files; each file entry carries "path" (list of components).
		hashes, err := hashPiecesMulti(files, plen)
		if err != nil {
			return err
		}
		var flist blist
		for _, fe := range files {
			var parts blist
			for _, p := range strings.Split(fe.rel, "/") {
				parts = append(parts, bstr(p))
			}
			flist = append(flist, bdict{
				"length":     bint(fe.size),
				"path":       parts,
				"path.utf-8": parts,
			})
		}
		info = bdict{
			"name":         bstr(name),
			"piece length": bint(plen),
			"pieces":       bstr(hashes),
			"files":        flist,
		}
	}

	top := bdict{"info": info}
	if len(trackers) > 0 {
		var tl blist
		for _, t := range trackers {
			tl = append(tl, bstr(t))
		}
		top["announce"] = bstr(trackers[0])
		top["announce-list"] = blist{tl}
	}
	ih := sha1.Sum(bencode(info))
	if outPath == "" {
		outPath = sanitizeName(name) + "-" + hex.EncodeToString(ih[:8]) + ".torrent"
	}
	if err := os.WriteFile(outPath, bencode(top), 0o644); err != nil {
		return err
	}
	logOut("created %s (%d files, %.1f MiB, %d pieces)", outPath, len(files),
		float64(total)/float64(1<<20), (total+plen-1)/plen)
	return nil
}
