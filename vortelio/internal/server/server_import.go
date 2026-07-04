package server

import (
	"encoding/json"
	"net/http"

	"github.com/vortelio/vortelio/internal/hub"
)

// /api/import/ollama — import models from a local Ollama installation. The
// actual scan/register logic lives in hub.ImportOllama so the CLI can run it
// without a server. See hub/import_ollama.go.
func handleImportOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		OllamaPath string `json:"ollama_path"` // optional override
		DryRun     bool   `json:"dry_run"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	res, err := hub.ImportOllama(req.OllamaPath, req.DryRun)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	respond(w, 200, res)
}
