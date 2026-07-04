package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vortelio/vortelio/internal/cloud"
	rt "github.com/vortelio/vortelio/internal/runtime"
)

// ── BYOK (bring your own key) cloud models ────────────────────────────────────
//
// Anyone can use cloud models by entering their own provider API key — no
// subscription. Keys are stored locally (encrypted on Windows via DPAPI) by the
// cloud package; requests are made directly to the provider with the user's key
// and streamed back as a uniform SSE stream of {"delta": "..."} events.

// The curated per-provider model list lives in the cloud package as
// cloud.ModelChoices so the unified /v1 gateway and the Open Code config
// generator can share it.

// GET /api/cloud/providers
// Lists providers, whether a key is stored, and the model choices.
// CLICloudModel describes a ready-to-use cloud model for the CLI picker.
type CLICloudModel struct {
	Provider     string
	ProviderName string
	Model        string
	Label        string
}

// CloudModelsForCLI returns the cloud models the user can use (providers with a
// saved API key), for the `vortelio code` /model picker.
func CloudModelsForCLI() []CLICloudModel {
	var out []CLICloudModel
	for _, p := range cloud.Providers {
		if cloud.LoadKey(p.ID) == "" {
			continue
		}
		choices := cloud.ModelChoices[p.ID]
		if len(choices) == 0 {
			choices = [][2]string{{p.DefaultModel, p.DefaultModel}}
		}
		for _, c := range choices {
			out = append(out, CLICloudModel{Provider: p.ID, ProviderName: p.Name, Model: c[0], Label: c[1]})
		}
	}
	return out
}

// RunCLICloudTurn streams one cloud chat turn for the CLI, using the same agentic
// harness (tools) as the GUI. Returns the full assistant text.
func RunCLICloudTurn(providerID, model, workdir, mode string, autonomous, mcpOn bool, skills []string, history []map[string]string, onToken func(string), emit rt.ToolEventEmitter, approve func(tool, summary, args string) bool, ask func(question string, options []string) string) (string, error) {
	p, ok := cloud.FindProvider(providerID)
	if !ok {
		return "", fmt.Errorf("provider cloud sconosciuto: %s", providerID)
	}
	keys := cloud.LoadKeys(providerID)
	if len(keys) == 0 {
		return "", fmt.Errorf("nessuna API key per %s", p.Name)
	}
	if model != "" {
		p.DefaultModel = model
		if p.Format == cloud.FormatGemini {
			p.BaseURL = "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
		}
	}
	prov, sys := BuildCLIHarness(workdir, mode, autonomous, mcpOn, skills, emit, approve, ask)
	msgs := []cloud.Message{}
	if sys != "" {
		msgs = append(msgs, cloud.Message{Role: "system", Content: sys})
	}
	for _, m := range history {
		msgs = append(msgs, cloud.Message{Role: m["role"], Content: m["content"]})
	}
	toolOpts := &cloud.ToolCallOptions{Tools: prov.Tools(), ExecTool: prov.Execute, OnEvent: emit}
	if autonomous {
		toolOpts.MaxRounds = 40
	} else {
		// Interactive ask/plan turns still need room to read several files before
		// answering; the default of 5 was too low (model ran out mid-exploration).
		toolOpts.MaxRounds = 16
	}
	return cloud.ChatWithToolsFailover(p, keys, msgs, toolOpts, onToken)
}

// GET /api/media/providers — media (image/audio/video/3d) cloud services + key state.
func handleMediaProviders(w http.ResponseWriter, r *http.Request) {
	type out struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Type         string `json:"type"`
		DefaultModel string `json:"default_model"`
		KeyHint      string `json:"key_hint"`
		HasKey       bool   `json:"has_key"`
	}
	res := make([]out, 0, len(cloud.MediaProviders))
	for _, p := range cloud.MediaProviders {
		res = append(res, out{p.ID, p.Name, p.Type, p.DefaultModel, p.KeyHint, cloud.LoadKey(p.ID) != ""})
	}
	respond(w, 200, map[string]interface{}{"providers": res})
}

