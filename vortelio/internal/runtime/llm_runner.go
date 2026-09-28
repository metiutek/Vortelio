package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
)

const VERSION = "0.1.1"

type LLMRunner struct {
	model          *hub.Model
	hw             *Hardware
	proc           *exec.Cmd
	apiURL         string
	currentCtxSize int
	history        []HistEntry
	thinkMode      bool
	toolsEnabled   bool
}

// ToolEventEmitter is an optional callback for streaming tool-related events to the UI.
type ToolEventEmitter func(eventType string, data interface{})

func NewLLMRunner(model *hub.Model, hw *Hardware) *LLMRunner {
	return &LLMRunner{model: model, hw: hw}
}

func (r *LLMRunner) Run(opts *RunOptions) error {
	// Route non-GGUF models to Python transformers
	modelExt := strings.ToLower(filepath.Ext(r.model.LocalPath))
	modelFmt := strings.ToLower(r.model.Format)
	if modelExt == ".safetensors" || modelExt == ".bin" || modelExt == ".pt" ||
		modelFmt == "safetensors" || modelFmt == "transformers" {
		return r.runWithTransformers(opts)
	}

	if opts.Prompt == "" && opts.InputFile == "" {
		return r.runInteractive(opts)
	}
	prompt := opts.Prompt
	if opts.InputFile != "" {
		data, err := os.ReadFile(opts.InputFile)
		if err != nil {
			return fmt.Errorf("file read failed: %w", err)
		}
		prompt = string(data)
	}
	if err := r.ensureServer(); err != nil {
		return err
	}
	defer r.stopServer()
	_, err := r.chatAPI(nil, prompt, opts)
	return err
}

type HistEntry struct{ User, Asst string }
type histEntry = HistEntry

func (r *LLMRunner) runInteractive(opts *RunOptions) error {
	if err := r.ensureServer(); err != nil {
		return err
	}
	defer r.stopServer()

	// Handle Ctrl+C: stop server and exit cleanly
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nInterruzione...")
		r.stopServer()
		os.Exit(0)
	}()

	fmt.Printf("💬  Chat con %s/%s:%s\n", r.model.Type, r.model.Name, r.model.Tag)
	fmt.Println("    Scrivi il tuo messaggio e premi Invio. Scrivi 'esci' per uscire.")

	scanner := bufio.NewScanner(os.Stdin)
	var history []histEntry

	for {
		fmt.Print("Tu: ")
		if !scanner.Scan() {
			fmt.Println()
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "esci" || input == "exit" || input == "quit" {
			break
		}

		fmt.Print("AI: ")
		response, err := r.chatAPI(history, input, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  %v\n", err)
			continue
		}
		fmt.Println()
		fmt.Println()

		history = append(history, histEntry{User: input, Asst: response})
		if len(history) > 8 {
			history = history[1:]
		}
	}
	return nil
}

