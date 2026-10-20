package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/internal/version"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// resolveModel resolves a model name that may or may not include the type prefix.
// Supports: "llm/mistral:7b", "mistral:7b", "mistral", "mistral/7b" (Ollama-style).
func resolveModel(name string) (*hub.Model, error) {
	store := hub.NewModelStore()

	// Try direct parse first (handles "llm/mistral:7b" etc.)
	if ref, err := hub.ParseModelRef(name); err == nil {
		if m, err := store.Resolve(ref); err == nil {
			return m, nil
		}
	}

	// Try with "llm/" prefix (Ollama users omit type prefix)
	if !strings.HasPrefix(name, "llm/") && !strings.HasPrefix(name, "image/") &&
		!strings.HasPrefix(name, "audio/") && !strings.HasPrefix(name, "video/") &&
		!strings.HasPrefix(name, "3d/") {
		if ref, err := hub.ParseModelRef("llm/" + name); err == nil {
			if m, err := store.Resolve(ref); err == nil {
				return m, nil
			}
		}
	}

	// Fuzzy search: match by model name (case-insensitive)
	models, _ := store.List()
	searchName := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		searchName = name[idx+1:]
	}
	baseName := strings.SplitN(searchName, ":", 2)[0]
	searchTag := ""
	if parts := strings.SplitN(searchName, ":", 2); len(parts) == 2 {
		searchTag = parts[1]
	}
	for _, m := range models {
		if strings.EqualFold(m.Name, baseName) {
			if searchTag == "" || strings.EqualFold(m.Tag, searchTag) {
				return m, nil
			}
		}
	}

	return nil, fmt.Errorf("model %q not found locally", name)
}

// ollamaModelID returns the Ollama-style model ID (name:tag) for a model.
func ollamaModelID(m *hub.Model) string {
	return m.Name + ":" + m.Tag
}

// parseKeepAlive parses an Ollama keep_alive value (string like "5m", "30s", or number of seconds).
func parseKeepAlive(raw json.RawMessage) time.Duration {
	if raw == nil {
		return runtime.DefaultKeepAliveDuration
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "", "0":
			return 0
		case "-1":
			return -1
		}
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		if n < 0 {
			return -1
		}
		return time.Duration(n) * time.Second
	}
	return runtime.DefaultKeepAliveDuration
}

// ollamaOptions converts an Ollama "options" map to runtime.LLMOptions.
func ollamaOptions(raw json.RawMessage) runtime.LLMOptions {
	if raw == nil {
		return runtime.LLMOptions{}
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return runtime.LLMOptions{}
	}
	opts := runtime.LLMOptions{}
	if v, ok := m["temperature"].(float64); ok {
		opts.Temperature = v
	}
	if v, ok := m["top_p"].(float64); ok {
		opts.TopP = v
	}
	if v, ok := m["top_k"].(float64); ok {
		opts.TopK = int(v)
	}
	if v, ok := m["num_predict"].(float64); ok {
		opts.MaxTokens = int(v)
	}
	if v, ok := m["num_ctx"].(float64); ok {
		opts.NumCtx = int(v)
	}
	if v, ok := m["repeat_penalty"].(float64); ok {
		opts.RepeatPenalty = v
	}
	if v, ok := m["repeat_last_n"].(float64); ok {
		opts.RepeatLastN = int(v)
	}
	if v, ok := m["min_p"].(float64); ok {
		opts.MinP = v
	}
	if v, ok := m["seed"].(float64); ok {
		opts.Seed = int(v)
	}
	if v, ok := m["stop"]; ok {
		switch sv := v.(type) {
		case string:
			opts.Stop = []string{sv}
		case []interface{}:
			for _, s := range sv {
				if str, ok2 := s.(string); ok2 {
					opts.Stop = append(opts.Stop, str)
				}
			}
		}
	}
	if v, ok := m["num_gpu"].(float64); ok {
		opts.NumGPU = int(v)
	}
	if v, ok := m["num_thread"].(float64); ok {
		opts.NumThreads = int(v)
	}
	if v, ok := m["flash_attn"].(bool); ok {
		opts.FlashAttn = v
	}
	if v, ok := m["use_mmap"].(bool); ok {
		opts.Mmap = &v
	}
	if v, ok := m["numa"].(bool); ok {
		opts.Numa = v
	}
	if v, ok := m["tfs_z"].(float64); ok {
		opts.TfsZ = v
	}
	if v, ok := m["typical_p"].(float64); ok {
		opts.TypicalP = v
	}
	if v, ok := m["presence_penalty"].(float64); ok {
		opts.PresencePenalty = v
	}
	if v, ok := m["frequency_penalty"].(float64); ok {
		opts.FrequencyPenalty = v
	}
	// Ollama aliases
	if v, ok := m["num_predict"].(float64); ok && opts.MaxTokens == 0 {
		opts.MaxTokens = int(v)
	}
	return opts
}

