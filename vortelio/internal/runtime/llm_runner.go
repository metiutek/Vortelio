package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/vortelio/vortelio/internal/gguf"
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
	// embedding: started with --embeddings (a separate server from chat).
	embedding bool
	// done is closed when the llama-server process exits.
	done chan struct{}
	// What the running server reported: layers on the GPU, context size and
	// whether its chat template has a native thinking switch.
	gpuLayers   int
	totalLayers int
	ctxSize     int
	nativeThink bool
	log         *tailBuf
	// abort, when closed, stops a load in progress (another model was
	// picked meanwhile).
	abort chan struct{}
	// active counts the requests streaming from this server: a newer model
	// waits for them instead of cutting a reply in half.
	active atomic.Int32
}

// Busy reports whether a request is streaming from this runner.
func (r *LLMRunner) Busy() bool { return r.active.Load() > 0 }

// ErrLoadAborted is returned by EnsureServer when the load was superseded.
var ErrLoadAborted = errors.New("loading was cancelled: another model was requested")

func (r *LLMRunner) aborted() bool {
	select {
	case <-r.abort:
		return true
	default:
		return false
	}
}

// Alive reports whether this runner's llama-server is still running.
func (r *LLMRunner) Alive() bool {
	if r.apiURL == "" || r.done == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// Offload returns how many layers run on the GPU and the model's total.
func (r *LLMRunner) Offload() (gpu, total int) { return r.gpuLayers, r.totalLayers }

// Model returns the model this runner serves.
func (r *LLMRunner) Model() *hub.Model { return r.model }

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

	caps := probeLlamaCaps(srv)

	// GPU placement. With --fit (llama.cpp ≥ b6xxx, on by default) llama.cpp
	// measures the FREE memory of each device and places as many layers as fit,
	// so Vortelio must not pass a guessed --n-gpu-layers: an explicit count turns
	// that adjustment off. A per-model override still wins.
	gpu := r.hw.Backend == BackendCUDA || r.hw.Backend == BackendMetal || r.hw.Backend == BackendROCm
	forcedGL := r.model.NumGPULayers > 0
	// Older versions remembered "this model only loads on CPU here" after one
	// failed GPU attempt — often just VRAM held by another app at that moment.
	// --fit sizes the load to the memory free right now, so that verdict is
	// only honoured without it.
	forcedCPU := r.model.NumGPULayers < 0 && !caps.Fit

	ctxSize := 8192
	if r.currentCtxSize > 0 {
		ctxSize = r.currentCtxSize
	}
	if v, ok := r.model.ModelParameters["num_ctx"]; ok {
		var parsed int
		if n, err := fmt.Sscanf(v, "%d", &parsed); n == 1 && err == nil && parsed > 0 {
			ctxSize = parsed
		}
	}

	// attempt starts llama-server (on the GPU, or CPU-only when cpuOnly) and
	// waits up to deadlineSecs for the model to become healthy. On any failure
	// it kills the process and returns an error so the caller can retry with a
	// safer configuration.
	attempt := func(cpuOnly bool, deadlineSecs int) error {
		if r.aborted() {
			return ErrLoadAborted
		}
		port, err := freePort()
		if err != nil {
			return fmt.Errorf("could not find a free port: %w", err)
		}
		r.apiURL = fmt.Sprintf("http://127.0.0.1:%d", port)

		r.gpuLayers = 0
		args := []string{
			"--model", r.model.LocalPath,
			"--port", fmt.Sprintf("%d", port),
			"--host", "127.0.0.1",
			"--ctx-size", fmt.Sprintf("%d", ctxSize),
			"-np", "1",
		}
		switch {
		case cpuOnly:
			args = append(args, "--n-gpu-layers", "0")
			if caps.Fit {
				args = append(args, "--fit", "off")
			}
		case forcedGL:
			args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", r.model.NumGPULayers))
			if caps.Fit {
				args = append(args, "--fit", "off")
			}
		case caps.Fit:
			// llama-fit-params does what --fit does inside the server, in the
			// same time, but tells us the result: the layer count is then known
			// (GPU/CPU split in the UI) without slow verbose server logging.
			if n, ok := fitGPULayers(srv, r.model, ctxSize, r.hw.VRAM); ok {
				args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", n), "--fit", "off")
				r.gpuLayers = n
				break
			}
			args = append(args, "--fit", "on")
			if caps.FitTarget {
				args = append(args, "--fit-target", fmt.Sprintf("%d", fitTargetMiB(r.hw.VRAM)))
			}
		default: // old llama.cpp without --fit: estimate from the model size
			args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", gpuLayersForModel(r.hw.VRAM, r.model.SizeBytes)))
		}
		if r.embedding {
			args = append(args, "--embeddings")
			// Chat models carry no pooling type; OpenAI-style embeddings need one.
			if caps.Pooling && needsPooling(r.model.LocalPath) {
				args = append(args, "--pooling", "mean")
			}
		} else if caps.Jinja {
			// Native chat template from the GGUF: correct prompts, tool calls
			// and the thinking switch for every architecture.
			args = append(args, "--jinja")
		}

		// Multimodal projector (vision)
		if r.model.MmProjPath != "" && !r.embedding {
			if _, statErr := os.Stat(r.model.MmProjPath); statErr == nil {
				args = append(args, "--mmproj", r.model.MmProjPath)
			}
		}
		// Flash attention (llama.cpp picks automatically unless set)
		if mp, ok := r.model.ModelParameters["flash_attn"]; ok {
			on := mp == "true" || mp == "1" || mp == "on"
			switch {
			case caps.FlashAttnValue && on:
				args = append(args, "--flash-attn", "on")
			case caps.FlashAttnValue:
				args = append(args, "--flash-attn", "off")
			case on:
				args = append(args, "--flash-attn")
			}
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
		sb := &tailBuf{max: 65536}
		cmd.Stdout = sb
		cmd.Stderr = sb
		r.log = sb

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("llama-server start failed: %w\n    Path: %s", err, srv)
		}
		r.proc = cmd

		// Watch for early process exit (e.g. VRAM OOM, bad model) so we can fail
		// over fast instead of polling /health until the deadline.
		exited := make(chan error, 1)
		done := make(chan struct{})
		r.done = done
		go func() { exited <- cmd.Wait(); close(done) }()

		// Wait until /health returns {"status":"ok"} — during loading it returns
		// 200 {"status":"loading model"}, so we must wait for "ok" specifically.
		deadline := time.Now().Add(time.Duration(deadlineSecs) * time.Second)
		client := &http.Client{Timeout: 2 * time.Second}
		for time.Now().Before(deadline) {
			select {
			case <-r.abort:
				cmd.Process.Kill()
				<-exited
				return ErrLoadAborted
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
				r.readServerInfo(sb.String())
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

	fmt.Println("⏳  Loading model…")
	cpuFirst := forcedCPU || !gpu
	deadline := 180 // partial GPU offload of a big model can take a while
	if cpuFirst {
		deadline = 600
	}
	err := attempt(cpuFirst, deadline)
	if err == nil {
		if r.model.NumGPULayers < 0 && !cpuFirst {
			r.model.NumGPULayers = 0 // the GPU works again: forget the old CPU-only verdict
			_ = hub.NewModelStore().Save(r.model)
		}
		r.printReady()
		return nil
	}

	// GPU load failed (driver OOM, a card shared with other apps…): run on the
	// CPU so the chat still works. Not when the failure is FATAL (bad file,
	// unknown architecture) — the CPU cannot fix those — or when the user pinned
	// a GPU-layer count.
	if le, ok := err.(loadError); ok && le.fatal {
		return fmt.Errorf("%s", le.reason)
	}
	if err == ErrLoadAborted {
		return err
	}
	if !cpuFirst && !forcedGL {
		fmt.Printf("⚠️  GPU load failed (%v).\n    Retrying on CPU — slower, but it will work…\n", err)
		if cpuErr := attempt(true, 600); cpuErr == nil {
			// Without --fit, remember that this model only loads on CPU here so
			// future loads skip the slow GPU attempt.
			if !caps.Fit {
				r.model.NumGPULayers = -1
				_ = hub.NewModelStore().Save(r.model)
			}
			r.printReady()
			return nil
		} else {
			err = cpuErr
		}
	}
	if le, ok := err.(loadError); ok {
		return fmt.Errorf("%s", le.reason)
	}
	return fmt.Errorf("this model could not be loaded. Two common causes: it is too large for this machine (%s) — try a smaller model; "+
		"or it uses a newer architecture that the installed llama.cpp can't read yet — update llama.cpp (Downloads → Engines, or `vortelio setup`). [%v]", r.hw.String(), err)
}

var (
	offloadRe = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)
	nCtxRe    = regexp.MustCompile(`n_ctx(?:_seq)?\s*=\s*(\d+)`)
)

// readServerInfo records what the freshly started server reports: GPU
// offload, context size and whether the chat template has a thinking switch.
func (r *LLMRunner) readServerInfo(log string) {
	r.totalLayers, r.ctxSize = 0, 0
	if g, err := gguf.Read(r.model.LocalPath, 0); err == nil {
		if n, ok := g.ArchInt("block_count"); ok {
			r.totalLayers = int(n) + 1 // + output layer, as llama.cpp counts
		}
		if r.gpuLayers > r.totalLayers {
			r.gpuLayers = r.totalLayers
		}
	}
	if m := offloadRe.FindAllStringSubmatch(log, -1); len(m) > 0 {
		last := m[len(m)-1]
		fmt.Sscanf(last[1], "%d", &r.gpuLayers)
		fmt.Sscanf(last[2], "%d", &r.totalLayers)
	}
	if m := nCtxRe.FindStringSubmatch(log); m != nil {
		fmt.Sscanf(m[1], "%d", &r.ctxSize)
	}
	r.nativeThink = false
	if r.embedding {
		return
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(r.apiURL + "/props")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var props struct {
		ChatTemplate string `json:"chat_template"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&props) == nil {
		t := props.ChatTemplate
		r.nativeThink = strings.Contains(t, "enable_thinking") || strings.Contains(t, "<think>") ||
			strings.Contains(t, "reasoning_content") || strings.Contains(t, "thinking")
	}
}

// needsPooling reports whether a GGUF declares no pooling (chat models), so
// an embedding server must be told how to pool token vectors.
func needsPooling(path string) bool {
	g, err := gguf.Read(path, 0)
	if err != nil {
		return true
	}
	v, ok := g.ArchInt("pooling_type")
	return !ok || v <= 0
}

var fitNGLRe = regexp.MustCompile(`(?m)-ngl (-?\d+)`)

// fitGPULayers runs llama-fit-params (shipped with llama.cpp) to find how many
// layers fit in the free VRAM, keeping fitTargetMiB free.
func fitGPULayers(srv string, model *hub.Model, ctxSize int, vram int64) (int, bool) {
	tool := filepath.Join(filepath.Dir(srv), "llama-fit-params")
	if runtime.GOOS == "windows" {
		tool += ".exe"
	}
	if _, err := os.Stat(tool); err != nil {
		return 0, false
	}
	// The tool has no --mmproj: keep the projector's room (weights + ~25%
	// for its compute buffer) free on top of the usual margin.
	target := fitTargetMiB(vram)
	if model.MmProjPath != "" {
		if st, err := os.Stat(model.MmProjPath); err == nil {
			target += st.Size() / (1 << 20) * 5 / 4
		}
	}
	args := []string{"--model", model.LocalPath, "--ctx-size", fmt.Sprintf("%d", ctxSize), "-np", "1",
		"--fit-target", fmt.Sprintf("%d", target)}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = filepath.Dir(tool)
	HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	m := fitNGLRe.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	var n int
	fmt.Sscanf(string(m[1]), "%d", &n)
	if n < 0 {
		n = 999 // "-ngl -1": every layer fits
	}
	return n, true
}

// fitTargetMiB is the free VRAM --fit leaves: ~8% of the card, 256–1024 MiB.
// llama.cpp's default (1 GiB) costs a third of the layers on a 4 GB card
// (measured on a GTX 1650 with qwen3.5:4b: 22/33 layers, 7.8 tok/s → 32/33,
// 28 tok/s).
func fitTargetMiB(vram int64) int64 {
	mib := vram / (1 << 20) * 8 / 100
	if mib < 256 {
		mib = 256
	}
	if mib > 1024 {
		mib = 1024
	}
	return mib
}

func (r *LLMRunner) printReady() {
	switch {
	case r.totalLayers > 0 && r.gpuLayers >= r.totalLayers:
		fmt.Printf("✅  Model ready (GPU, %d layers)\n", r.totalLayers)
	case r.totalLayers > 0 && r.gpuLayers > 0:
		fmt.Printf("✅  Model ready (%d/%d layers on GPU, rest on CPU)\n", r.gpuLayers, r.totalLayers)
	case r.totalLayers > 0:
		fmt.Println("✅  Model ready (CPU)")
	default:
		fmt.Println("✅  Model ready")
	}
}

func (r *LLMRunner) stopServer() {
	if r.proc != nil && r.proc.Process != nil {
		r.proc.Process.Kill()
		// Wait for the exit so its VRAM is really free before the next load.
		if r.done != nil {
			select {
			case <-r.done:
			case <-time.After(10 * time.Second):
			}
		}
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
		"temperature":    0.7,
		"repeat_penalty": 1.1,
		"stream":         true,
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Minute}
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
	thinking := false
	defer func() {
		if thinking {
			fmt.Print("\033[0m")
		}
	}()
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
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
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
		// Reasoning models: show the chain of thought dimmed, then the answer.
		if rc := chunk.Choices[0].Delta.ReasoningContent; rc != "" {
			if !thinking {
				thinking = true
				fmt.Print("\033[2m💭 ")
			}
			fmt.Print(rc)
		}
		token := chunk.Choices[0].Delta.Content
		if token != "" {
			if thinking {
				thinking = false
				fmt.Print("\033[0m\n\n")
			}
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
		client := &http.Client{Timeout: 30 * time.Minute} // covers the whole stream: long CPU/reasoning answers exceed minutes
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
						Content string `json:"content"`
						// Reasoning models: llama-server (--reasoning-format auto,
						// the default) streams the chain of thought here, not in
						// content. "reasoning" is the Ollama/OpenRouter spelling.
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
						ToolCalls        []struct {
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
			complete := true
			for _, e := range embs {
				complete = complete && len(e) > 0
			}
			if complete {
				return embs, nil
			}
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
		if len(res2.Embedding) == 0 {
			return nil, fmt.Errorf("llama-server returned no embedding (HTTP %d): %s", r2.StatusCode, strings.TrimSpace(string(b2data[:min(len(b2data), 300)])))
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
	Messages      []map[string]interface{} // full conversation; with Prompt set, the turns before it
	System        string                   // system prompt override (used with Prompt)
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
	// Ctx aborts the generation and the tool loop (nil = never).
	Ctx context.Context
	// ToolResultLimit caps each tool result fed back to the model, in bytes
	// (0 = 2000). Coding agents raise it so whole files fit.
	ToolResultLimit int
	// Stats, when set, receives token counts and timings (Ollama eval_* fields).
	Stats *GenStats
}

// GenStats are the token counts and timings llama-server reports.
type GenStats struct {
	PromptTokens   int
	PromptDuration time.Duration
	EvalTokens     int
	EvalDuration   time.Duration
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
	// -1 = until end of turn / context full (Ollama's default). A fixed cap cut
	// reasoning models off before they reached the answer.
	maxTok := -1
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
	client := &http.Client{Timeout: 30 * time.Minute} // covers the whole stream: long CPU/reasoning answers exceed minutes
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
	r.active.Add(1)
	defer r.active.Add(-1)

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
	} else if sopts.Messages != nil && sopts.Prompt == "" {
		// A complete conversation (Ollama/OpenAI clients, vortelio code).
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
		// Models with a native thinking switch get it through the chat template
		// (below); only plain models are asked to reason in <think> tags.
		if sopts.Think && !r.nativeThink {
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
		// Earlier turns sit between the system prompt and the new question.
		// Chat templates (Qwen…) accept a single system message, first: the
		// caller's leading system messages are merged into ours.
		history := sopts.Messages
		for len(history) > 0 && history[0]["role"] == "system" {
			if c, ok := history[0]["content"].(string); ok && strings.TrimSpace(c) != "" {
				sysPrompt += "\n\n" + c
			}
			history = history[1:]
		}
		messages = []interface{}{map[string]string{"role": "system", "content": sysPrompt}}
		for _, m := range history {
			messages = append(messages, m)
		}
		messages = append(messages, map[string]interface{}{"role": "user", "content": userContent})
	}

	temp := 0.7
	if sopts.Options.Temperature > 0 {
		temp = sopts.Options.Temperature
	}
	maxTok := 0 // 0 = no cap (see streamWithRawPrompt)
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
		// A tool (e.g. image generation) may have freed the GPU by stopping this
		// server between rounds: start it again before continuing.
		if !r.Alive() {
			if err := GlobalModelManager.revive(r); err != nil {
				return err
			}
		}
		// On the final round, force a text answer (no tools) so the model can't
		// keep calling tools forever and return an empty response.
		lastRound := round == maxToolRounds-1
		payload := map[string]interface{}{
			"model":          "local",
			"messages":       messages,
			"temperature":    temp,
			"repeat_penalty": repPen,
			"stream":         true,
		}
		if r.nativeThink {
			// Qwen3/3.5, GLM, DeepSeek, Gemma…: honour the Think toggle instead
			// of letting the model always reason (which, with a token cap,
			// can leave the answer empty).
			payload["chat_template_kwargs"] = map[string]interface{}{"enable_thinking": sopts.Think}
		}
		if maxTok > 0 {
			payload["max_tokens"] = maxTok
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
		client := &http.Client{Timeout: 30 * time.Minute} // covers the whole stream: long CPU/reasoning answers exceed minutes
		ctx := sopts.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.apiURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("connection to llama-server failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return fmt.Errorf("llama-server error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}

		var toolCalls []ToolCall
		toolCallMap := map[int]*ToolCall{}
		var contentBuf strings.Builder
		finishReason := ""

		sc := bufio.NewScanner(resp.Body)
		// One SSE line can carry a whole tool call (e.g. a file to write).
		sc.Buffer(make([]byte, 64*1024), 16<<20)
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
				Timings *struct {
					PromptN     int     `json:"prompt_n"`
					PromptMS    float64 `json:"prompt_ms"`
					PredictedN  int     `json:"predicted_n"`
					PredictedMS float64 `json:"predicted_ms"`
				} `json:"timings"`
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
						// Reasoning models: llama-server (--reasoning-format auto,
						// the default) streams the chain of thought here, not in
						// content. "reasoning" is the Ollama/OpenRouter spelling.
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
						ToolCalls        []struct {
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
			if chunk.Timings != nil && sopts.Stats != nil {
				t := chunk.Timings
				sopts.Stats.PromptTokens += t.PromptN
				sopts.Stats.PromptDuration += time.Duration(t.PromptMS * float64(time.Millisecond))
				sopts.Stats.EvalTokens += t.PredictedN
				sopts.Stats.EvalDuration += time.Duration(t.PredictedMS * float64(time.Millisecond))
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if rc := choice.Delta.ReasoningContent + choice.Delta.Reasoning; rc != "" && sopts.ThinkEmit != nil {
				sopts.ThinkEmit(rc)
			}
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
		if ctx.Err() != nil {
			return ctx.Err()
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
			if ctx.Err() != nil {
				return ctx.Err()
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
				"role": "tool", "content": TruncateForContext(resultStr, sopts.ToolResultLimit), "tool_call_id": tc.ID,
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
