//go:build webview

package main

import (
	"github.com/webview/webview_go"
)

// webviewBuilt is true in builds with the `webview` tag.
const webviewBuilt = true

// openNativeWindow opens the Stroom UI in a native webview window.
// Blocks until the window is closed. Returns false if the window could
// not be created (caller should fall back to the browser).
func openNativeWindow(url string) bool {
	w := webview.New(false)
	if w == nil {
		return false
	}
	defer w.Destroy()
	w.SetTitle("Stroom")
	w.SetSize(1280, 800, webview.HintNone)
	w.Navigate(url)
	w.Run()
	return true
}
