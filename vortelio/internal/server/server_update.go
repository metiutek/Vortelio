package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/updater"
)

func handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "GET only")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info, err := updater.Check(ctx)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	respond(w, 200, info)
}

func handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Force   bool `json:"force"`
		Restart bool `json:"restart"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if !req.Force {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		info, err := updater.Check(ctx)
		cancel()
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		if !info.Available {
			respond(w, 200, map[string]interface{}{"started": false, "message": "Vortelio e' gia' aggiornato.", "info": info})
			return
		}
	}

	res, err := updater.StartDetached(req.Restart)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	respond(w, 200, res)
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestShutdown()
	}()
}

func requestShutdown() {
	select {
	case shutdownCh <- struct{}{}:
	default:
	}
}

// AutoUpdateVortelio installs new Vortelio versions by itself when the user
// enabled auto_update in config. It only fires while the server is idle (no
// request in flight, e.g. no chat streaming), then restarts the GUI.
func AutoUpdateVortelio(logf func(format string, args ...any)) {
	go func() {
		time.Sleep(2 * time.Minute) // let startup settle
		for {
			if config.Get().AutoUpdate && tryAutoUpdate(logf) {
				return
			}
			time.Sleep(30 * time.Minute)
		}
	}()
}

func tryAutoUpdate(logf func(format string, args ...any)) bool {
	info, err := updater.CheckWithTimeout(15 * time.Second)
	if err != nil || !info.Available {
		return false
	}
	if atomic.LoadInt64(&mReqInFlight) > 0 {
		return false // busy: retry on the next tick
	}
	logf("Auto-update: installing Vortelio %s (current %s)", info.Latest, info.Current)
	if _, err := updater.StartDetached(true); err != nil {
		logf("Auto-update failed: %v", err)
		return false
	}
	time.Sleep(500 * time.Millisecond)
	requestShutdown()
	return true
}