// POST/DELETE /api/media/key — {provider, key} save or remove a media API key.
func handleMediaKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, 400, "invalid request")
		return
	}
	if _, ok := cloud.FindMediaProvider(req.Provider); !ok {
		jsonError(w, 400, "unknown media provider")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		cloud.DeleteKey(req.Provider)
		respond(w, 200, map[string]interface{}{"ok": true, "has_key": false})
	case http.MethodPost:
		if req.Key == "" {
			jsonError(w, 400, "key required")
			return
		}
		if err := cloud.SaveKey(req.Provider, req.Key); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]interface{}{"ok": true, "has_key": true})
	default:
		jsonError(w, 405, "use POST or DELETE")
	}
}

// normalizeChatURL turns a user-supplied server address into a full OpenAI-style
// chat-completions URL. Accepts "host:port", "http://host:port", ".../v1", or a
// full ".../chat/completions" URL.
func normalizeChatURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	if strings.Contains(u, "/chat/completions") {
		return u
	}
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/v1") {
		return u + "/chat/completions"
	}
	return u + "/v1/chat/completions"
}

func handleCloudProviders(w http.ResponseWriter, r *http.Request) {
	type modelOut struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	type providerOut struct {
		ID       string     `json:"id"`
		Name     string     `json:"name"`
		KeyHint  string     `json:"key_hint"`
		HasKey   bool       `json:"has_key"`
		KeyCount int        `json:"key_count"`
		MaxKeys  int        `json:"max_keys"`
		Models   []modelOut `json:"models"`
	}
	out := make([]providerOut, 0, len(cloud.Providers))
	for _, p := range cloud.Providers {
		models := []modelOut{}
		if choices, ok := cloud.ModelChoices[p.ID]; ok {
			for _, c := range choices {
				models = append(models, modelOut{ID: c[0], Label: c[1]})
			}
		} else {
			models = append(models, modelOut{ID: p.DefaultModel, Label: p.DefaultModel})
		}
		n := len(cloud.LoadKeys(p.ID))
		out = append(out, providerOut{
			ID:       p.ID,
			Name:     p.Name,
			KeyHint:  p.KeyHint,
			HasKey:   n > 0,
			KeyCount: n,
			MaxKeys:  cloud.MaxKeysPerProvider,
			Models:   models,
		})
	}
	respond(w, 200, map[string]interface{}{"providers": out})
}

// POST   /api/cloud/key   {"provider":"openai","key":"sk-..."}  → save
// DELETE /api/cloud/key   {"provider":"openai"}                 → remove
func handleCloudKey(w http.ResponseWriter, r *http.Request) {
	// GET returns the stored keys so the edit form can show/pre-fill them. Served
	// only on the (local) server; the keys belong to the user on this machine.
	if r.Method == http.MethodGet {
		provider := r.URL.Query().Get("provider")
		if _, ok := cloud.FindProvider(provider); !ok {
			jsonError(w, 400, "unknown provider")
			return
		}
		respond(w, 200, map[string]interface{}{"provider": provider, "keys": cloud.LoadKeys(provider)})
		return
	}
	var req struct {
		Provider string   `json:"provider"`
		Key      string   `json:"key"`  // single key (backward compatible)
		Keys     []string `json:"keys"` // up to 5 keys for failover
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		jsonError(w, 400, "invalid request")
		return
	}
	if req.Provider == "" {
		jsonError(w, 400, "provider required")
		return
	}
	if _, ok := cloud.FindProvider(req.Provider); !ok {
		jsonError(w, 400, "unknown provider")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := cloud.DeleteKey(req.Provider); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]interface{}{"ok": true, "has_key": false, "key_count": 0})
	case http.MethodPost:
		keys := req.Keys
		if len(keys) == 0 && req.Key != "" {
			keys = []string{req.Key}
		}
		var clean []string
		for _, k := range keys {
			if strings.TrimSpace(k) != "" {
				clean = append(clean, k)
			}
		}
		if len(clean) == 0 {
			jsonError(w, 400, "key required")
			return
		}
		if err := cloud.SaveKeys(req.Provider, clean); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]interface{}{"ok": true, "has_key": true, "key_count": len(cloud.LoadKeys(req.Provider))})
	default:
		jsonError(w, 405, "method not allowed")
	}
}

