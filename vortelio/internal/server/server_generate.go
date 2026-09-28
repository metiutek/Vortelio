package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/history"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		jsonError(w, 400, "model is required")
		return
	}
	if req.Steps == 0 {
		req.Steps = 20
	}

	ref, err := hub.ParseModelRef(req.Model)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	store := hub.NewModelStore()
	model, err := store.Resolve(ref)
	if err != nil {
		jsonError(w, 404, fmt.Sprintf("model %q not found. Download it first with vortelio pull", req.Model))
		return
	}

	hw := getHardware()
	if req.ForceCPU {
		hw.Backend = runtime.BackendCPU
	}

	runner, err := runtime.NewRunner(model, hw)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}

	streamBool := req.Stream == nil || *req.Stream
	opts := &runtime.RunOptions{
		Prompt: req.Prompt, InputFile: req.InputFile, OutputFile: req.OutputFile,
		Steps: req.Steps, Stream: streamBool, ForceCPU: req.ForceCPU,
		ContextSize: req.ContextSize,
	}

	if model.Type == "llm" {
		handleGenerateLLM(w, r, model, hw, opts, req)
		return
	}

	if _, ok := map[string]string{"image": "png", "audio": "wav", "video": "mp4", "3d": "obj"}[model.Type]; ok {
		handleGenerateMedia(w, r, model, runner, opts, req)
		return
	}

	if err := runner.Run(opts); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]interface{}{"model": req.Model, "status": "done"})
}

// estTokens is a rough token estimate (~4 chars per token).
func estTokens(s string) int { return (len(s) + 3) / 4 }

// compactStringMessages is the Ollama-path equivalent of compactMessages: it
// keeps a system message and trims the oldest turns to fit the context window.
func compactStringMessages(msgs []map[string]string, ctxSize int) []map[string]string {
	if ctxSize <= 0 {
		ctxSize = 4096
	}
	budget := ctxSize - ctxSize/3
	if budget < 256 {
		budget = 256
	}
	total := 0
	for _, m := range msgs {
		total += estTokens(m["content"]) + 4
	}
	if total <= budget || len(msgs) <= 2 {
		return msgs
	}
	// Preserve a leading system message, trim oldest of the rest.
	var sys []map[string]string
	rest := msgs
	if len(msgs) > 0 && msgs[0]["role"] == "system" {
		sys = msgs[:1]
		rest = msgs[1:]
		total -= 0
	}
	for total > budget && len(rest) > 1 {
		total -= estTokens(rest[0]["content"]) + 4
		rest = rest[1:]
	}
	return append(append([]map[string]string{}, sys...), rest...)
}

// compactMessages keeps the conversation within the model's context window by
// dropping the oldest turns when the history would overflow. It reserves room for
// the system prompt, the new prompt and the reply, and leaves a short note in
// place of the trimmed turns. Cheap and reliable for small local models.
func compactMessages(messages []map[string]interface{}, system, prompt string, ctxSize int) []map[string]interface{} {
	if len(messages) == 0 {
		return messages
	}
	reserve := ctxSize / 3
	if reserve > 1024 {
		reserve = 1024
	}
	if reserve < 256 {
		reserve = 256
	}
	budget := ctxSize - reserve - estTokens(system) - estTokens(prompt)
	if budget < 256 {
		budget = 256
	}
	total := 0
	for _, m := range messages {
		if c, ok := m["content"].(string); ok {
			total += estTokens(c) + 4
		}
	}
	if total <= budget {
		return messages
	}
	dropped := 0
	for total > budget && len(messages) > 1 {
		if c, ok := messages[0]["content"].(string); ok {
			total -= estTokens(c) + 4
		}
		messages = messages[1:]
		dropped++
	}
	if dropped > 0 {
		note := map[string]interface{}{"role": "system",
			"content": fmt.Sprintf("[Note: %d earlier messages were omitted to stay within the model's context window. Summarise from what remains.]", dropped)}
		messages = append([]map[string]interface{}{note}, messages...)
	}
	return messages
}

