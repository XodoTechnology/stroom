//go:build windows

package main

import (
	"os"
	"syscall"
)

// attachParentConsole reconnects stdin/stdout/stderr to the parent console.
// The binary is built with -H windowsgui so Explorer/shortcut launches show
// no console window; this restores CLI output for `stroom add|update|version`.
func attachParentConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS = (DWORD)-1
	ret, _, _ := kernel32.NewProc("AttachConsole").Call(attachParentProcess)
	if ret == 0 {
		return // no parent console (launched from Explorer/shortcut)
	}
	getStd := kernel32.NewProc("GetStdHandle")
	for _, h := range []struct {
		id   uintptr
		name string
		dst  **os.File
	}{
		{^uintptr(9), "stdin", &os.Stdin},    // STD_INPUT_HANDLE  = -10
		{^uintptr(10), "stdout", &os.Stdout}, // STD_OUTPUT_HANDLE = -11
		{^uintptr(11), "stderr", &os.Stderr}, // STD_ERROR_HANDLE  = -12
	} {
		v, _, _ := getStd.Call(h.id)
		if v == 0 || v == ^uintptr(0) { // NULL or INVALID_HANDLE_VALUE
			continue
		}
		if f := os.NewFile(v, h.name); f != nil {
			*h.dst = f
		}
	}
}