// convertOllamaMessages converts Ollama-style messages (with top-level "images" array)
// to OpenAI vision format (content array with text + image_url parts).
func convertOllamaMessages(msgs []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs))
	for _, m := range msgs {
		role, _ := m["role"].(string)
		content := m["content"]

		// Check for Ollama-style images field: {"role":"user","content":"text","images":["base64..."]}
		var images []interface{}
		if raw, ok := m["images"]; ok {
			if arr, ok2 := raw.([]interface{}); ok2 {
				images = arr
			}
		}

		if len(images) > 0 {
			// Convert to OpenAI vision content array
			textStr := ""
			if s, ok := content.(string); ok {
				textStr = s
			}
			parts := []interface{}{map[string]string{"type": "text", "text": textStr}}
			for _, img := range images {
				b64, ok := img.(string)
				if !ok {
					continue
				}
				// Detect image type from base64 header; default to jpeg
				mime := "image/jpeg"
				if strings.HasPrefix(b64, "iVBOR") {
					mime = "image/png"
				}
				if strings.HasPrefix(b64, "R0lGO") {
					mime = "image/gif"
				}
				if strings.HasPrefix(b64, "Qk0") {
					mime = "image/bmp"
				}
				parts = append(parts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": "data:" + mime + ";base64," + b64},
				})
			}
			newMsg := map[string]interface{}{"role": role, "content": parts}
			// Preserve other fields (tool_call_id, name, etc.)
			for k, v := range m {
				if k != "role" && k != "content" && k != "images" {
					newMsg[k] = v
				}
			}
			out = append(out, newMsg)
		} else {
			out = append(out, m)
		}
	}
	return out
}

// blobDir returns the path to the blob storage directory.
func blobDir() string {
	return filepath.Join(config.HomeDir(), "blobs")
}

// blobPath returns the filesystem path for a blob by digest (e.g. "sha256:abc...").
func blobPath(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return "", fmt.Errorf("invalid digest format: must be sha256:<64 hex chars>")
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(blobDir(), "sha256-"+hex), nil
}

// ── GET /api/version ──────────────────────────────────────────────────────────

func handleOllamaVersion(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]string{"version": version.Version})
}

// ── GET /api/ps ───────────────────────────────────────────────────────────────

func handleOllamaPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "GET only")
		return
	}
	loaded := runtime.GlobalModelManager.ListLoaded()
	type psModel struct {
		Name      string    `json:"name"`
		Model     string    `json:"model"`
		Size      int64     `json:"size"`
		Digest    string    `json:"digest"`
		Details   any       `json:"details"`
		ExpiresAt time.Time `json:"expires_at"`
		SizeVRAM  int64     `json:"size_vram"`
	}
	var models []psModel
	for _, lm := range loaded {
		expiry := lm.ExpiresAt
		if expiry.IsZero() {
			expiry = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		models = append(models, psModel{
			Name:      ollamaModelID(lm.Model),
			Model:     ollamaModelID(lm.Model),
			Size:      lm.Model.SizeBytes,
			Digest:    "sha256:" + strings.Repeat("0", 64),
			ExpiresAt: expiry,
			SizeVRAM:  lm.SizeVRAM,
			Details: map[string]string{
				"format":             lm.Model.Format,
				"family":             lm.Model.Name,
				"parameter_size":     lm.Model.Parameters,
				"quantization_level": "",
			},
		})
	}
	if models == nil {
		models = []psModel{}
	}
	respond(w, 200, map[string]interface{}{"models": models})
}

