package server

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/vortelio/vortelio/internal/config"
)

// GET  /api/config — return current server config
// POST /api/config — partial update + persist to config.json
func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respond(w, 200, config.Get())

	case http.MethodPost:
		cfg := config.Get()
		// Decode into a map so we can do partial updates
		var patch map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			jsonError(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if v, ok := patch["port"].(float64); ok && v > 0 {
			cfg.Port = int(v)
		}
		if v, ok := patch["bind_addr"].(string); ok {
			cfg.BindAddr = v
		}
		if v, ok := patch["api_key"].(string); ok {
			cfg.APIKey = v
		}
		if v, ok := patch["ollama_port"].(float64); ok && v > 0 {
			cfg.OllamaPort = int(v)
		}
		if v, ok := patch["cloud_timeout_sec"].(float64); ok && v > 0 {
			cfg.CloudTimeoutSec = int(v)
		}
		if v, ok := patch["python_bin"].(string); ok {
			cfg.PythonBin = v
		}
		if v, ok := patch["auto_update"].(bool); ok {
			cfg.AutoUpdate = v
		}
		if v, ok := patch["allow_origins"].([]interface{}); ok {
			origins := make([]string, 0, len(v))
			for _, o := range v {
				if s, ok := o.(string); ok && s != "" {
					origins = append(origins, s)
				}
			}
			cfg.AllowOrigins = origins
		}

		// Persist to file
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			jsonError(w, 500, "marshal failed: "+err.Error())
			return
		}
		os.MkdirAll(config.HomeDir(), 0755)
		if err := os.WriteFile(config.HomeDir()+"/config.json", data, 0644); err != nil {
			jsonError(w, 500, "write config failed: "+err.Error())
			return
		}
		respond(w, 200, map[string]interface{}{"status": "saved", "config": cfg})

	default:
		jsonError(w, 405, "use GET or POST")
	}
}
