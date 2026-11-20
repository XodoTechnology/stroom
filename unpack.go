package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Post-download archive unpacking for NZB jobs. After download (+ optional
// par2 repair), split RAR/7z/ZIP sets common on Usenet are extracted in
// place when a suitable tool is on PATH. Archive volumes are removed only
// after a successful extraction.

var archiveRe = map[string]bool{
	".rar": true, ".zip": true, ".7z": true,
	".001": true, // first volume of split 7z/rar sets
}

// unpackIfNeeded extracts any archive set found in dir. Returns a status
// string for the job error field, or "" when nothing was needed/succeeded.
func unpackIfNeeded(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var first, part string
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		n := strings.ToLower(en.Name())
		ext := filepath.Ext(n)
		switch {
		case strings.HasSuffix(n, ".part1.rar") || strings.HasSuffix(n, ".part01.rar") ||
			strings.HasSuffix(n, ".7z.001") || strings.HasSuffix(n, ".zip.001"):
			if part == "" {
				part = en.Name()
			}
		case archiveRe[ext] && !strings.Contains(n, ".vol") && first == "":
			first = en.Name()
		}
	}
	target := part
	if target == "" {
		target = first
	}
	if target == "" {
		return ""
	}
	tool, args := unpackTool()
	if tool == "" {
		return "archive present but no unrar/7z/unzip on PATH"
	}
	full := filepath.Join(dir, target)
	cmd := exec.Command(tool, append(args, full)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unpack failed: " + strings.TrimSpace(lastLines(string(out), 3))
	}
	// Extraction succeeded — drop archive volumes + par2 leftovers.
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		n := strings.ToLower(en.Name())
		if isArchiveVolume(n) || strings.HasSuffix(n, ".par2") {
			_ = os.Remove(filepath.Join(dir, en.Name()))
		}
	}
	return ""
}

func unpackTool() (string, []string) {
	for _, c := range []struct {
		bin  string
		args []string
	}{
		{"unrar", []string{"x", "-o+", "-idq"}},
		{"7z", []string{"x", "-y"}},
		{"7zz", []string{"x", "-y"}},
		{"unzip", []string{"-o", "-q"}},
	} {
		if p, err := exec.LookPath(c.bin); err == nil {
			return p, c.args
		}
	}
	return "", nil
}

func isArchiveVolume(n string) bool {
	ext := filepath.Ext(n)
	if archiveRe[ext] {
		return true
	}
	// .r00-.r99, .z01-.z99, .7z.NNN volumes
	for _, pre := range []string{".r", ".s", ".z", ".7z."} {
		if strings.Contains(n, pre) {
			tail := n[strings.LastIndex(n, "."):]
			if len(tail) == 4 || strings.HasSuffix(pre, ".") {
				return true
			}
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}
