package main

import (
	"context"
	"net/http"
	"time"
)

// updateCheckInterval throttles automatic update checks to once a day.
const updateCheckInterval = 24 * time.Hour

// maybeAutoUpdateCheck runs a background update check if due. Never blocks.
func (a *App) maybeAutoUpdateCheck() {
	cfg := a.configSnapshot()
	if cfg.LastUpdateCheck != "" {
		if t, err := time.Parse(time.RFC3339, cfg.LastUpdateCheck); err == nil && time.Since(t) < updateCheckInterval {
			return
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		a.refreshUpdate(ctx, true)
	}()
}

// refreshUpdate performs a check and caches the result.
func (a *App) refreshUpdate(ctx context.Context, markChecked bool) *updateInfo {
	info, err := checkForUpdate(ctx, updateRepo(), a.version)
	if err != nil && info.Error == "" {
		info.Error = err.Error()
	}
	a.updMu.Lock()
	a.upd = info
	a.updMu.Unlock()
	if markChecked {
		a.lockConfig()
		a.Config.LastUpdateCheck = time.Now().UTC().Format(time.RFC3339)
		_ = a.saveConfig()
		a.unlockConfig()
	}
	return info
}

// updateStatus returns the cached check result (or an unchecked placeholder).
func (a *App) updateStatus() *updateInfo {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	if a.upd != nil {
		cp := *a.upd
		return &cp
	}
	return &updateInfo{Configured: true, Current: a.version}
}

// ---------- API ----------

func (a *apiServer) handleUpdateGet(w http.ResponseWriter, r *http.Request) {
	writeJSONResp(w, a.app.updateStatus())
}

func (a *apiServer) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSONResp(w, a.app.refreshUpdate(ctx, true))
}

func (a *apiServer) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	info := a.app.updateStatus()
	if !info.Available {
		writeErr(w, http.StatusConflict, "no update available")
		return
	}
	a.app.updMu.Lock()
	if a.app.upd != nil {
		a.app.upd.Applying = true
	}
	a.app.updMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	msg, err := applyUpdate(ctx, info)
	a.app.updMu.Lock()
	if a.app.upd != nil {
		a.app.upd.Applying = false
	}
	a.app.updMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONResp(w, map[string]any{"ok": true, "message": msg, "restartRequired": true})
}