// ── POST /api/chat ────────────────────────────────────────────────────────────

type ollamaChatRequest struct {
	Model     string                   `json:"model"`
	Messages  []map[string]interface{} `json:"messages"`
	Stream    *bool                    `json:"stream"`
	Format    json.RawMessage          `json:"format"`
	Options   json.RawMessage          `json:"options"`
	KeepAlive json.RawMessage          `json:"keep_alive"`
	Think     bool                     `json:"think"`
	Tools     json.RawMessage          `json:"tools"`
}

func handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req ollamaChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}

	model, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	if model.Type != "llm" {
		jsonError(w, 400, "model type must be llm for /api/chat")
		return
	}

	hw := getHardware()
	ka := parseKeepAlive(req.KeepAlive)
	loadStart := time.Now()
	runner, err := runtime.GlobalModelManager.GetOrLoad(model, hw, ka)
	loadDur := time.Since(loadStart)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	var formatStr string
	if req.Format != nil {
		var s string
		if json.Unmarshal(req.Format, &s) == nil {
			formatStr = s
		} else {
			formatStr = string(req.Format)
		}
	}

	modelID := ollamaModelID(model)

	// Determine tool mode: client-side (pass tools to model, return tool_calls) vs server-side builtin
	clientTools := req.Tools                     // may be nil
	serverSideTools := req.Tools == nil && false // reserved for future: builtin tools via separate flag

	// Convert Ollama messages: images field inside message → OpenAI vision content array
	convertedMsgs := convertOllamaMessages(req.Messages)

	sopts := runtime.StreamOpts{
		Messages:     convertedMsgs,
		Think:        req.Think,
		ToolsEnabled: serverSideTools,
		ClientTools:  clientTools,
		Options:      ollamaOptions(req.Options),
		Format:       formatStr,
	}
	var genStats runtime.GenStats
	sopts.Stats = &genStats

	var thinkBuf strings.Builder
	if req.Think {
		if stream {
			flusherRef, canFlushRef := w.(http.Flusher)
			sopts.ThinkEmit = func(token string) {
				thinkBuf.WriteString(token)
				chunk := map[string]interface{}{
					"model":      modelID,
					"created_at": time.Now().UTC().Format(time.RFC3339Nano),
					"message":    map[string]interface{}{"role": "assistant", "content": "", "thinking": token},
					"done":       false,
				}
				line, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "%s\n", line)
				if canFlushRef {
					flusherRef.Flush()
				}
			}
		} else {
			sopts.ThinkEmit = func(token string) { thinkBuf.WriteString(token) }
		}
	}

	writeNDJSON := func(obj map[string]interface{}) {
		line, _ := json.Marshal(obj)
		fmt.Fprintf(w, "%s\n", line)
	}
	flushW := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	buildDoneChunk := func(msg map[string]interface{}, reason string) map[string]interface{} {
		obj := map[string]interface{}{
			"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"message": msg, "done": true, "done_reason": reason,
		}
		addGenStats(obj, &genStats, loadDur, loadStart)
		return obj
	}

	// Wire up ToolCallEmit: when client sends tools, return tool_calls to client (don't execute server-side)
	var capturedToolCalls []map[string]interface{}
	if clientTools != nil {
		sopts.ToolCallEmit = func(calls []runtime.ToolCall) {
			ollamaCalls := make([]map[string]interface{}, len(calls))
			for i, tc := range calls {
				var argsMap interface{}
				if json.Unmarshal([]byte(tc.Function.Arguments), &argsMap) != nil {
					argsMap = tc.Function.Arguments
				}
				ollamaCalls[i] = map[string]interface{}{
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": argsMap,
					},
				}
			}
			capturedToolCalls = ollamaCalls
		}
	}

	if stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")

		runErr := runner.StreamWithOpts(sopts, func(token string) {
			msg := map[string]interface{}{"role": "assistant", "content": token}
			writeNDJSON(map[string]interface{}{
				"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"message": msg, "done": false,
			})
			flushW()
		}, nil)

		if runErr != nil {
			msg := map[string]interface{}{"role": "assistant", "content": ""}
			writeNDJSON(buildDoneChunk(msg, "error"))
			flushW()
			return
		}
		if len(capturedToolCalls) > 0 {
			msg := map[string]interface{}{"role": "assistant", "content": "", "tool_calls": capturedToolCalls}
			writeNDJSON(buildDoneChunk(msg, "tool_calls"))
		} else {
			msg := map[string]interface{}{"role": "assistant", "content": ""}
			if thinkBuf.Len() > 0 {
				msg["thinking"] = thinkBuf.String()
			}
			writeNDJSON(buildDoneChunk(msg, "stop"))
		}
		flushW()
		return
	}

	// Non-streaming
	var fullResp strings.Builder
	runErr := runner.StreamWithOpts(sopts, func(token string) {
		fullResp.WriteString(token)
	}, nil)
	if runErr != nil {
		jsonError(w, 500, runErr.Error())
		return
	}
	if len(capturedToolCalls) > 0 {
		msg := map[string]interface{}{"role": "assistant", "content": "", "tool_calls": capturedToolCalls}
		respond(w, 200, buildDoneChunk(msg, "tool_calls"))
		return
	}
	msg := map[string]interface{}{"role": "assistant", "content": fullResp.String()}
	if thinkBuf.Len() > 0 {
		msg["thinking"] = thinkBuf.String()
	}
	respond(w, 200, buildDoneChunk(msg, "stop"))
}

