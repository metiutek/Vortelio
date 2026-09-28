package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/vortelio/vortelio/internal/runtime"
)

// llama.cpp engine status + in-app update (Settings → Engine).

type engineJob struct {
	Running bool    `json:"running"`
	Message string  `json:"message,omitempty"`
	Percent float64 `json:"percent"`
	Error   string  `json:"error,omitempty"`
	Done    bool    `json:"done"`
}

var (
	engineMu  sync.Mutex
	engineCur engineJob
)

func engineSnapshot() engineJob {
	engineMu.Lock()
	defer engineMu.Unlock()
	return engineCur
}

// GET /api/engine → installed/latest llama.cpp build and any update in progress.
func handleEngineStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"llama": runtime.CheckLlama(),
		"job":   engineSnapshot(),
	})
}

// POST /api/engine/update → stop loaded models and install the latest llama.cpp
// in the background. Poll GET /api/engine for progress.
func handleEngineUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	engineMu.Lock()
	if engineCur.Running {
		engineMu.Unlock()
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "an update is already running"})
		return
	}
	engineCur = engineJob{Running: true, Message: "Starting…"}
	engineMu.Unlock()

	go func() {
		runtime.GlobalModelManager.UnloadAll()
		_, err := runtime.InstallLlamaCpp(getHardware(), func(msg string, pct float64) {
			engineMu.Lock()
			engineCur.Message, engineCur.Percent = msg, pct
			engineMu.Unlock()
		})
		engineMu.Lock()
		engineCur.Running, engineCur.Done = false, true
		if err != nil {
			engineCur.Error = err.Error()
		} else {
			engineCur.Percent = 1
		}
		engineMu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(engineSnapshot())
}

// AutoUpdateEngine installs/refreshes llama.cpp in the background at server
// start, before any model is loaded.
func AutoUpdateEngine(logf func(format string, args ...any)) {
	go runtime.AutoUpdateLlama(getHardware(), logf)
}
