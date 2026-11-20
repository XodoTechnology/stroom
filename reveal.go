package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealInFileManager selects a file inside its containing folder without
// executing it (untrusted downloads must never be run via the OS handler).
func revealInFileManager(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer.exe", "/select,"+path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	_ = cmd.Start()
}

// openInFileManager opens a path with the OS default handler
// (file manager for dirs, default app for files).
func openInFileManager(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}