// ensureServer starts llama-server if not already running and waits for it to be ready.
func (r *LLMRunner) ensureServer() error {
	srv := LlamaServerBin()
	if srv == "" {
		srv = r.findBin("llama-server", "llama-server.exe")
	}
	if srv == "" {
		// First run (typical on Linux/macOS pip installs): fetch llama.cpp now
		// instead of failing silently.
		fmt.Println("📦  llama.cpp not found — downloading it (one-time)…")
		if _, err := InstallLlamaCpp(r.hw, CLIProgress()); err != nil {
			return fmt.Errorf("llama.cpp is not installed and the automatic download failed (%v). "+
				"Check your connection and run `vortelio setup`", err)
		}
		fmt.Println()
		if srv = LlamaServerBin(); srv == "" {
			return fmt.Errorf("llama.cpp was downloaded but llama-server was not found in %s", LlamaDir())
		}
	}
	// Never launch while an update is replacing the binaries.
	llamaInstallMu.RLock()
	defer llamaInstallMu.RUnlock()

	// Pre-flight: the model file must exist. Ollama-imported models point straight
	// at ~/.ollama/models/blobs, which vanishes when Ollama is uninstalled or its
	// blobs are pruned. llama-server then exits instantly with no output, which
	// used to surface as "too large for this machine" — misleading, and the CPU
	// retry cannot fix it either.
	if err := r.checkModelFile(); err != nil {
		return err
	}

	// Determine GPU layers: model override > hardware auto-detect
	autoGL := 0
	switch r.hw.Backend {
	case BackendCUDA:
		// Fit as many layers as the card can actually hold (like Ollama), based on
		// the model's size — not a flat per-VRAM number that OOMs on big models.
		autoGL = gpuLayersForModel(r.hw.VRAM, r.model.SizeBytes)
	case BackendMetal:
		autoGL = 999
	}
	forcedGL := r.model.NumGPULayers > 0
	forcedCPU := r.model.NumGPULayers < 0 // remembered: this model only loads on CPU here
	if forcedGL {
		autoGL = r.model.NumGPULayers
	}
	if forcedCPU {
		autoGL = 0 // go straight to CPU — skip the slow GPU attempt
	}

	ctxSize := 4096
	if r.currentCtxSize > 0 {
		ctxSize = r.currentCtxSize
	}
	if v, ok := r.model.ModelParameters["num_ctx"]; ok {
		if n, err := fmt.Sscanf(v, "%d", new(int)); n == 1 && err == nil {
			var parsed int
			fmt.Sscanf(v, "%d", &parsed)
			if parsed > 0 {
				ctxSize = parsed
			}
		}
	}

	// attempt starts llama-server with nGL GPU layers and waits up to deadlineSecs
	// for the model to become healthy. On any failure it kills the process and
	// returns an error so the caller can retry with a safer configuration.
	attempt := func(nGL, deadlineSecs int) error {
		port, err := freePort()
		if err != nil {
			return fmt.Errorf("could not find a free port: %w", err)
		}
		r.apiURL = fmt.Sprintf("http://127.0.0.1:%d", port)

		args := []string{
			"--model", r.model.LocalPath,
			"--port", fmt.Sprintf("%d", port),
			"--host", "127.0.0.1",
			"--ctx-size", fmt.Sprintf("%d", ctxSize),
			"--n-gpu-layers", fmt.Sprintf("%d", nGL),
			"-np", "1",
		}

		// Multimodal projector (LLaVA, BakLLaVA, etc.)
		if r.model.MmProjPath != "" {
			if _, statErr := os.Stat(r.model.MmProjPath); statErr == nil {
				args = append(args, "--mmproj", r.model.MmProjPath)
			}
		}
		// Flash attention
		if mp, ok := r.model.ModelParameters["flash_attn"]; ok && (mp == "true" || mp == "1") {
			args = append(args, "--flash-attn")
		}
		// mmap
		if mp, ok := r.model.ModelParameters["use_mmap"]; ok && (mp == "false" || mp == "0") {
			args = append(args, "--no-mmap")
		}
		// numa
		if mp, ok := r.model.ModelParameters["numa"]; ok && (mp == "true" || mp == "1") {
			args = append(args, "--numa", "distribute")
		}
		// threads
		if mp, ok := r.model.ModelParameters["num_thread"]; ok && mp != "" {
			args = append(args, "--threads", mp)
		}

		cmd := HideWindow(exec.Command(srv, args...))
		// Capture the tail of llama-server's output (it writes load errors to both
		// stdout and stderr) so we can report the REAL failure — unsupported
		// architecture, bad file, OOM — instead of guessing.
		sb := &tailBuf{max: 16384}
		cmd.Stdout = sb
		cmd.Stderr = sb

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("llama-server start failed: %w\n    Path: %s", err, srv)
		}
		r.proc = cmd

		// Watch for early process exit (e.g. VRAM OOM, bad model) so we can fail
		// over fast instead of polling /health until the deadline.
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()

		// Wait until /health returns {"status":"ok"} — during loading it returns
		// 200 {"status":"loading model"}, so we must wait for "ok" specifically.
		deadline := time.Now().Add(time.Duration(deadlineSecs) * time.Second)
		client := &http.Client{Timeout: 2 * time.Second}
		for time.Now().Before(deadline) {
			select {
			case <-exited:
				if reason := classifyLoadError(sb.String()); reason != "" {
					return loadError{reason: reason, fatal: isFatalLoadError(sb.String())}
				}
				return fmt.Errorf("llama-server exited during load")
			case <-time.After(500 * time.Millisecond):
			}
			resp, err := client.Get(r.apiURL + "/health")
			if err != nil {
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var health struct {
				Status string `json:"status"`
			}
			json.Unmarshal(body, &health)
			switch health.Status {
			case "ok":
				return nil
			case "error":
				return fmt.Errorf("llama-server reported an error: %s", string(body))
			}
		}
		if r.proc != nil && r.proc.Process != nil {
			r.proc.Process.Kill()
		}
		<-exited // let Wait finish so stderr is fully flushed
		if reason := classifyLoadError(sb.String()); reason != "" {
			return loadError{reason: reason, fatal: isFatalLoadError(sb.String())}
		}
		return fmt.Errorf("timeout: model did not become ready within %ds", deadlineSecs)
	}

	// First try with the auto/forced GPU layers. Keep this window short so a model
	// that is too big for the GPU fails over to CPU quickly instead of hanging.
	fmt.Println("⏳  Loading model…")
	err := attempt(autoGL, 60)
	if err == nil {
		fmt.Println("✅  Model ready")
		return nil
	}

	// If the GPU attempt failed — most often VRAM OOM on a model too large for the
	// card — fall back to CPU so the chat still works instead of hard-failing.
	// Slower, but it always answers. We skip CPU retry when the user forced a
	// GPU-layer count, or when the failure is FATAL (bad file / unsupported model
	// architecture) — CPU can't fix those, so retrying just wastes time.
	if le, ok := err.(loadError); ok && le.fatal {
		return fmt.Errorf("%s", le.reason)
	}
	if autoGL > 0 && !forcedGL {
		fmt.Printf("⚠️  GPU load failed (%v).\n    Retrying on CPU — slower, but it will work…\n", err)
		if cpuErr := attempt(0, 600); cpuErr == nil {
			// Remember that this model only loads on CPU on this machine, so future
			// loads skip the slow GPU attempt (no more long "Starting…" wait).
			r.model.NumGPULayers = -1
			_ = hub.NewModelStore().Save(r.model)
			fmt.Println("✅  Model ready (CPU)")
			return nil
		} else {
			err = cpuErr
		}
	}
	if le, ok := err.(loadError); ok {
		return fmt.Errorf("%s", le.reason)
	}
	return fmt.Errorf("this model could not be loaded. Two common causes: it is too large for this machine (%s) — try a smaller model; "+
		"or it uses a newer architecture (e.g. Gemma 3n) that the bundled llama.cpp can't read yet — update llama.cpp (run `vortelio setup`, "+
		"or replace the binaries in ~/.vortelio/bin with a recent llama.cpp release). [%v]", r.hw.String(), err)
}

func (r *LLMRunner) stopServer() {
	if r.proc != nil && r.proc.Process != nil {
		r.proc.Process.Kill()
		r.proc = nil
	}
	r.apiURL = ""
}

// chatAPI sends a message via /v1/chat/completions with SSE streaming.
func (r *LLMRunner) chatAPI(history []histEntry, userMsg string, opts *RunOptions) (string, error) {
	messages := []map[string]string{
		(func() map[string]string {
			sysPrompt := "You are a helpful and precise AI assistant."
			if r.thinkMode {
				sysPrompt = "You are a helpful and precise AI assistant. " +
					"Before answering, reason step by step, wrapping your reasoning " +
					"between <think> and </think> tags. Then give the final answer."
			}
			return map[string]string{"role": "system", "content": sysPrompt}
		}()),
	}
	for _, h := range history {
		messages = append(messages,
			map[string]string{"role": "user", "content": h.User},
			map[string]string{"role": "assistant", "content": h.Asst},
		)
	}
	messages = append(messages, map[string]string{"role": "user", "content": userMsg})

	payload := map[string]interface{}{
		"model":          "local",
		"messages":       messages,
		"max_tokens":     512,
		"temperature":    0.7,
		"repeat_penalty": 1.1,
		"stream":         true,
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(r.apiURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("server connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server error %d: %s", resp.StatusCode, string(b))
	}

	// Parse SSE stream: lines like "data: {...}" or "data: [DONE]"
	var full strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64*1024)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		token := chunk.Choices[0].Delta.Content
		if token != "" {
			fmt.Print(token)
			full.WriteString(token)
		}
		if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason == "stop" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return full.String(), fmt.Errorf("stream read error: %w", err)
	}

	return strings.TrimSpace(full.String()), nil
}

