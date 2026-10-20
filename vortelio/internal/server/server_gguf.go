package server

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/vortelio/vortelio/internal/gguf"
	"github.com/vortelio/vortelio/internal/hub"
)

// /api/gguf/inspect — metadata and tensor list of a GGUF file (see internal/gguf).
func handleGGUFInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Path  string `json:"path"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	path := req.Path
	if path == "" && req.Model != "" {
		ref, err := hub.ParseModelRef(req.Model)
		if err != nil {
			jsonError(w, 400, err.Error())
			return
		}
		m, err := hub.NewModelStore().Resolve(ref)
		if err != nil {
			jsonError(w, 404, err.Error())
			return
		}
		path = m.LocalPath
	}
	if path == "" {
		jsonError(w, 400, "path or model required")
		return
	}

	st, err := os.Stat(path)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	g, err := gguf.Read(path, 2000)
	if err != nil {
		jsonError(w, 500, "GGUF parse error: "+err.Error())
		return
	}
	respond(w, 200, map[string]interface{}{
		"path":           path,
		"file_size":      st.Size(),
		"gguf_version":   g.Version,
		"tensor_count":   g.TensorCount,
		"metadata_count": len(g.Metadata),
		"metadata":       g.Metadata,
		"tensor_sample":  g.Tensors,
	})
}
