package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string, null, or array of content parts
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type openAIChatRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	Stream         *bool           `json:"stream"`
	Temperature    *float64        `json:"temperature"`
	TopP           *float64        `json:"top_p"`
	MaxTokens      *int            `json:"max_tokens"`
	Seed           *int            `json:"seed"`
	Stop           json.RawMessage `json:"stop"`
	Tools          json.RawMessage `json:"tools"`       // OpenAI-style tool definitions
	ToolChoice     json.RawMessage `json:"tool_choice"` // "auto", "none", or specific
	ResponseFormat *struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"json_schema"`
	} `json:"response_format"`
}

type openAICompletionRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	Suffix      string   `json:"suffix"`
	Stream      *bool    `json:"stream"`
	Temperature *float64 `json:"temperature"`
	MaxTokens   *int     `json:"max_tokens"`
}

// ── GET /v1/models ────────────────────────────────────────────────────────────

func handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
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

	type oaiModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []oaiModel
	for _, m := range models {
		data = append(data, oaiModel{
			ID:      ollamaModelID(m),
			Object:  "model",
			Created: m.DownloadedAt.Unix(),
			OwnedBy: "vortelio",
		})
	}
	// Also advertise configured cloud models (provider/model), so the /v1
	// endpoint is a unified catalogue of everything usable in Vortelio.
	for _, cm := range cloud.ModelsWithKeys() {
		data = append(data, oaiModel{
			ID:      cm.Provider + "/" + cm.Model,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: cm.Provider,
		})
	}
	if data == nil {
		data = []oaiModel{}
	}
	respond(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// ── GET /v1/models/:id ────────────────────────────────────────────────────────

func handleOpenAIModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "GET only")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if name == "" {
		handleOpenAIModels(w, r)
		return
	}
	m, err := resolveModel(name)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	respond(w, 200, map[string]interface{}{
		"id":       ollamaModelID(m),
		"object":   "model",
		"created":  m.DownloadedAt.Unix(),
		"owned_by": "vortelio",
	})
}

// ── POST /v1/chat/completions ─────────────────────────────────────────────────

func handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, 400, "could not read body: "+err.Error())
		return
	}
	var req openAIChatRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}

	model, err := resolveModel(req.Model)
	if err != nil {
		// Not a local model — try routing it to a configured cloud provider so
		// /v1 is a unified gateway (local + cloud). Model ids are "provider/model".
		if proxyCloudChatCompletion(w, req.Model, rawBody) {
			return
		}
		jsonError(w, 404, err.Error())
		return
	}

	hw := getHardware()
	runner, err := runtime.GlobalModelManager.GetOrLoad(model, hw, 0)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	// Build options
	opts := runtime.LLMOptions{}
	if req.Temperature != nil {
		opts.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		opts.TopP = *req.TopP
	}
	if req.MaxTokens != nil {
		opts.MaxTokens = *req.MaxTokens
	}
	if req.Seed != nil {
		opts.Seed = *req.Seed
	}

	// Format
	var formatStr string
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "json_object":
			formatStr = "json"
		case "json_schema":
			if req.ResponseFormat.Schema != nil {
				formatStr = string(req.ResponseFormat.Schema)
			} else {
				formatStr = "json"
			}
		}
	}

	// Convert messages — preserve all fields so tool_calls and tool results pass through
	var messages []map[string]interface{}
	for _, msg := range req.Messages {
		m := map[string]interface{}{"role": msg.Role}
		if len(msg.Content) > 0 {
			var c interface{}
			if json.Unmarshal(msg.Content, &c) == nil {
				m["content"] = c
			} else {
				m["content"] = string(msg.Content)
			}
		}
		if msg.ToolCallID != "" {
			m["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			m["name"] = msg.Name
		}
		if len(msg.ToolCalls) > 0 {
			var tc interface{}
			if json.Unmarshal(msg.ToolCalls, &tc) == nil {
				m["tool_calls"] = tc
			}
		}
		messages = append(messages, m)
	}

	sopts := runtime.StreamOpts{
		Messages:    messages,
		Options:     opts,
		Format:      formatStr,
		ClientTools: req.Tools,
	}

	stream := false
	if req.Stream != nil {
		stream = *req.Stream
	}

	modelID := ollamaModelID(model)
	reqID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// tool_calls captured for non-streaming path
	var capturedToolCalls []map[string]interface{}

	if req.Tools != nil {
		sopts.ToolCallEmit = func(calls []runtime.ToolCall) {
			oaiCalls := make([]map[string]interface{}, len(calls))
			for i, tc := range calls {
				oaiCalls[i] = map[string]interface{}{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				}
			}
			if stream {
				// stream each tool_call as a delta chunk
				flusher, canFlush := w.(http.Flusher)
				for i, tc := range oaiCalls {
					delta := map[string]interface{}{"tool_calls": []map[string]interface{}{
						{"index": i, "id": calls[i].ID, "type": "function",
							"function": map[string]string{
								"name":      calls[i].Function.Name,
								"arguments": calls[i].Function.Arguments,
							}},
					}}
					toolReason := "tool_calls"
					chunk := map[string]interface{}{
						"id": reqID, "object": "chat.completion.chunk",
						"created": created, "model": modelID,
						"choices": []map[string]interface{}{
							{"index": 0, "delta": delta, "finish_reason": toolReason},
						},
					}
					_ = tc
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					if canFlush {
						flusher.Flush()
					}
				}
			} else {
				capturedToolCalls = oaiCalls
			}
		}
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, canFlush := w.(http.Flusher)

		sendChunk := func(content string, finishReason *string) {
			delta := map[string]interface{}{}
			if content != "" {
				delta["content"] = content
			}
			if finishReason == nil && content == "" {
				delta["role"] = "assistant"
			}
			chunk := map[string]interface{}{
				"id": reqID, "object": "chat.completion.chunk",
				"created": created, "model": modelID,
				"choices": []map[string]interface{}{
					{"index": 0, "delta": delta, "finish_reason": finishReason},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if canFlush {
				flusher.Flush()
			}
		}

		sendChunk("", nil) // role chunk

		runErr := runner.StreamWithOpts(sopts, func(token string) {
			sendChunk(token, nil)
		}, nil)

		stop := "stop"
		if runErr != nil {
			errMsg := "error"
			sendChunk("[ERROR] "+runErr.Error(), &errMsg)
		} else if sopts.ToolCallEmit == nil {
			sendChunk("", &stop)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if canFlush {
			flusher.Flush()
		}
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

	// Tool calls path
	if len(capturedToolCalls) > 0 {
		respond(w, 200, map[string]interface{}{
			"id": reqID, "object": "chat.completion",
			"created": created, "model": modelID,
			"choices": []map[string]interface{}{{
				"index": 0,
				"message": map[string]interface{}{
					"role": "assistant", "content": nil,
					"tool_calls": capturedToolCalls,
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
		return
	}

	respond(w, 200, map[string]interface{}{
		"id": reqID, "object": "chat.completion",
		"created": created, "model": modelID,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": fullResp.String()},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

// ── POST /v1/completions ──────────────────────────────────────────────────────

func handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req openAICompletionRequest
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
	runner, err := runtime.GlobalModelManager.GetOrLoad(model, hw, 0)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	opts := runtime.LLMOptions{}
	if req.Temperature != nil {
		opts.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		opts.MaxTokens = *req.MaxTokens
	}

	// For completions, wrap the prompt as a user message
	// If suffix is set, include it as a hint in system prompt (FIM approximation)
	sysPrompt := "You are a helpful AI assistant. Complete the following text."
	if req.Suffix != "" {
		sysPrompt += fmt.Sprintf(" The completion should naturally lead into: %q", req.Suffix)
	}
	messages := []map[string]interface{}{
		{"role": "system", "content": sysPrompt},
		{"role": "user", "content": req.Prompt},
	}

	sopts := runtime.StreamOpts{
		Messages: messages,
		Options:  opts,
	}

	stream := false
	if req.Stream != nil {
		stream = *req.Stream
	}

	modelID := ollamaModelID(model)
	reqID := fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, canFlush := w.(http.Flusher)

		runErr := runner.StreamWithOpts(sopts, func(token string) {
			chunk := map[string]interface{}{
				"id":      reqID,
				"object":  "text_completion",
				"created": created,
				"model":   modelID,
				"choices": []map[string]interface{}{
					{"text": token, "index": 0, "finish_reason": nil},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if canFlush {
				flusher.Flush()
			}
		}, nil)

		stop := "stop"
		_ = stop
		if runErr != nil {
			finalChunk := map[string]interface{}{
				"id": reqID, "object": "text_completion", "created": created, "model": modelID,
				"choices": []map[string]interface{}{{"text": "", "index": 0, "finish_reason": "error"}},
			}
			data, _ := json.Marshal(finalChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if canFlush {
			flusher.Flush()
		}
		return
	}

	var fullResp strings.Builder
	runErr := runner.StreamWithOpts(sopts, func(token string) {
		fullResp.WriteString(token)
	}, nil)
	if runErr != nil {
		jsonError(w, 500, runErr.Error())
		return
	}
	respond(w, 200, map[string]interface{}{
		"id":      reqID,
		"object":  "text_completion",
		"created": created,
		"model":   modelID,
		"choices": []map[string]interface{}{
			{"text": fullResp.String(), "index": 0, "finish_reason": "stop"},
		},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

// ── POST /v1/embeddings ───────────────────────────────────────────────────────

func handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
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
	runner, err := runtime.GlobalModelManager.GetOrLoad(model, hw, 0)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	embs, err := runner.EmbedBatch(inputs)
	if err != nil {
		jsonError(w, 500, "embedding failed: "+err.Error())
		return
	}

	type embData struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	var data []embData
	for i, emb := range embs {
		data = append(data, embData{Object: "embedding", Index: i, Embedding: emb})
	}
	if data == nil {
		data = []embData{}
	}

	totalTokens := 0
	for _, inp := range inputs {
		totalTokens += len(strings.Fields(inp))
	}

	respond(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
		"model":  ollamaModelID(model),
		"usage": map[string]int{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	})
}
