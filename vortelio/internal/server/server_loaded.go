package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// Chat model residency for the GUI (LM Studio style): the model is loaded as
// soon as it is picked and stays ready for the whole conversation, until it
// is ejected, another model is picked, or a studio needs the GPU.

type loadedInfo struct {
	Model       string    `json:"model"` // type/name:tag
	ContextSize int       `json:"context_size"`
	GPULayers   int       `json:"gpu_layers"`
	TotalLayers int       `json:"total_layers"`
	SizeVRAM    int64     `json:"size_vram"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"` // zero = until ejected
}

func loadedChatModels() []loadedInfo {
	out := []loadedInfo{}
	for _, lm := range runtime.GlobalModelManager.ListLoaded() {
		if lm.Embedding || lm.Model == nil {
			continue
		}
		out = append(out, loadedInfo{
			Model: lm.Model.Type + "/" + lm.Model.Name + ":" + lm.Model.Tag, ContextSize: lm.ContextSize,
			GPULayers: lm.GPULayers, TotalLayers: lm.TotalLayers, SizeVRAM: lm.SizeVRAM, ExpiresAt: lm.ExpiresAt,
		})
	}
	return out
}

// cpuOnly returns a CPU-only copy of hw (the detected hardware is shared).
func cpuOnly(hw *runtime.Hardware) *runtime.Hardware {
	c := *hw
	c.Backend = runtime.BackendCPU
	return &c
}

// GET /api/models/loaded
func handleModelsLoaded(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]any{"models": loadedChatModels()})
}

// POST /api/models/load {model, context_size, keep_alive}: loads a chat model
// and returns when it is ready.
func handleModelLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model       string          `json:"model"`
		ContextSize int             `json:"context_size"`
		KeepAlive   json.RawMessage `json:"keep_alive"`
		ForceCPU    bool            `json:"force_cpu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	ref, err := hub.ParseModelRef(req.Model)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	m, err := hub.NewModelStore().Resolve(ref)
	if err != nil {
		jsonError(w, 404, "model "+req.Model+" is not installed")
		return
	}
	if m.Type != "llm" {
		jsonError(w, 400, req.Model+" is not a chat model")
		return
	}
	hw := getHardware()
	if req.ForceCPU {
		hw = cpuOnly(hw)
	}
	start := time.Now()
	if _, err := runtime.GlobalModelManager.GetOrLoadWithContext(m, hw, parseKeepAlive(req.KeepAlive), req.ContextSize); err != nil {
		if errors.Is(err, runtime.ErrLoadAborted) {
			respond(w, 200, map[string]any{"status": "cancelled", "models": loadedChatModels()})
			return
		}
		jsonError(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]any{"status": "ready", "load_seconds": time.Since(start).Seconds(), "models": loadedChatModels()})
}

// POST /api/models/eject {model}: frees the memory (all chat models when
// model is empty).
func handleModelEject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		runtime.GlobalModelManager.UnloadAll()
	} else if ref, err := hub.ParseModelRef(req.Model); err == nil {
		if m, err := hub.NewModelStore().Resolve(ref); err == nil {
			runtime.GlobalModelManager.Unload(m)
		}
	}
	respond(w, 200, map[string]any{"models": loadedChatModels()})
}