func handleGenerateLLM(w http.ResponseWriter, r *http.Request, model *hub.Model, hw *runtime.Hardware, opts *runtime.RunOptions, req GenerateRequest) {
	// Models imported from Ollama may use an engine/format llama.cpp can't load
	// (e.g. Gemma 3n, Qwen3). If the local Ollama server is running, forward the
	// chat to it — it already runs these models. This is the "it worked before"
	// path: Vortelio acting as an Ollama-compatible front-end.
	if strings.HasPrefix(model.Source, "ollama-import") {
		if streamFromOllama(w, model, req) {
			return
		}
		// If Ollama isn't reachable we fall through and let llama.cpp try.
	}

	// Parse format field
	var formatStr string
	if req.Format != nil {
		var s string
		if json.Unmarshal(req.Format, &s) == nil {
			formatStr = s
		} else {
			formatStr = string(req.Format)
		}
	}

	llmOpts := ollamaOptions(req.Options)

	// Determine streaming mode: default true for /api/generate
	streaming := req.Stream == nil || *req.Stream

	// Build NDJSON emitter helpers
	modelID := req.Model
	writeNDJSON := func(obj map[string]interface{}) {
		line, _ := json.Marshal(obj)
		w.Write(line)
		w.Write([]byte("\n"))
	}
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	if streaming {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}

	// Build messages: restore context session if client provides Context tokens
	var messages []map[string]interface{}
	if len(req.Messages) > 0 {
		for _, m := range req.Messages {
			messages = append(messages, map[string]interface{}{
				"role": m.Role, "content": m.Content,
			})
		}
	} else if len(req.Context) > 0 {
		// Restore prior conversation from context session store
		if hist := restoreContext(req.Context); hist != nil {
			for _, m := range hist {
				messages = append(messages, map[string]interface{}{
					"role": m["role"], "content": m["content"],
				})
			}
		}
	}

	// Agentic mode implies tools are on.
	toolsOn := req.ToolsEnabled || req.Agentic != nil

	// If tools are requested but the model can't do native function-calling, we
	// never hard-fail. We silently drop the tools and answer as a plain chat, so a
	// beginner is never blocked — but we emit one friendly inline notice so the
	// missing web/agentic activity is not a mystery. (Previously this returned a
	// hard error with no answer, which read as "the chat is broken".)
	if toolsOn && !runtime.ModelSupportsTools(req.Model) {
		req.Agentic = nil
		req.ToolsEnabled = false
		toolsOn = false
		if streaming {
			w.Header().Set("Content-Type", "application/x-ndjson")
			notice, _ := json.Marshal(map[string]string{
				"level": "info",
				"text":  "This model can't use tools, so I answered without web search or agentic actions. For those, pick a tool-capable model such as Llama 3.2, Qwen2.5/3, or Mistral.",
			})
			writeNDJSON(map[string]interface{}{
				"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"event": "notice", "data": json.RawMessage(notice), "done": false,
			})
			flush()
		}
	}

	// Apply enabled skills as system-prompt augmentation.
	systemPrompt := req.System
	if req.Agentic != nil && len(req.Agentic.Skills) > 0 {
		systemPrompt = applySkills(systemPrompt, req.Agentic.Skills)
	}
	// Smart/auto mode: nudge the model to use its tools on its own.
	if req.Agentic != nil && req.Agentic.Auto {
		systemPrompt = autoSystemPrompt(systemPrompt)
	}
	// Autonomous goal mode: drive a long self-directed loop toward the objective.
	if req.Agentic != nil && req.Agentic.Autonomous {
		systemPrompt = autonomousSystemPrompt(systemPrompt)
	}
	// Tell the agent its working directory so it writes files in the right place.
	if ws := workspaceContext(req.Agentic); ws != "" {
		systemPrompt = ws + "\n\n" + systemPrompt
	}

	// Compact the conversation for small context windows: drop the oldest turns
	// (leaving a note) so small models don't overflow near saturation.
	ctxBudget := req.ContextSize
	if ctxBudget <= 0 {
		ctxBudget = 4096
	}
	messages = compactMessages(messages, systemPrompt, req.Prompt, ctxBudget)

	sopts := runtime.StreamOpts{
		Prompt:       req.Prompt,
		System:       systemPrompt,
		Messages:     messages,
		Images:       req.Images,
		Raw:          req.Raw,
		Think:        req.Think,
		ToolsEnabled: toolsOn,
		Options:      llmOpts,
		Format:       formatStr,
	}
	// Autonomous goal sessions get a much larger tool-round budget so the agent
	// can build a whole project across many steps in one go.
	if req.Agentic != nil && req.Agentic.Autonomous {
		sopts.MaxToolRounds = 40
	}

	// Always capture reasoning: models like Qwen 3.5, Gemma 4 or gpt-oss think
	// on their own (llama-server returns it separately), so the GUI must show
	// it even when the "Think" toggle — which only asks non-reasoning models to
	// reason — is off.
	var thinkBuf strings.Builder
	{
		sopts.ThinkEmit = func(token string) {
			thinkBuf.WriteString(token)
			if streaming {
				writeNDJSON(map[string]interface{}{
					"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
					"thinking": token, "done": false,
				})
				flush()
			}
		}
	}

	var toolEmitter runtime.ToolEventEmitter
	if toolsOn {
		toolEmitter = func(eventType string, data interface{}) {
			evJSON, _ := json.Marshal(data)
			if streaming {
				writeNDJSON(map[string]interface{}{
					"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
					"event": eventType, "data": json.RawMessage(evJSON), "done": false,
				})
				flush()
			}
		}
	}

	// Build the composite tool provider for agentic requests.
	if req.Agentic != nil {
		sopts.ToolProvider = buildAgenticProvider(req.Agentic, toolEmitter)
	}

	var llmRunner *runtime.LLMRunner

	// Route every request through the ModelManager so the llama-server stays
	// warm between messages (keyed by model + context size). Previously a
	// non-zero context size spawned a fresh server and killed it on return,
	// which reloaded the model on every message and caused intermittent
	// failures under VRAM/port contention.
	ka := parseKeepAlive(req.KeepAlive)
	var loadErr error
	llmRunner, loadErr = runtime.GlobalModelManager.GetOrLoadWithContext(model, hw, ka, req.ContextSize)
	if loadErr != nil {
		jsonError(w, 500, loadErr.Error())
		return
	}

	if !streaming {
		// Non-streaming: collect full response then send single JSON object
		var response strings.Builder
		err := llmRunner.StreamWithOpts(sopts, func(token string) {
			response.WriteString(token)
		}, toolEmitter)
		// Save context session
		newCtx := saveContext(req.Context, req.Prompt, response.String(), messages)
		obj := map[string]interface{}{
			"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"response": response.String(), "done": true, "done_reason": "stop",
			"context": newCtx,
		}
		if thinkBuf.Len() > 0 {
			obj["thinking"] = thinkBuf.String()
		}
		if err != nil {
			obj["done_reason"] = "error"
			obj["error"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		writeNDJSON(obj)
		if req.Prompt != "" && response.Len() > 0 && !req.Incognito {
			convID := fmt.Sprintf("%d", time.Now().UnixNano())
			history.Append(
				history.Entry{ID: convID, Model: req.Model, Role: "user", Content: req.Prompt},
				history.Entry{ID: convID, Model: req.Model, Role: "assistant", Content: response.String()},
			)
		}
		return
	}

	// Streaming: emit token-per-line NDJSON
	var response strings.Builder
	err := llmRunner.StreamWithOpts(sopts, func(token string) {
		response.WriteString(token)
		writeNDJSON(map[string]interface{}{
			"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"response": token, "done": false,
		})
		flush()
	}, toolEmitter)

	newCtx := saveContext(req.Context, req.Prompt, response.String(), messages)
	doneObj := map[string]interface{}{
		"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"response": "", "done": true, "done_reason": "stop",
		"context": newCtx,
	}
	if thinkBuf.Len() > 0 {
		doneObj["thinking"] = thinkBuf.String()
	}
	if err != nil {
		doneObj["done_reason"] = "error"
		doneObj["error"] = err.Error()
	}
	writeNDJSON(doneObj)
	flush()

	if req.Prompt != "" && response.Len() > 0 && !req.Incognito {
		convID := fmt.Sprintf("%d", time.Now().UnixNano())
		history.Append(
			history.Entry{ID: convID, Model: req.Model, Role: "user", Content: req.Prompt},
			history.Entry{ID: convID, Model: req.Model, Role: "assistant", Content: response.String()},
		)
	}
}

func handleGenerateMedia(w http.ResponseWriter, r *http.Request, model *hub.Model, runner runtime.Runner, opts *runtime.RunOptions, req GenerateRequest) {
	mediaExt := map[string]string{"image": "png", "audio": "wav", "video": "mp4", "3d": "obj"}
	mediaMime := map[string]string{"image": "image/png", "audio": "audio/wav", "video": "video/mp4", "3d": "model/obj"}
	ext := mediaExt[model.Type]

	if opts.OutputFile == "" {
		tmp, err := os.CreateTemp("", "vortelio-out-*."+ext)
		if err != nil {
			jsonError(w, 500, "cannot create temporary file: "+err.Error())
			return
		}
		tmp.Close()
		opts.OutputFile = tmp.Name()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, canFlush := w.(http.Flusher)

	sseEvent := func(typ, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
		if canFlush {
			flusher.Flush()
		}
	}

	ctx := r.Context()
	progCh := make(chan runtime.ProgressEvent, 32)
	done := make(chan error, 1)

	go func() {
		switch model.Type {
		case "image":
			if ir, ok := runner.(*runtime.ImageRunner); ok {
				done <- ir.RunWithProgress(opts, progCh)
				return
			}
		case "audio":
			if ar, ok := runner.(*runtime.AudioRunner); ok {
				done <- ar.RunWithProgress(opts, progCh)
				return
			}
		case "video":
			if vr, ok := runner.(*runtime.VideoRunner); ok {
				done <- vr.RunWithProgress(opts, progCh)
				return
			}
		case "3d":
			if tr, ok := runner.(*runtime.ThreeDRunner); ok {
				done <- tr.RunWithProgress(opts, progCh)
				return
			}
		}
		done <- runner.Run(opts)
		close(progCh)
	}()

	cancelled := false
	for ev := range progCh {
		if cancelled {
			continue // keep draining so the runner goroutine never blocks
		}
		select {
		case <-ctx.Done():
			sseEvent("error", `{"error":"generation cancelled"}`)
			cancelled = true
			continue
		default:
		}
		progJSON, _ := json.Marshal(map[string]interface{}{"pct": ev.Percent, "msg": ev.Message})
		sseEvent("progress", string(progJSON))
	}
	if cancelled {
		<-done
		return
	}

	if runErr := <-done; runErr != nil {
		errJSON, _ := json.Marshal(map[string]string{"error": runErr.Error()})
		sseEvent("error", string(errJSON))
		return
	}

	time.Sleep(200 * time.Millisecond)
	data, err := os.ReadFile(opts.OutputFile)
	if err != nil {
		errJSON, _ := json.Marshal(map[string]string{"error": "cannot read output: " + err.Error()})
		sseEvent("error", string(errJSON))
		return
	}
	// A runner that gave up without an error (e.g. a missing dependency) leaves
	// the pre-created temp file empty. Report that instead of a blank result.
	if len(data) == 0 {
		errJSON, _ := json.Marshal(map[string]string{
			"error": "the " + model.Type + " backend produced no output — check the server log for the missing dependency",
		})
		sseEvent("error", string(errJSON))
		return
	}
	// Keep every generation: a fixed name would overwrite the previous result.
	dlDir := runtime.DefaultOutputDir()
	dlPath := filepath.Join(dlDir, fmt.Sprintf("vortelio-%s-%s.%s", model.Type, time.Now().Format("20060102-150405"), ext))
	if err := os.WriteFile(dlPath, data, 0644); err != nil {
		dlPath = ""
	}

	resultJSON, _ := json.Marshal(map[string]interface{}{
		"model": req.Model, "status": "done", "type": model.Type,
		"mime":     mediaMime[model.Type],
		"data":     base64.StdEncoding.EncodeToString(data),
		"saved_to": dlPath,
	})
	sseEvent("result", string(resultJSON))
}

// restoreContext looks up a prior conversation session by context token array.
func restoreContext(ctx []int) []map[string]string {
	key := contextToKey(ctx)
	if key == "" {
		return nil
	}
	contextSessions.mu.Lock()
	defer contextSessions.mu.Unlock()
	return contextSessions.data[key]
}

// saveContext stores the conversation turn and returns a new context token array.
func saveContext(prevCtx []int, prompt, response string, existingMsgs []map[string]interface{}) []int {
	if prompt == "" || response == "" {
		return nil
	}

	// Restore prior history
	var history []map[string]string
	if len(prevCtx) > 0 {
		history = restoreContext(prevCtx)
	}
	// Append from existing messages if any
	if len(existingMsgs) > 0 {
		history = nil
		for _, m := range existingMsgs {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			history = append(history, map[string]string{"role": role, "content": content})
		}
	}
	history = append(history,
		map[string]string{"role": "user", "content": prompt},
		map[string]string{"role": "assistant", "content": response},
	)
	// Trim to last 20 messages to avoid unbounded growth
	if len(history) > 20 {
		history = history[len(history)-20:]
	}

	newCtx := contextFromKey(fmt.Sprintf("%d", time.Now().UnixNano()))
	key := contextToKey(newCtx)

	contextSessions.mu.Lock()
	contextSessions.data[key] = history
	contextSessions.mu.Unlock()

	return newCtx
}

// streamFromOllama forwards a chat to the local Ollama server for models that
// were imported from Ollama (and that llama.cpp may not be able to load itself,
// e.g. Gemma 3n / Qwen3). Returns true if it handled the request (Ollama was
// reachable); false means Ollama is down and the caller should try llama.cpp.
func streamFromOllama(w http.ResponseWriter, model *hub.Model, req GenerateRequest) bool {
	port := config.Get().OllamaPort
	if port == 0 {
		port = 11434
	}
	base := fmt.Sprintf("http://localhost:%d", port)

	// Reachability probe — if Ollama isn't running, let the caller fall through.
	probe, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + "/api/tags")
	if err != nil {
		return false
	}
	probe.Body.Close()

	ollamaModel := model.Name
	if model.Tag != "" {
		ollamaModel += ":" + model.Tag
	}

	var msgs []map[string]string
	if s := strings.TrimSpace(req.System); s != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": s})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	if req.Prompt != "" {
		msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})
	}
	msgs = compactStringMessages(msgs, req.ContextSize)

	streaming := req.Stream == nil || *req.Stream
	modelID := req.Model
	writeNDJSON := func(obj map[string]interface{}) {
		line, _ := json.Marshal(obj)
		w.Write(line)
		w.Write([]byte("\n"))
	}
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// attempt runs one /api/chat call. forceCPU passes options.num_gpu=0 so a
	// model whose GPU offload OOMs (e.g. Gemma 3n on a small card) runs on CPU
	// instead. It only writes to w once it has real output, so an OOM detected
	// before any token lets us cleanly retry on CPU. Returns (handled, oom).
	attempt := func(forceCPU bool) (handled bool, oom bool) {
		payload := map[string]interface{}{"model": ollamaModel, "messages": msgs, "stream": streaming}
		if forceCPU {
			payload["options"] = map[string]interface{}{"num_gpu": 0}
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(base+"/api/chat", "application/json", bytes.NewReader(body))
		if err != nil {
			return false, false
		}
		defer resp.Body.Close()

		isOOM := func(s string) bool {
			s = strings.ToLower(s)
			return strings.Contains(s, "out of memory") || strings.Contains(s, "cuda error")
		}

		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			if !forceCPU && isOOM(string(b)) {
				return false, true
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeNDJSON(map[string]interface{}{"model": modelID, "done": true, "done_reason": "error",
				"error": "Ollama could not run this model: " + strings.TrimSpace(string(b))})
			flush()
			return true, false
		}

		var full strings.Builder
		headersSet := false
		ensureHeaders := func() {
			if headersSet {
				return
			}
			headersSet = true
			if streaming {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.Header().Set("Cache-Control", "no-cache")
			} else {
				w.Header().Set("Content-Type", "application/json")
			}
			if forceCPU && streaming {
				notice, _ := json.Marshal(map[string]string{"level": "info",
					"text": "Running this model on CPU (it doesn't fit your GPU) — replies may be slower."})
				writeNDJSON(map[string]interface{}{"model": modelID, "event": "notice", "data": json.RawMessage(notice), "done": false})
				flush()
			}
		}

		dec := json.NewDecoder(resp.Body)
		for {
			var chunk struct {
				Message struct {
					Content  string `json:"content"`
					Thinking string `json:"thinking"`
				} `json:"message"`
				Done  bool   `json:"done"`
				Error string `json:"error"`
			}
			if err := dec.Decode(&chunk); err != nil {
				break
			}
			if chunk.Message.Thinking != "" && streaming {
				ensureHeaders()
				writeNDJSON(map[string]interface{}{"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
					"thinking": chunk.Message.Thinking, "done": false})
				flush()
			}
			if chunk.Error != "" {
				if !forceCPU && !headersSet && isOOM(chunk.Error) {
					return false, true // clean retry on CPU — nothing written yet
				}
				ensureHeaders()
				writeNDJSON(map[string]interface{}{"model": modelID, "done": true, "done_reason": "error", "error": chunk.Error})
				flush()
				return true, false
			}
			if c := chunk.Message.Content; c != "" {
				ensureHeaders()
				full.WriteString(c)
				if streaming {
					writeNDJSON(map[string]interface{}{"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
						"response": c, "done": false})
					flush()
				}
			}
			if chunk.Done {
				break
			}
		}

		ensureHeaders()
		doneObj := map[string]interface{}{"model": modelID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"done": true, "done_reason": "stop"}
		if streaming {
			doneObj["response"] = ""
		} else {
			doneObj["response"] = full.String()
		}
		writeNDJSON(doneObj)
		flush()

		if req.Prompt != "" && full.Len() > 0 && !req.Incognito {
			convID := fmt.Sprintf("%d", time.Now().UnixNano())
			history.Append(
				history.Entry{ID: convID, Model: req.Model, Role: "user", Content: req.Prompt},
				history.Entry{ID: convID, Model: req.Model, Role: "assistant", Content: full.String()},
			)
		}
		return true, false
	}

	handled, oom := attempt(false)
	if !handled && oom {
		// GPU offload OOM'd in Ollama — retry on CPU (slower, but it runs).
		handled, _ = attempt(true)
	}
	if !handled {
		return false // Ollama became unreachable mid-call; let llama.cpp try
	}
	return true
}
