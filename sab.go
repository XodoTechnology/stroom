package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Minimal SABnzbd-compatible API shim at /sabnzbd/api so *arr tools
// (Sonarr/Radarr/Lidarr/Readarr/Prowlarr) can submit + track NZB downloads
// natively. Supports mode=version|queue|history|addfile|addurl.
//
// Auth: pass the configured UIToken as `apikey` (or the normal Bearer/
// cookie/query-token mechanisms).

func (a *apiServer) handleSAB(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mode := r.FormValue("mode")
	enc := func(v any) { _ = json.NewEncoder(w).Encode(v) }

	switch mode {
	case "version":
		enc(map[string]any{"version": "4.3.0"})

	case "queue":
		var slots []any
		for _, j := range a.app.NZB.List() {
			slots = append(slots, map[string]any{
				"nzo_id":     j.ID,
				"filename":   j.Name,
				"status":     sabStatus(j.Status),
				"size":       bytesHuman(j.TotalBytes),
				"sizeleft":   bytesHuman(j.TotalBytes - j.DoneBytes),
				"mb":         j.TotalBytes / (1 << 20),
				"mbleft":     (j.TotalBytes - j.DoneBytes) / (1 << 20),
				"percentage": int(j.Progress * 100),
				"cat":        j.Category,
				"priority":   "Normal",
				"timeleft":   etaHuman(j),
			})
		}
		enc(map[string]any{"queue": map[string]any{
			"slots": slots, "version": "4.3.0",
			"speed":  bytesHuman(a.app.NZB.GlobalDownSpeed()) + "/s",
			"paused": false,
			"mbleft": 0, "mb": 0,
			"diskspace1": "", "diskspace2": "",
		}})

	case "history":
		var slots []any
		for _, j := range a.app.NZB.List() {
			jSt := j.Status
			if jSt == "downloading" || jSt == "queued" || jSt == "verifying" {
				continue
			}
			slots = append(slots, map[string]any{
				"nzo_id":       j.ID,
				"name":         j.Name,
				"status":       map[bool]string{true: "Completed", false: "Failed"}[jSt == "done"],
				"size":         bytesHuman(j.TotalBytes),
				"category":     j.Category,
				"completed":    j.FinishedAt.Unix(),
				"fail_message": j.Error,
				"storage":      j.Dir,
			})
		}
		enc(map[string]any{"history": map[string]any{"slots": slots, "version": "4.3.0"}})

	case "addfile":
		f, hdr, err := r.FormFile("name")
		if err != nil {
			enc(map[string]any{"status": false, "error": "no file"})
			return
		}
		defer f.Close()
		data := make([]byte, 0, 1<<20)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := f.Read(buf)
			data = append(data, buf[:n]...)
			if rerr != nil {
				break
			}
			if len(data) > 64<<20 {
				enc(map[string]any{"status": false, "error": "nzb too large"})
				return
			}
		}
		name := strings.TrimSuffix(hdr.Filename, ".nzb")
		id, err := a.app.NZB.ParseAndAdd(data, name, r.FormValue("server"), r.FormValue("cat"))
		if err != nil {
			enc(map[string]any{"status": false, "error": err.Error()})
			return
		}
		enc(map[string]any{"status": true, "nzo_ids": []string{id}})

	case "addurl":
		u := r.FormValue("name")
		id, err := a.app.NZB.FetchAndAdd(r.Context(), u, r.FormValue("server"), r.FormValue("cat"))
		if err != nil {
			enc(map[string]any{"status": false, "error": err.Error()})
			return
		}
		enc(map[string]any{"status": true, "nzo_ids": []string{id}})

	default:
		enc(map[string]any{"status": false, "error": "unsupported mode: " + mode})
	}
}

func sabStatus(s string) string {
	switch s {
	case "downloading":
		return "Downloading"
	case "queued":
		return "Queued"
	case "paused":
		return "Paused"
	case "verifying":
		return "Verifying"
	case "done":
		return "Completed"
	default:
		return "Failed"
	}
}

func etaHuman(j *NZBJob) string {
	if j.DownSpeed <= 0 || j.TotalBytes <= 0 {
		return "0:00:00"
	}
	s := (j.TotalBytes - j.DoneBytes) / j.DownSpeed
	d := time.Duration(s) * time.Second
	return d.Round(time.Second).String()
}

func bytesHuman(n int64) string {
	const u = 1 << 30
	switch {
	case n >= u:
		return strconv.FormatFloat(float64(n)/u, 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}