// ── POST /api/embed ───────────────────────────────────────────────────────────

type ollamaEmbedRequest struct {
	Model     string          `json:"model"`
	Input     json.RawMessage `json:"input"` // string or []string
	Truncate  *bool           `json:"truncate"`
	Options   json.RawMessage `json:"options"`
	KeepAlive json.RawMessage `json:"keep_alive"`
}

func handleOllamaEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req ollamaEmbedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}

	var inputs []string
	var s string
	if json.Unmarshal(req.Input, &s) == nil {
		inputs = []string{s}
	} else {
		if json.Unmarshal(req.Input, &inputs) != nil {
			jsonError(w, 400, "input must be a string or array of strings")
			return
		}
	}

	model, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}

	hw := getHardware()
	ka := parseKeepAlive(req.KeepAlive)
	runner, err := runtime.GlobalModelManager.GetOrLoadEmbed(model, hw, ka)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	start := time.Now()
	embs, err := runner.EmbedBatch(inputs)
	if err != nil {
		jsonError(w, 500, "embedding failed: "+err.Error())
		return
	}
	respond(w, 200, map[string]interface{}{
		"model":             ollamaModelID(model),
		"embeddings":        embs,
		"total_duration":    time.Since(start).Nanoseconds(),
		"load_duration":     0,
		"prompt_eval_count": len(inputs),
	})
}

// ── POST /api/embeddings (legacy) ─────────────────────────────────────────────

type ollamaEmbeddingsLegacyRequest struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	Options   json.RawMessage `json:"options"`
	KeepAlive json.RawMessage `json:"keep_alive"`
}

func handleOllamaEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req ollamaEmbeddingsLegacyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}

	model, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}

	hw := getHardware()
	ka := parseKeepAlive(req.KeepAlive)
	runner, err := runtime.GlobalModelManager.GetOrLoadEmbed(model, hw, ka)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	emb, err := runner.Embed(req.Prompt)
	if err != nil {
		jsonError(w, 500, "embedding failed: "+err.Error())
		return
	}
	respond(w, 200, map[string]interface{}{"embedding": emb})
}

// ── POST /api/copy ────────────────────────────────────────────────────────────

func handleOllamaCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	srcModel, err := resolveModel(req.Source)
	if err != nil {
		jsonError(w, 404, "source model not found: "+err.Error())
		return
	}

	dstRef, err := hub.ParseModelRef(req.Destination)
	if err != nil {
		// Try with "llm/" prefix
		dstRef, err = hub.ParseModelRef("llm/" + req.Destination)
		if err != nil {
			jsonError(w, 400, "invalid destination: "+err.Error())
			return
		}
	}

	// Create new manifest pointing to same model file
	newModel := *srcModel
	newModel.Name = dstRef.Name
	newModel.Tag = dstRef.Tag
	if dstRef.Type != "" {
		newModel.Type = dstRef.Type
	}
	newModel.DisplayName = ""
	newModel.DownloadedAt = time.Now()

	store := hub.NewModelStore()
	if err := store.Save(&newModel); err != nil {
		jsonError(w, 500, "failed to save copy: "+err.Error())
		return
	}
	w.WriteHeader(200)
}

// ── POST /api/create ──────────────────────────────────────────────────────────

func handleOllamaCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model     string          `json:"model"`
		Modelfile string          `json:"modelfile"`
		Stream    *bool           `json:"stream"`
		From      string          `json:"from"`
		Files     json.RawMessage `json:"files"`
		System    string          `json:"system"`
		Quantize  string          `json:"quantize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}

	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	sendStatus := func(status string) {
		line, _ := json.Marshal(map[string]string{"status": status})
		if stream {
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, "%s\n", line)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}

	// Parse Modelfile or use req.From / req.System directly
	fromModel := req.From
	systemPrompt := req.System

	if req.Modelfile != "" {
		var tmpl, adap string
		var extraParams map[string]string
		var modelfileMessages []map[string]string
		fromModel, systemPrompt, tmpl, adap, extraParams, modelfileMessages = parseModelfile(req.Modelfile)
		_, _, _ = tmpl, adap, extraParams
		_ = modelfileMessages
	}

	if fromModel == "" {
		jsonError(w, 400, "FROM directive required in Modelfile or 'from' field")
		return
	}

	sendStatus("reading model metadata")

	baseModel, err := resolveModel(fromModel)
	if err != nil {
		// Check blob store
		if bpath, berr := blobPath(fromModel); berr == nil {
			if _, serr := os.Stat(bpath); serr == nil {
				jsonError(w, 501, "creating models from raw blobs not yet supported; use a local model reference")
				return
			}
		}
		jsonError(w, 404, "base model not found: "+err.Error())
		return
	}

	sendStatus("creating model")

	dstRef, err := hub.ParseModelRef(req.Model)
	if err != nil {
		dstRef, err = hub.ParseModelRef("llm/" + req.Model)
		if err != nil {
			jsonError(w, 400, "invalid model name: "+err.Error())
			return
		}
	}

	newModel := *baseModel
	newModel.Name = dstRef.Name
	newModel.Tag = dstRef.Tag
	if dstRef.Type != "" {
		newModel.Type = dstRef.Type
	}
	newModel.DisplayName = ""
	newModel.DownloadedAt = time.Now()
	if systemPrompt != "" {
		newModel.SystemOverride = systemPrompt
	}
	if req.Modelfile != "" {
		newModel.Modelfile = req.Modelfile
		// Store parsed Modelfile fields on the model
		var tmplStr, adapStr string
		var mfParams map[string]string
		var mfMsgs []map[string]string
		_, _, tmplStr, adapStr, mfParams, mfMsgs = parseModelfile(req.Modelfile)
		_, _ = adapStr, mfMsgs
		if tmplStr != "" {
			newModel.Template = tmplStr
		}
		if mfParams != nil {
			if newModel.ModelParameters == nil {
				newModel.ModelParameters = make(map[string]string)
			}
			for k, v := range mfParams {
				newModel.ModelParameters[k] = v
			}
			// Apply num_gpu_layers if present
			if v, ok := mfParams["num_gpu"]; ok {
				var n int
				fmt.Sscanf(v, "%d", &n)
				if n > 0 {
					newModel.NumGPULayers = n
				}
			}
			// Apply stop tokens
			if stop, ok := mfParams["stop"]; ok && stop != "" {
				newModel.StopTokens = append(newModel.StopTokens, stop)
			}
		}
	}

	store := hub.NewModelStore()
	if err := store.Save(&newModel); err != nil {
		jsonError(w, 500, "failed to save model: "+err.Error())
		return
	}

	// Optional quantization: if quantize field set, run llama-quantize on the new model.
	if req.Quantize != "" {
		sendStatus("quantizing model to " + req.Quantize)
		bin := findLlamaQuantize()
		if bin == "" {
			jsonError(w, 500, "llama-quantize binary not found in PATH")
			return
		}
		srcPath := newModel.LocalPath
		dstPath := strings.TrimSuffix(srcPath, filepath.Ext(srcPath)) + "-" + strings.ToUpper(req.Quantize) + ".gguf"
		cmd := exec.Command(bin, srcPath, dstPath, strings.ToUpper(req.Quantize))
		if out, qerr := cmd.CombinedOutput(); qerr != nil {
			jsonError(w, 500, "quantize failed: "+qerr.Error()+"\n"+string(out))
			return
		}
		newModel.LocalPath = dstPath
		if st, serr := os.Stat(dstPath); serr == nil {
			newModel.SizeBytes = st.Size()
		}
		if err := store.Save(&newModel); err != nil {
			jsonError(w, 500, "failed to save quantized model: "+err.Error())
			return
		}
	}

	sendStatus("success")
	if !stream {
		respond(w, 200, map[string]string{"status": "success"})
	}
}

// parseModelfile extracts Modelfile directives: FROM, SYSTEM, PARAMETER, TEMPLATE, MESSAGE, ADAPTER.
func parseModelfile(content string) (from, system, template, adapter string, params map[string]string, messages []map[string]string) {
	params = make(map[string]string)
	var sysLines []string
	var tmplLines []string
	inSystem := false
	inTemplate := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if inSystem {
				sysLines = append(sysLines, line)
			} else if inTemplate {
				tmplLines = append(tmplLines, line)
			}
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "FROM "):
			from = strings.TrimSpace(trimmed[5:])
			inSystem, inTemplate = false, false
		case strings.HasPrefix(upper, "SYSTEM "):
			sysLines = append(sysLines, strings.TrimSpace(trimmed[7:]))
			inSystem = true
			inTemplate = false
		case strings.HasPrefix(upper, "TEMPLATE "):
			rest := strings.TrimSpace(trimmed[9:])
			// Strip surrounding quotes if present
			if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
				rest = rest[1 : len(rest)-1]
			}
			tmplLines = append(tmplLines, rest)
			inTemplate = true
			inSystem = false
		case strings.HasPrefix(upper, "PARAMETER "):
			rest := strings.TrimSpace(trimmed[10:])
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				params[strings.ToLower(parts[0])] = strings.TrimSpace(parts[1])
			}
			inSystem, inTemplate = false, false
		case strings.HasPrefix(upper, "MESSAGE "):
			rest := strings.TrimSpace(trimmed[8:])
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				messages = append(messages, map[string]string{
					"role":    strings.ToLower(parts[0]),
					"content": strings.TrimSpace(parts[1]),
				})
			}
			inSystem, inTemplate = false, false
		case strings.HasPrefix(upper, "ADAPTER "):
			adapter = strings.TrimSpace(trimmed[8:])
			inSystem, inTemplate = false, false
		default:
			if inSystem {
				sysLines = append(sysLines, line)
			} else if inTemplate {
				tmplLines = append(tmplLines, line)
			} else {
				inSystem, inTemplate = false, false
			}
		}
	}
	if len(sysLines) > 0 {
		system = strings.Join(sysLines, "\n")
	}
	if len(tmplLines) > 0 {
		template = strings.Join(tmplLines, "\n")
	}
	return
}

// ── POST /api/push ────────────────────────────────────────────────────────────

func handleOllamaPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	jsonError(w, 501, "push to registry not supported by Vortelio")
}

// ── Blob management ───────────────────────────────────────────────────────────

func handleOllamaBlobs(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/api/blobs/")
	if digest == "" {
		jsonError(w, 400, "digest required in path")
		return
	}

	bpath, err := blobPath(digest)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}

	switch r.Method {
	case http.MethodHead:
		if _, err := os.Stat(bpath); err != nil {
			w.WriteHeader(404)
		} else {
			w.WriteHeader(200)
		}

	case http.MethodPost:
		if err := os.MkdirAll(filepath.Dir(bpath), 0755); err != nil {
			jsonError(w, 500, "cannot create blob directory: "+err.Error())
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, 400, "failed to read body: "+err.Error())
			return
		}
		// Verify digest
		sum := sha256.Sum256(body)
		actual := "sha256:" + hex.EncodeToString(sum[:])
		if actual != digest {
			jsonError(w, 400, fmt.Sprintf("digest mismatch: expected %s, got %s", digest, actual))
			return
		}
		if err := os.WriteFile(bpath, body, 0644); err != nil {
			jsonError(w, 500, "failed to write blob: "+err.Error())
			return
		}
		w.WriteHeader(201)

	default:
		jsonError(w, 405, "HEAD or POST only")
	}
}

// ── GET /api/tags (Ollama-compat alias for /api/models) ──────────────────────

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "GET only")
		return
	}
	store := hub.NewModelStore()
	models, err := store.List()
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	type tagModel struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		ModifiedAt time.Time `json:"modified_at"`
		Size       int64     `json:"size"`
		Digest     string    `json:"digest"`
		Details    any       `json:"details"`
	}
	var out []tagModel
	for _, m := range models {
		id := ollamaModelID(m)
		out = append(out, tagModel{
			Name:       id,
			Model:      id,
			ModifiedAt: m.DownloadedAt,
			Size:       m.SizeBytes,
			Digest:     "sha256:" + strings.Repeat("0", 64),
			Details: map[string]string{
				"format":             m.Format,
				"family":             m.Name,
				"parameter_size":     m.Parameters,
				"quantization_level": "",
			},
		})
	}
	if out == nil {
		out = []tagModel{}
	}
	respond(w, 200, map[string]interface{}{"models": out})
}

// ── POST /api/show ────────────────────────────────────────────────────────────

func handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model   string `json:"model"`
		Verbose bool   `json:"verbose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	m, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	mf := m.Modelfile
	if mf == "" {
		mf = "FROM " + m.LocalPath + "\n"
		if m.SystemOverride != "" {
			mf += "SYSTEM " + m.SystemOverride + "\n"
		}
		if m.Template != "" {
			mf += "TEMPLATE \"" + m.Template + "\"\n"
		}
		if m.ModelParameters != nil {
			for k, v := range m.ModelParameters {
				mf += "PARAMETER " + k + " " + v + "\n"
			}
		}
	}

	tmpl := m.Template
	if tmpl == "" {
		tmpl = "{{ if .System }}<|system|>\n{{ .System }}<|end|>\n{{ end }}{{ if .Prompt }}<|user|>\n{{ .Prompt }}<|end|>\n{{ end }}<|assistant|>\n"
	}

	// Build parameters string (Ollama format)
	var paramLines []string
	for k, v := range m.ModelParameters {
		paramLines = append(paramLines, k+" "+v)
	}
	paramsStr := strings.Join(paramLines, "\n")

	respond(w, 200, map[string]interface{}{
		"modelfile":  mf,
		"parameters": paramsStr,
		"template":   tmpl,
		"details": map[string]string{
			"parent_model":       "",
			"format":             m.Format,
			"family":             m.Name,
			"families":           "",
			"parameter_size":     m.Parameters,
			"quantization_level": "",
		},
		"model_info": map[string]interface{}{
			"general.name":            m.Name,
			"general.parameter_count": 0,
			"vortelio.local_path":     m.LocalPath,
			"vortelio.type":           m.Type,
			"vortelio.tag":            m.Tag,
			"vortelio.license":        m.License,
			"vortelio.mmproj_path":    m.MmProjPath,
			"vortelio.num_gpu_layers": m.NumGPULayers,
		},
		"capabilities": m.Capabilities,
	})
}

