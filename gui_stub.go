//go:build !webview

package main

// webviewBuilt is false in builds without the `webview` tag.
const webviewBuilt = false

// openNativeWindow is unavailable in builds without the `webview` tag.
// Returns false so the caller falls back to opening a browser.
func openNativeWindow(url string) bool { return false }