// findBin searches for any of the given binary names in PATH and known dirs.
func (r *LLMRunner) findBin(names ...string) string {
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		self := filepath.Dir(exe)
		dirs = append(dirs, self, filepath.Join(self, "bin"))
	}
	if runtime.GOOS == "windows" {
		for _, pf := range []string{os.Getenv("PROGRAMFILES"), `C:\Program Files`} {
			if pf != "" {
				dirs = append(dirs, filepath.Join(pf, "Vortelio", "bin"), filepath.Join(pf, "Vortelio"))
			}
		}
	} else if runtime.GOOS == "darwin" {
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	} else {
		dirs = append(dirs, "/usr/local/bin", "/usr/bin")
	}
	if home, _ := os.UserHomeDir(); home != "" {
		dirs = append(dirs, filepath.Join(home, ".vortelio", "bin"))
	}
	for _, dir := range dirs {
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return filepath.Join(dir, name)
			}
		}
	}
	return ""
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func gpuLayersForVRAM(vram int64) int {
	gb := float64(vram) / 1e9
	switch {
	case gb >= 16:
		return 99
	case gb >= 8:
		return 35
	case gb >= 6:
		return 28
	case gb >= 4:
		return 18
	default:
		return 0
	}
}

// gpuLayersForModel decides how many transformer layers to offload to the GPU
// based on BOTH the available VRAM and the model's size — the way Ollama does it.
// A flat per-VRAM number (gpuLayersForVRAM) tries to push the same count onto the
// card regardless of model size, which OOMs on large models and forces a slow
// full-CPU fallback. Here we instead fit as many layers as the card can hold and
// let llama.cpp run the rest on the CPU (partial offload), which is much faster.
func gpuLayersForModel(vram, modelBytes int64) int {
	if vram <= 0 {
		return 0
	}
	if modelBytes <= 0 {
		return gpuLayersForVRAM(vram) // unknown size — fall back to the old table
	}
	// Reserve VRAM for the KV cache, compute buffers and driver overhead.
	const overhead = 1024 * 1024 * 1024 // ~1 GB
	budget := int64(float64(vram)*0.92) - overhead
	if budget <= 0 {
		return 0 // not enough VRAM for any layer — run on CPU
	}
	// We don't know the exact layer count up-front, so estimate per-layer weight
	// size assuming a typical ~40-block transformer. n = budget / perLayer.
	const assumedLayers = 40
	perLayer := modelBytes / assumedLayers
	if perLayer <= 0 {
		return 0
	}
	n := int(budget / perLayer)
	if n <= 0 {
		return 0
	}
	if n > 999 {
		n = 999 // model fits entirely — offload everything (llama.cpp clamps)
	}
	return n
}

// tailBuf keeps only the last `max` bytes written to it — used to capture the
// tail of llama-server's stderr without growing unbounded while it runs.
type tailBuf struct {
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}
func (t *tailBuf) String() string { return string(t.b) }

// loadError carries a human-readable reason for a model-load failure, plus a
// `fatal` flag meaning a CPU retry can't help (bad file / unsupported model).
type loadError struct {
	reason string
	fatal  bool
}

func (e loadError) Error() string { return e.reason }

// isFatalLoadError reports whether the llama-server log shows a failure that a
// CPU retry won't fix (the model file is unreadable or its architecture isn't
// supported by this llama.cpp build).
func isFatalLoadError(log string) bool {
	l := strings.ToLower(log)
	return strings.Contains(l, "wrong number of tensors") ||
		strings.Contains(l, "unknown model architecture") ||
		strings.Contains(l, "unsupported model architecture") ||
		strings.Contains(l, "unknown architecture") ||
		strings.Contains(l, "no such file") ||
		strings.Contains(l, "failed to open")
}

// checkModelFile verifies the model file is still on disk before we bother
// starting llama-server, and explains what to do when it is not.
func (r *LLMRunner) checkModelFile() error {
	ref := r.model.Name
	if r.model.Tag != "" {
		ref = ref + ":" + r.model.Tag
	}
	if r.model.LocalPath == "" {
		return fmt.Errorf("%s has no model file recorded on disk. Re-download it: vortelio pull %s", ref, ref)
	}
	if _, err := os.Stat(r.model.LocalPath); err != nil {
		hint := ""
		if strings.HasPrefix(r.model.Source, "ollama-import") {
			hint = "\n    This model was imported from Ollama and only referenced its blob; " +
				"the blob is gone (Ollama uninstalled, or its blobs pruned)."
		}
		return fmt.Errorf("the file for %s is missing:\n    %s%s\n"+
			"    Re-download it (vortelio pull %s), or drop the entry (vortelio remove %s).",
			ref, r.model.LocalPath, hint, ref, ref)
	}
	return nil
}

// classifyLoadError turns a raw llama-server stderr tail into a short, clear
// explanation for the user. Returns "" when nothing recognisable is found.
func classifyLoadError(log string) string {
	l := strings.ToLower(log)
	switch {
	case strings.Contains(l, "wrong number of tensors") ||
		strings.Contains(l, "unknown model architecture") ||
		strings.Contains(l, "unsupported model architecture") ||
		strings.Contains(l, "unknown architecture"):
		return archUnsupportedMsg
	case strings.Contains(l, "out of memory") ||
		strings.Contains(l, "cudamalloc") ||
		strings.Contains(l, "failed to allocate") ||
		strings.Contains(l, "insufficient memory"):
		return "Not enough memory to load this model. Try a smaller model, or it will run on CPU."
	case strings.Contains(l, "no such file") || strings.Contains(l, "failed to open"):
		return "The model file could not be opened. It may be missing or corrupted — re-download it."
	case strings.Contains(l, "error loading model") || strings.Contains(l, "failed to load model"):
		return "llama-server could not load this model file."
	}
	return ""
}

// archUnsupportedMsg is returned when the installed llama.cpp is too old for a
// model; ModelManager recognises it and updates llama.cpp automatically.
const archUnsupportedMsg = "This model's architecture isn't supported by the installed llama.cpp engine. " +
	"Update llama.cpp (Settings → Engine, or run `vortelio setup --force`) to run this model."

// IsArchUnsupported reports whether err means "llama.cpp is too old for this model".
func IsArchUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), archUnsupportedMsg)
}

// CLIProgress prints a single updating progress line on the terminal.
func CLIProgress() func(string, float64) {
	last := ""
	return func(msg string, pct float64) {
		line := fmt.Sprintf("   %s %3.0f%%", msg, pct*100)
		if line != last {
			fmt.Printf("\r%-70s", line)
			last = line
		}
	}
}