// ── DELETE /api/delete ────────────────────────────────────────────────────────

func handleOllamaDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, 405, "DELETE only")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	m, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	store := hub.NewModelStore()
	ref := &hub.ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
	if err := store.Remove(ref); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	runtime.GlobalModelManager.Unload(m)
	w.WriteHeader(200)
}

// ── POST /api/quantize ────────────────────────────────────────────────────────

func handleOllamaQuantize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model    string `json:"model"`
		Quantize string `json:"quantize"` // e.g. "q4_0", "q4_k_m", "q8_0"
		Output   string `json:"output"`   // optional output model name
		Stream   *bool  `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Quantize == "" {
		jsonError(w, 400, "model and quantize are required")
		return
	}

	model, err := resolveModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}

	// Find llama-quantize binary
	llamaQuant := findLlamaQuantize()
	if llamaQuant == "" {
		jsonError(w, 501, "llama-quantize binary not found; install llama.cpp and add to PATH")
		return
	}

	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	sendStatus := func(status string) {
		line, _ := json.Marshal(map[string]string{"status": status})
		if stream {
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, "%s\n", line)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}

	// Build output path
	outName := req.Output
	if outName == "" {
		outName = model.Name + "-" + strings.ToLower(req.Quantize)
	}
	outPath := model.LocalPath[:len(model.LocalPath)-len(filepath.Ext(model.LocalPath))] +
		"-" + strings.ToLower(req.Quantize) + ".gguf"

	sendStatus("quantizing " + req.Model + " to " + req.Quantize)

	cmd := exec.Command(llamaQuant, model.LocalPath, outPath, strings.ToUpper(req.Quantize))
	outBytes, runErr := cmd.CombinedOutput()
	if runErr != nil {
		errMsg := string(outBytes)
		if errMsg == "" {
			errMsg = runErr.Error()
		}
		line, _ := json.Marshal(map[string]string{"status": "error", "error": errMsg})
		fmt.Fprintf(w, "%s\n", line)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	// Register the new quantized model
	fi, statErr := os.Stat(outPath)
	if statErr == nil {
		newModel := *model
		newModel.Name = outName
		newModel.Tag = strings.ToLower(req.Quantize)
		newModel.LocalPath = outPath
		newModel.SizeBytes = fi.Size()
		newModel.DownloadedAt = time.Now()
		store := hub.NewModelStore()
		store.Save(&newModel)
	}

	sendStatus("success")
	if !stream {
		respond(w, 200, map[string]string{"status": "success"})
	}
}

func findLlamaQuantize() string {
	names := []string{"llama-quantize", "llama-quantize.exe", "quantize", "quantize.exe"}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range names {
			if p := filepath.Join(dir, name); fileExists(p) {
				return p
			}
			if p := filepath.Join(dir, "bin", name); fileExists(p) {
				return p
			}
		}
	}
	if home, _ := os.UserHomeDir(); home != "" {
		for _, name := range names {
			if p := filepath.Join(home, ".vortelio", "bin", name); fileExists(p) {
				return p
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ── Context continuation (stateless session store) ────────────────────────────

// contextSessions maps a context hash (returned as []int) to conversation history.
// This lets clients reuse conversation state across /api/generate calls.
var contextSessions = struct {
	mu   sync.Mutex
	data map[string][]map[string]string
}{data: make(map[string][]map[string]string)}

// contextToKey converts a []int context token array to a string key.
func contextToKey(ctx []int) string {
	if len(ctx) == 0 {
		return ""
	}
	parts := make([]string, len(ctx))
	for i, v := range ctx {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// contextFromHistory returns a stable []int "token array" representing a session.
// We use a simple encoding: hash the history key and return a sequence of ints.
func contextFromKey(key string) []int {
	if key == "" {
		return nil
	}
	h := sha256.Sum256([]byte(key))
	out := make([]int, 8)
	for i := 0; i < 8; i++ {
		out[i] = int(h[i*4])<<24 | int(h[i*4+1])<<16 | int(h[i*4+2])<<8 | int(h[i*4+3])
	}
	return out
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// iterdir is used in Modelfile parsing — kept for potential future use.
var _ = strconv.Itoa