// POST /api/cloud/chat
// Body: {"provider":"openai","model":"gpt-4o","messages":[{"role","content"}]}
// Streams SSE: data: {"delta":"..."}  then  data: [DONE]  (or data: {"error":"..."}).
func handleCloudChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "method not allowed")
		return
	}
	var req struct {
		Provider string          `json:"provider"`
		Model    string          `json:"model"`
		Messages []cloud.Message `json:"messages"`
		Agentic  *AgenticConfig  `json:"agentic"`
		System   string          `json:"system"`
		BaseURL  string          `json:"base_url"` // custom/self-hosted endpoint (vast.ai, home server, Ollama)
		Key      string          `json:"key"`      // optional key for the custom endpoint
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, 400, "invalid request")
		return
	}
	if len(req.Messages) == 0 {
		jsonError(w, 400, "messages required")
		return
	}

	var p cloud.Provider
	var keys []string
	if req.Provider == "custom" || req.BaseURL != "" {
		// Custom / self-hosted OpenAI- or Ollama-compatible endpoint.
		bu := normalizeChatURL(req.BaseURL)
		if bu == "" {
			jsonError(w, 400, "base_url required for a custom server")
			return
		}
		p = cloud.Provider{ID: "custom", Name: "Custom server", BaseURL: bu,
			AuthHeader: "Authorization", AuthPrefix: "Bearer ", Format: cloud.FormatOpenAI, DefaultModel: req.Model}
		keys = []string{req.Key} // may be empty for no-auth home servers
	} else {
		var ok bool
		p, ok = cloud.FindProvider(req.Provider)
		if !ok {
			jsonError(w, 400, "unknown provider")
			return
		}
		keys = cloud.LoadKeys(req.Provider)
		if len(keys) == 0 {
			jsonError(w, 400, fmt.Sprintf("no API key for %s — add your own key in Cloud Models", p.Name))
			return
		}
		// Override the model if the caller picked one.
		if req.Model != "" {
			p.DefaultModel = req.Model
			if p.Format == cloud.FormatGemini {
				p.BaseURL = "https://generativelanguage.googleapis.com/v1beta/models/" + req.Model + ":generateContent"
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, canFlush := w.(http.Flusher)
	emit := func(obj interface{}) {
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if canFlush {
			flusher.Flush()
		}
	}

	// Apply enabled skills as a system-prompt augmentation, prepended as a
	// system message so cloud providers receive it.
	systemPrompt := req.System
	if req.Agentic != nil && len(req.Agentic.Skills) > 0 {
		systemPrompt = applySkills(systemPrompt, req.Agentic.Skills)
	}
	if req.Agentic != nil && req.Agentic.Auto {
		systemPrompt = autoSystemPrompt(systemPrompt)
	}
	if req.Agentic != nil && req.Agentic.Autonomous {
		systemPrompt = autonomousSystemPrompt(systemPrompt)
	}
	if ws := workspaceContext(req.Agentic); ws != "" {
		systemPrompt = ws + "\n\n" + systemPrompt
	}
	if systemPrompt != "" {
		req.Messages = append([]cloud.Message{{Role: "system", Content: systemPrompt}}, req.Messages...)
	}

	// Build agentic tool options when requested. The tool event emitter doubles
	// as the approval-request channel for risky coding tools.
	var toolOpts *cloud.ToolCallOptions
	if req.Agentic != nil {
		toolEmit := func(eventType string, data interface{}) {
			b, _ := json.Marshal(data)
			emit(map[string]interface{}{"event": eventType, "data": json.RawMessage(b)})
		}
		provider := buildAgenticProvider(req.Agentic, toolEmit)
		if tools := provider.Tools(); len(tools) > 0 {
			toolOpts = &cloud.ToolCallOptions{
				Tools:    tools,
				ExecTool: provider.Execute,
				OnEvent:  toolEmit,
			}
			if req.Agentic.Autonomous {
				toolOpts.MaxRounds = 40
			}
		}
	}

	_, err := cloud.ChatWithToolsFailover(p, keys, req.Messages, toolOpts, func(tok string) {
		emit(map[string]string{"delta": tok})
	})
	if err != nil {
		emit(map[string]string{"error": err.Error()})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}
}