// NewLLMRunnerForServer creates an LLMRunner for use by the HTTP server.
func (r *LLMRunner) SetContextSize(n int)     { r.currentCtxSize = n }
func (r *LLMRunner) SetThink(v bool)          { r.thinkMode = v }
func (r *LLMRunner) SetHistory(h []HistEntry) { r.history = h }
func (r *LLMRunner) SetToolsEnabled(v bool)   { r.toolsEnabled = v }

func NewLLMRunnerForServer(model *hub.Model, hw *Hardware) *LLMRunner {
	return &LLMRunner{model: model, hw: hw}
}

// StreamToWriter starts llama-server (if needed), sends a prompt, and streams
// each token to the provided callback function.
// When toolEventEmitter is non-nil, it can emit tool call/result events.
func (r *LLMRunner) StreamToWriter(opts *RunOptions, emit func(string)) error {
	return r.StreamToWriterWithTools(opts, emit, nil)
}

// StreamToWriterWithTools is the full version that supports tool use.
func (r *LLMRunner) StreamToWriterWithTools(opts *RunOptions, emit func(string), toolEmit ToolEventEmitter) error {
	// Route safetensors / non-GGUF models to Python transformers
	modelExt := strings.ToLower(filepath.Ext(r.model.LocalPath))
	modelFmt := strings.ToLower(r.model.Format)
	if modelExt == ".safetensors" || modelExt == ".bin" || modelExt == ".pt" ||
		modelFmt == "safetensors" || modelFmt == "transformers" {
		return r.streamWithTransformers(opts, emit)
	}

	if r.apiURL == "" {
		if err := r.ensureServer(); err != nil {
			return err
		}
	}

	// Build messages: system + history + current prompt
	sysPrompt := "You are a helpful and precise AI assistant."
	if r.thinkMode {
		sysPrompt = "You are a helpful and precise AI assistant. " +
			"Before answering, reason step by step, wrapping your reasoning " +
			"between <think> and </think> tags. Then give the final answer."
	}
	if r.toolsEnabled {
		sysPrompt += " You have access to tools. Use them when helpful to provide accurate answers."
	}

	// Use []interface{} for messages so we can include tool messages with extra fields
	messages := []interface{}{
		map[string]string{"role": "system", "content": sysPrompt},
	}
	for _, h := range r.history {
		messages = append(messages, map[string]string{"role": "user", "content": h.User})
		if h.Asst != "" {
			messages = append(messages, map[string]string{"role": "assistant", "content": h.Asst})
		}
	}
	messages = append(messages, map[string]string{"role": "user", "content": opts.Prompt})

	// Tool-calling loop: may iterate if the LLM requests tool calls
	maxToolRounds := 5 // safety limit to prevent infinite loops
	for round := 0; round < maxToolRounds; round++ {
		payload := map[string]interface{}{
			"model":          "local",
			"messages":       messages,
			"max_tokens":     512,
			"temperature":    0.7,
			"repeat_penalty": 1.1,
			"stream":         true,
		}

		// Attach tools definition if enabled
		if r.toolsEnabled {
			payload["tools"] = BuiltinTools()
			payload["tool_choice"] = "auto"
		}

		body, _ := json.Marshal(payload)
		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Post(r.apiURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("server connection failed: %w", err)
		}

		// Accumulate tool calls from the streaming response
		var toolCalls []ToolCall
		toolCallMap := map[int]*ToolCall{}
		var contentBuf strings.Builder
		finishReason := ""

		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}

			choice := chunk.Choices[0]

			// Stream text content tokens
			if choice.Delta.Content != "" {
				emit(choice.Delta.Content)
				contentBuf.WriteString(choice.Delta.Content)
			}

			// Accumulate tool call fragments
			for _, tc := range choice.Delta.ToolCalls {
				existing, ok := toolCallMap[tc.Index]
				if !ok {
					newTC := &ToolCall{
						ID:   tc.ID,
						Type: tc.Type,
						Function: ToolCallFunc{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
					toolCallMap[tc.Index] = newTC
				} else {
					// Append streamed fragments
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Function.Name != "" {
						existing.Function.Name = tc.Function.Name
					}
					existing.Function.Arguments += tc.Function.Arguments
				}
			}

			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}
		resp.Body.Close()

		// Collect tool calls in order
		for i := 0; i < len(toolCallMap); i++ {
			if tc, ok := toolCallMap[i]; ok {
				// Generate an ID if the server didn't provide one
				if tc.ID == "" {
					tc.ID = fmt.Sprintf("call_%d_%d", round, i)
				}
				if tc.Type == "" {
					tc.Type = "function"
				}
				toolCalls = append(toolCalls, *tc)
			}
		}

		// If no tool calls, we're done — the model gave a final answer
		if finishReason != "tool_calls" || len(toolCalls) == 0 {
			return nil
		}

		// ── Execute tool calls ─────────────────────────────────────

		// Build the assistant message with tool_calls
		tcJSON := make([]map[string]interface{}, len(toolCalls))
		for i, tc := range toolCalls {
			tcJSON[i] = map[string]interface{}{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]string{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			}
		}
		asstMsg := map[string]interface{}{
			"role":       "assistant",
			"content":    contentBuf.String(),
			"tool_calls": tcJSON,
		}
		messages = append(messages, asstMsg)

		// Execute each tool and add results
		for _, tc := range toolCalls {
			// Emit tool_call event to the UI
			if toolEmit != nil {
				toolEmit("tool_call", map[string]string{
					"id":        tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}

			result, err := ExecuteTool(tc.Function.Name, tc.Function.Arguments)
			resultStr := result
			errStr := ""
			if err != nil {
				errStr = err.Error()
				resultStr = fmt.Sprintf(`{"error":%q}`, err.Error())
			}

			// Emit tool_result event to the UI
			if toolEmit != nil {
				toolEmit("tool_result", ToolResult{
					CallID: tc.ID,
					Name:   tc.Function.Name,
					Result: result,
					Error:  errStr,
				})
			}

			// Add tool result message (truncated to protect the context window)
			messages = append(messages, map[string]string{
				"role":         "tool",
				"content":      TruncateForContext(resultStr, 2000),
				"tool_call_id": tc.ID,
			})
		}

		// Loop continues: re-send messages to the LLM with tool results
	}

	return nil
}

// ── LLMOptions ────────────────────────────────────────────────────────────────

// LLMOptions holds per-request generation parameters forwarded to llama-server.
type LLMOptions struct {
	Temperature   float64
	TopP          float64
	TopK          int
	MaxTokens     int
	RepeatPenalty float64
	RepeatLastN   int
	MinP          float64
	Stop          []string
	NumCtx        int
	Seed          int
	// Hardware options (may trigger server restart if different from loaded config)
	NumGPU           int     // override GPU layers (-1 = full offload, 0 = CPU)
	FlashAttn        bool    // enable flash attention
	Mmap             *bool   // nil = default, true/false = explicit
	Numa             bool    // enable NUMA
	NumThreads       int     // number of CPU threads
	TfsZ             float64 // tail free sampling
	TypicalP         float64 // locally typical sampling
	PresencePenalty  float64
	FrequencyPenalty float64
}

// ── Public API ────────────────────────────────────────────────────────────────

// EnsureServer is the public wrapper around ensureServer.
func (r *LLMRunner) EnsureServer() error { return r.ensureServer() }

// StopServer stops the running llama-server process.
func (r *LLMRunner) StopServer() { r.stopServer() }

// GetAPIURL returns the base URL of the running llama-server.
func (r *LLMRunner) GetAPIURL() string { return r.apiURL }

// ── Embeddings ────────────────────────────────────────────────────────────────

// Embed generates an embedding vector for a single input string.
func (r *LLMRunner) Embed(input string) ([]float64, error) {
	embs, err := r.EmbedBatch([]string{input})
	if err != nil {
		return nil, err
	}
	if len(embs) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return embs[0], nil
}

// EmbedBatch generates embedding vectors for multiple inputs.
// Tries /v1/embeddings first (llama.cpp >= b3000), falls back to legacy /embedding.
func (r *LLMRunner) EmbedBatch(inputs []string) ([][]float64, error) {
	if r.apiURL == "" {
		if err := r.ensureServer(); err != nil {
			return nil, err
		}
	}

	var inp interface{}
	if len(inputs) == 1 {
		inp = inputs[0]
	} else {
		inp = inputs
	}
	payload := map[string]interface{}{"model": "local", "input": inp}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(r.apiURL+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == 200 {
		var result struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBody, &result); err == nil && len(result.Data) > 0 {
			embs := make([][]float64, len(result.Data))
			for _, d := range result.Data {
				if d.Index < len(embs) {
					embs[d.Index] = d.Embedding
				}
			}
			return embs, nil
		}
	}

	// Fallback: legacy /embedding endpoint (one request per input)
	var results [][]float64
	for _, input := range inputs {
		b2, _ := json.Marshal(map[string]string{"content": input})
		r2, err := client.Post(r.apiURL+"/embedding", "application/json", bytes.NewReader(b2))
		if err != nil {
			return nil, fmt.Errorf("embedding fallback failed: %w", err)
		}
		b2data, _ := io.ReadAll(r2.Body)
		r2.Body.Close()
		var res2 struct {
			Embedding []float64 `json:"embedding"`
		}
		if err := json.Unmarshal(b2data, &res2); err != nil {
			return nil, fmt.Errorf("embedding decode failed: %w", err)
		}
		results = append(results, res2.Embedding)
	}
	return results, nil
}

// ── Thread-safe streaming ─────────────────────────────────────────────────────

// StreamOpts holds per-request options for StreamWithOpts.
// Thread-safe: does not mutate runner state.
type StreamOpts struct {
	Prompt        string
	Messages      []map[string]interface{} // nil = use Prompt with default system prompt
	System        string                   // system prompt override (used when Messages is nil)
	Images        []string                 // base64-encoded images for multimodal input
	Raw           bool                     // skip system/chat-template wrapping
	Think         bool
	ThinkEmit     func(string)     // called with thinking tokens (separate from content)
	ToolsEnabled  bool             // enable server-side builtin tool execution loop
	ClientTools   json.RawMessage  // tool definitions from client forwarded to model as-is
	ToolCallEmit  func([]ToolCall) // when set: emit tool_calls to client instead of executing server-side
	ToolProvider  ToolProvider     // when set (with ToolsEnabled): supplies tool defs + execution; defaults to builtins
	Options       LLMOptions
	Format        string // "json" or raw JSON schema string
	MaxToolRounds int    // override the tool-loop round cap (0 = default); raised for autonomous goal sessions
}

// applyModelfileTemplate renders an Ollama-style TEMPLATE with the given StreamOpts.
// Supports: {{.System}}, {{.Prompt}}, {{.Response}}, {{.Messages}}.
func applyModelfileTemplate(tmplSrc string, sopts StreamOpts) (string, error) {
	type msgData struct {
		Role    string
		Content string
	}
	type tmplData struct {
		System   string
		Prompt   string
		Response string
		Messages []msgData
	}

	data := tmplData{Response: ""}

	// System prompt
	data.System = sopts.System

	if sopts.Messages != nil {
		// Multi-turn: build from messages list
		var sb strings.Builder
		for _, m := range sopts.Messages {
			role, _ := m["role"].(string)
			content := ""
			switch v := m["content"].(type) {
			case string:
				content = v
			}
			if role == "system" && data.System == "" {
				data.System = content
				continue
			}
			data.Messages = append(data.Messages, msgData{Role: role, Content: content})
			if role == "user" {
				data.Prompt = content
			}
			_ = sb
		}
	} else {
		data.Prompt = sopts.Prompt
	}

	// Ollama uses Go templates but with {{ }} delimiters
	t, err := template.New("modelfile").Parse(tmplSrc)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// streamWithRawPrompt sends a pre-formatted raw string to llama-server's /completion endpoint.
func (r *LLMRunner) streamWithRawPrompt(prompt string, sopts StreamOpts, emit func(string)) error {
	maxTok := 512
	if sopts.Options.MaxTokens > 0 {
		maxTok = sopts.Options.MaxTokens
	}
	temp := 0.7
	if sopts.Options.Temperature > 0 {
		temp = sopts.Options.Temperature
	}

	payload := map[string]interface{}{
		"prompt":      prompt,
		"n_predict":   maxTok,
		"temperature": temp,
		"stream":      true,
	}
	if sopts.Options.TopP > 0 {
		payload["top_p"] = sopts.Options.TopP
	}
	if sopts.Options.TopK > 0 {
		payload["top_k"] = sopts.Options.TopK
	}
	if sopts.Options.RepeatPenalty > 0 {
		payload["repeat_penalty"] = sopts.Options.RepeatPenalty
	}
	if sopts.Options.Seed != 0 {
		payload["seed"] = sopts.Options.Seed
	}
	if len(sopts.Options.Stop) > 0 {
		payload["stop"] = sopts.Options.Stop
	}

	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(r.apiURL+"/completion", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llama-server /completion failed: %w", err)
	}
	defer resp.Body.Close()

	// ThinkSplitter
	effectiveEmit := emit
	var splitter *thinkSplitter
	if sopts.ThinkEmit != nil {
		splitter = &thinkSplitter{emit: emit, thinkEmit: sopts.ThinkEmit}
		effectiveEmit = splitter.feed
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var chunk struct {
			Content string `json:"content"`
			Stop    bool   `json:"stop"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil {
			if chunk.Content != "" {
				effectiveEmit(chunk.Content)
			}
			if chunk.Stop {
				break
			}
		}
	}
	if splitter != nil {
		splitter.flush()
	}
	return sc.Err()
}

// thinkSplitter routes <think>...</think> tokens to ThinkEmit and the rest to emit.
type thinkSplitter struct {
	inThink   bool
	buf       string
	emit      func(string)
	thinkEmit func(string)
}

func (ts *thinkSplitter) feed(token string) {
	ts.buf += token
	for {
		if !ts.inThink {
			if idx := strings.Index(ts.buf, "<think>"); idx >= 0 {
				if idx > 0 {
					ts.emit(ts.buf[:idx])
				}
				ts.buf = ts.buf[idx+7:]
				ts.inThink = true
			} else {
				safe := len(ts.buf) - 7
				if safe > 0 {
					ts.emit(ts.buf[:safe])
					ts.buf = ts.buf[safe:]
				}
				break
			}
		} else {
			if idx := strings.Index(ts.buf, "</think>"); idx >= 0 {
				if idx > 0 && ts.thinkEmit != nil {
					ts.thinkEmit(ts.buf[:idx])
				}
				ts.buf = ts.buf[idx+8:]
				ts.inThink = false
			} else {
				safe := len(ts.buf) - 8
				if safe > 0 && ts.thinkEmit != nil {
					ts.thinkEmit(ts.buf[:safe])
					ts.buf = ts.buf[safe:]
				} else if safe > 0 {
					ts.buf = ts.buf[safe:]
				}
				break
			}
		}
	}
}

func (ts *thinkSplitter) flush() {
	if ts.buf == "" {
		return
	}
	if ts.inThink {
		if ts.thinkEmit != nil {
			ts.thinkEmit(ts.buf)
		}
	} else {
		ts.emit(ts.buf)
	}
	ts.buf = ""
}

// StreamWithOpts streams an LLM response using explicit per-request options.
// Thread-safe: all request-specific data is passed as parameters, not stored on the runner.
func (r *LLMRunner) StreamWithOpts(sopts StreamOpts, emit func(string), toolEmit ToolEventEmitter) error {
	if r.apiURL == "" {
		return fmt.Errorf("llama-server not running; call EnsureServer first")
	}

	modelExt := strings.ToLower(filepath.Ext(r.model.LocalPath))
	modelFmt := strings.ToLower(r.model.Format)
	if modelExt == ".safetensors" || modelExt == ".bin" || modelExt == ".pt" ||
		modelFmt == "safetensors" || modelFmt == "transformers" {
		return r.streamWithTransformers(&RunOptions{Prompt: sopts.Prompt, Stream: true}, emit)
	}

	// Apply Modelfile TEMPLATE if model has one and raw mode is off
	if !sopts.Raw && r.model.Template != "" {
		formatted, err := applyModelfileTemplate(r.model.Template, sopts)
		if err == nil && formatted != "" {
			// Send as raw prompt — template already handles all formatting
			return r.streamWithRawPrompt(formatted, sopts, emit)
		}
		// On template error: fall through to normal message-based path
	}

	var messages []interface{}
	if sopts.Raw {
		// Raw mode: single user message, no system wrapper
		if sopts.Messages != nil {
			for _, m := range sopts.Messages {
				messages = append(messages, m)
			}
		} else {
			messages = []interface{}{
				map[string]string{"role": "user", "content": sopts.Prompt},
			}
		}
	} else if sopts.Messages != nil {
		for _, m := range sopts.Messages {
			messages = append(messages, m)
		}
	} else {
		sysPrompt := "You are a helpful, precise AI assistant."
		if sopts.System != "" {
			sysPrompt = sopts.System
		} else if r.model.SystemOverride != "" {
			sysPrompt = r.model.SystemOverride
		}
		if sopts.Think {
			sysPrompt += " Before answering, reason step by step enclosed in <think> and </think> tags, then provide your final answer."
		}
		if sopts.ToolsEnabled {
			sysPrompt += " You have access to tools. Use them when appropriate to provide accurate answers."
		}
		var userContent interface{}
		if len(sopts.Images) > 0 {
			parts := []interface{}{map[string]string{"type": "text", "text": sopts.Prompt}}
			for _, img := range sopts.Images {
				parts = append(parts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": "data:image/jpeg;base64," + img},
				})
			}
			userContent = parts
		} else {
			userContent = sopts.Prompt
		}
		messages = []interface{}{
			map[string]string{"role": "system", "content": sysPrompt},
			map[string]interface{}{"role": "user", "content": userContent},
		}
	}

	temp := 0.7
	if sopts.Options.Temperature > 0 {
		temp = sopts.Options.Temperature
	}
	maxTok := 512
	if sopts.Options.MaxTokens > 0 {
		maxTok = sopts.Options.MaxTokens
	}
	repPen := 1.1
	if sopts.Options.RepeatPenalty > 0 {
		repPen = sopts.Options.RepeatPenalty
	}

	// Decide whether to route thinking tokens separately
	effectiveEmit := emit
	var splitter *thinkSplitter
	if sopts.ThinkEmit != nil {
		splitter = &thinkSplitter{emit: emit, thinkEmit: sopts.ThinkEmit}
		effectiveEmit = splitter.feed
	}

	maxToolRounds := 8
	if sopts.MaxToolRounds > 0 {
		maxToolRounds = sopts.MaxToolRounds
	}
	for round := 0; round < maxToolRounds; round++ {
		// On the final round, force a text answer (no tools) so the model can't
		// keep calling tools forever and return an empty response.
		lastRound := round == maxToolRounds-1
		payload := map[string]interface{}{
			"model":          "local",
			"messages":       messages,
			"max_tokens":     maxTok,
			"temperature":    temp,
			"repeat_penalty": repPen,
			"stream":         true,
		}
		if sopts.Options.TopP > 0 {
			payload["top_p"] = sopts.Options.TopP
		}
		if sopts.Options.TopK > 0 {
			payload["top_k"] = sopts.Options.TopK
		}
		if sopts.Options.Seed != 0 {
			payload["seed"] = sopts.Options.Seed
		}
		if sopts.Options.MinP > 0 {
			payload["min_p"] = sopts.Options.MinP
		}
		if sopts.Options.RepeatLastN > 0 {
			payload["repeat_last_n"] = sopts.Options.RepeatLastN
		}
		if len(sopts.Options.Stop) > 0 {
			payload["stop"] = sopts.Options.Stop
		}
		if sopts.Options.NumCtx > 0 {
			payload["n_ctx"] = sopts.Options.NumCtx
		}
		if sopts.Options.TfsZ > 0 {
			payload["tfs_z"] = sopts.Options.TfsZ
		}
		if sopts.Options.TypicalP > 0 {
			payload["typical_p"] = sopts.Options.TypicalP
		}
		if sopts.Options.PresencePenalty != 0 {
			payload["presence_penalty"] = sopts.Options.PresencePenalty
		}
		if sopts.Options.FrequencyPenalty != 0 {
			payload["frequency_penalty"] = sopts.Options.FrequencyPenalty
		}
		if sopts.Options.NumThreads > 0 {
			payload["n_threads"] = sopts.Options.NumThreads
		}
		if sopts.Format == "json" {
			payload["response_format"] = map[string]string{"type": "json_object"}
		} else if sopts.Format != "" {
			var schema interface{}
			if json.Unmarshal([]byte(sopts.Format), &schema) == nil {
				payload["response_format"] = map[string]interface{}{
					"type":        "json_schema",
					"json_schema": schema,
				}
			}
		}
		if len(sopts.ClientTools) > 0 {
			// Client-side tools: forward raw JSON to model
			var clientTools interface{}
			if json.Unmarshal(sopts.ClientTools, &clientTools) == nil {
				payload["tools"] = clientTools
				payload["tool_choice"] = "auto"
			}
		} else if sopts.ToolsEnabled && !lastRound {
			// Server-side tools via the request's provider (defaults to builtins).
			tp := sopts.ToolProvider
			if tp == nil {
				tp = builtinProvider{}
			}
			payload["tools"] = tp.Tools()
			payload["tool_choice"] = "auto"
		}

		body, _ := json.Marshal(payload)
		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Post(r.apiURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("connection to llama-server failed: %w", err)
		}

		var toolCalls []ToolCall
		toolCallMap := map[int]*ToolCall{}
		var contentBuf strings.Builder
		finishReason := ""

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				effectiveEmit(choice.Delta.Content)
				contentBuf.WriteString(choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				existing, ok := toolCallMap[tc.Index]
				if !ok {
					toolCallMap[tc.Index] = &ToolCall{
						ID:   tc.ID,
						Type: tc.Type,
						Function: ToolCallFunc{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
				} else {
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Function.Name != "" {
						existing.Function.Name = tc.Function.Name
					}
					existing.Function.Arguments += tc.Function.Arguments
				}
			}
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}
		resp.Body.Close()
		if splitter != nil {
			splitter.flush()
		}

		for i := 0; i < len(toolCallMap); i++ {
			if tc, ok := toolCallMap[i]; ok {
				if tc.ID == "" {
					tc.ID = fmt.Sprintf("call_%d_%d", round, i)
				}
				if tc.Type == "" {
					tc.Type = "function"
				}
				toolCalls = append(toolCalls, *tc)
			}
		}

		if finishReason != "tool_calls" || len(toolCalls) == 0 {
			return nil
		}

		// Client-side tool execution: hand tool_calls back to the caller and stop
		if sopts.ToolCallEmit != nil {
			sopts.ToolCallEmit(toolCalls)
			return nil
		}

		// Server-side tool execution loop (builtin tools only)
		tcJSON := make([]map[string]interface{}, len(toolCalls))
		for i, tc := range toolCalls {
			tcJSON[i] = map[string]interface{}{
				"id": tc.ID, "type": "function",
				"function": map[string]string{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			}
		}
		messages = append(messages, map[string]interface{}{
			"role": "assistant", "content": contentBuf.String(), "tool_calls": tcJSON,
		})
		for _, tc := range toolCalls {
			if toolEmit != nil {
				toolEmit("tool_call", map[string]string{
					"id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
				})
			}
			tp := sopts.ToolProvider
			if tp == nil {
				tp = builtinProvider{}
			}
			result, err := tp.Execute(tc.Function.Name, tc.Function.Arguments)
			resultStr := result
			errStr := ""
			if err != nil {
				errStr = err.Error()
				resultStr = fmt.Sprintf(`{"error":%q}`, err.Error())
			}
			if toolEmit != nil {
				toolEmit("tool_result", ToolResult{
					CallID: tc.ID, Name: tc.Function.Name, Result: result, Error: errStr,
				})
			}
			messages = append(messages, map[string]string{
				"role": "tool", "content": TruncateForContext(resultStr, 2000), "tool_call_id": tc.ID,
			})
		}
	}
	return nil
}

// runWithTransformers handles non-GGUF LLM models (safetensors / transformers format)
func (r *LLMRunner) runWithTransformers(opts *RunOptions) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("python3 not found")
	}
	prompt := opts.Prompt
	if opts.InputFile != "" {
		data, _ := os.ReadFile(opts.InputFile)
		prompt = string(data)
	}
	if prompt == "" {
		prompt = "Hello!"
	}
	if !CheckPythonPackage(pythonBin, "transformers") {
		fmt.Println("📦  Installing transformers...")
		_ = InstallPythonPackage(pythonBin, "transformers", "accelerate", "sentencepiece", "torch")
	}
	modelDir := filepath.Dir(r.model.LocalPath)
	for {
		if _, err := os.Stat(filepath.Join(modelDir, "config.json")); err == nil {
			break
		}
		p := filepath.Dir(modelDir)
		if p == modelDir {
			break
		}
		modelDir = p
	}
	modelDirFwd := strings.ReplaceAll(modelDir, `\`, `/`)
	hwDevice := "cuda"
	if r.hw.Backend == BackendCPU || opts.ForceCPU {
		hwDevice = "cpu"
	}
	maxNew := 512
	if opts.ContextSize > 0 {
		maxNew = opts.ContextSize / 4
	}

	script := fmt.Sprintf(`import sys, os
os.environ["PYTHONIOENCODING"] = "utf-8"
model_path = r"""%s"""
prompt = """%s"""
device = "%s"
try:
    from transformers import AutoTokenizer, AutoModelForCausalLM, TextStreamer
    import torch
except ImportError as e:
    print(f"Dipendenza mancante: {e}")
    print("pip install transformers accelerate torch sentencepiece")
    sys.exit(1)
print(f"Loading model from {model_path}...")
dtype = torch.float16 if device != "cpu" else torch.float32
try:
    tokenizer = AutoTokenizer.from_pretrained(model_path, trust_remote_code=True)
    model = AutoModelForCausalLM.from_pretrained(
        model_path, torch_dtype=dtype,
        device_map="auto" if device != "cpu" else "cpu",
        trust_remote_code=True, low_cpu_mem_usage=True,
    )
except Exception as e:
    print(f"Load error: {e}")
    sys.exit(1)
print("Generating response...")
inputs = tokenizer(prompt, return_tensors="pt").to(device if device != "cpu" else "cpu")
streamer = TextStreamer(tokenizer, skip_prompt=True, skip_special_tokens=True)
with torch.no_grad():
    model.generate(**inputs, max_new_tokens=%d, do_sample=True,
        temperature=0.7, top_p=0.9, streamer=streamer)
`, modelDirFwd, escapePy(prompt), hwDevice, maxNew)

	tmp, err := os.CreateTemp("", "vortelio-llm-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	return RunWithOutput(cmd, os.Stdout, os.Stderr)
}

// streamWithTransformers runs a safetensors/bin model via Python and streams
// tokens to the emit callback by parsing VORTELIO_TOKEN: prefix lines.
func (r *LLMRunner) streamWithTransformers(opts *RunOptions, emit func(string)) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("python3 not found.\nInstall Python 3.10+ from https://python.org/downloads")
	}

	// Find the model directory (walk up to find config.json)
	modelDir := filepath.Dir(r.model.LocalPath)
	for {
		if _, err := os.Stat(filepath.Join(modelDir, "config.json")); err == nil {
			break
		}
		parent := filepath.Dir(modelDir)
		if parent == modelDir {
			break
		}
		modelDir = parent
	}
	modelDirFwd := strings.ReplaceAll(modelDir, `\`, `/`)

	hwDevice := "cuda"
	if r.hw.Backend == BackendCPU || opts.ForceCPU {
		hwDevice = "cpu"
	}

	ctxSize := 2048
	if r.currentCtxSize > 0 {
		ctxSize = r.currentCtxSize
	}
	maxNew := ctxSize / 4
	if maxNew < 256 {
		maxNew = 256
	}
	if maxNew > 1024 {
		maxNew = 1024
	}

	// Build conversation from history + current prompt
	conversation := ""
	for _, h := range r.history {
		conversation += "Utente: " + h.User + "\nAssistente: " + h.Asst + "\n"
	}
	conversation += "Utente: " + opts.Prompt + "\nAssistente:"

	script := fmt.Sprintf(`import sys, os, json
os.environ["PYTHONIOENCODING"] = "utf-8"
os.environ["TOKENIZERS_PARALLELISM"] = "false"
model_path = r"""%s"""
prompt = """%s"""
device = "%s"
max_new = %d

try:
    from transformers import AutoTokenizer, AutoModelForCausalLM, TextIteratorStreamer
    import torch
    from threading import Thread
except ImportError:
    import subprocess
    ok = False
    for args in [
        [sys.executable, "-m", "pip", "install", "-q", "transformers", "accelerate", "torch"],
        [sys.executable, "-m", "pip", "install", "-q", "--break-system-packages", "transformers", "accelerate", "torch"],
    ]:
        ok = subprocess.run(args, capture_output=True).returncode == 0
        if ok: break
    if not ok:
        print("VORTELIO_ERROR: could not install transformers. Run: pip install transformers torch")
        sys.exit(1)
    from transformers import AutoTokenizer, AutoModelForCausalLM, TextIteratorStreamer
    import torch
    from threading import Thread

sys.stdout.write("VORTELIO_PROGRESS:10:Loading tokenizer...\n"); sys.stdout.flush()
dtype = torch.float16 if device != "cpu" else torch.float32
try:
    tokenizer = AutoTokenizer.from_pretrained(model_path, trust_remote_code=True)
    sys.stdout.write("VORTELIO_PROGRESS:30:Loading model...\n"); sys.stdout.flush()
    model = AutoModelForCausalLM.from_pretrained(
        model_path, torch_dtype=dtype,
        device_map="auto" if device == "cuda" else "cpu",
        trust_remote_code=True, low_cpu_mem_usage=True,
    )
except Exception as e:
    print(f"VORTELIO_ERROR: {e}")
    sys.exit(1)

sys.stdout.write("VORTELIO_PROGRESS:60:Generating response...\n"); sys.stdout.flush()
inputs = tokenizer(prompt, return_tensors="pt").to(device if device == "cuda" else "cpu")
streamer = TextIteratorStreamer(tokenizer, skip_prompt=True, skip_special_tokens=True)
gen_kwargs = dict(
    **inputs,
    max_new_tokens=max_new,
    do_sample=True, temperature=0.7, top_p=0.9,
    streamer=streamer,
)
t = Thread(target=model.generate, kwargs=gen_kwargs)
t.start()
for token in streamer:
    if token:
        sys.stdout.write("VORTELIO_TOKEN:" + json.dumps(token) + "\n")
        sys.stdout.flush()
t.join()
sys.stdout.write("VORTELIO_DONE\n"); sys.stdout.flush()
`, modelDirFwd, escapePy(conversation), hwDevice, maxNew)

	tmp, err := os.CreateTemp("", "vortelio-stream-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()

	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VORTELIO_TOKEN:") {
			raw := strings.TrimPrefix(line, "VORTELIO_TOKEN:")
			var token string
			if err := json.Unmarshal([]byte(raw), &token); err != nil {
				token = raw // plain-text fallback for older script output
			}
			emit(token)
		} else if strings.HasPrefix(line, "VORTELIO_ERROR:") {
			cmd.Wait()
			return fmt.Errorf("%s", strings.TrimPrefix(line, "VORTELIO_ERROR:"))
		}
		// VORTELIO_PROGRESS and other lines are silently consumed
	}
	return cmd.Wait()
}
