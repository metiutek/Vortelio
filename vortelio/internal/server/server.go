package server

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	fb "github.com/vortelio/vortelio/internal/firebase"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/internal/version"
)

//go:embed ui.html
var uiFS embed.FS

//go:embed vortelio.ico
var faviconICO []byte

// handleFavicon serves the Vortelio icon so the browser/PWA (and the Edge --app
// window in the taskbar) shows the Vortelio logo instead of the browser's.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(faviconICO)
}

// handleManifest serves a minimal PWA manifest so the Edge/Chrome --app window
// is treated as an installed app and uses the Vortelio icon.
func handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Write([]byte(`{"name":"Vortelio","short_name":"Vortelio","start_url":"/","display":"standalone","background_color":"#0b0b12","theme_color":"#6366f1","icons":[{"src":"/favicon.ico","sizes":"256x256 128x128 64x64 48x48 32x32 16x16","type":"image/x-icon"}]}`))
}

// InitLogger configures the global slog logger. Call once at startup.
// level: "debug", "info", "warn", "error"
func InitLogger(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

// ── Shared state ─────────────────────────────────────────────────────────────

var (
	activePullsMu sync.Mutex
	activePulls   = map[string]context.CancelFunc{}
	cachedHW      *runtime.Hardware
	cachedHWOnce  sync.Once
)

func registerPull(model string, cancel context.CancelFunc) {
	activePullsMu.Lock()
	defer activePullsMu.Unlock()
	activePulls[model] = cancel
}
func unregisterPull(model string) {
	activePullsMu.Lock()
	defer activePullsMu.Unlock()
	delete(activePulls, model)
}
func cancelPull(model string) bool {
	activePullsMu.Lock()
	defer activePullsMu.Unlock()
	if fn, ok := activePulls[model]; ok {
		fn()
		delete(activePulls, model)
		return true
	}
	return false
}

func getHardware() *runtime.Hardware {
	cachedHWOnce.Do(func() { cachedHW = runtime.DetectHardware() })
	return cachedHW
}

// ── Types ─────────────────────────────────────────────────────────────────────

type ModelWithSize struct {
	*hub.Model
	SizeHuman string `json:"size_human"`
}

type GenerateRequest struct {
	Model        string          `json:"model"`
	Prompt       string          `json:"prompt"`
	System       string          `json:"system"`   // system prompt override
	Template     string          `json:"template"` // prompt template override
	Context      []int           `json:"context"`  // conversation context tokens
	Images       []string        `json:"images"`   // base64-encoded images
	Raw          bool            `json:"raw"`      // skip template wrapping
	Messages     []ChatMessage   `json:"messages"`
	InputFile    string          `json:"input_file"`
	OutputFile   string          `json:"output_file"`
	Steps        int             `json:"steps"`
	Stream       *bool           `json:"stream"` // nil = true (streaming default)
	ForceCPU     bool            `json:"force_cpu"`
	ContextSize  int             `json:"context_size"`
	Think        bool            `json:"think"`
	ToolsEnabled bool            `json:"tools_enabled"`
	Incognito    bool            `json:"incognito"`  // when true, do not persist chat history
	Agentic      *AgenticConfig  `json:"agentic"`    // when set, builds a composite tool provider
	Format       json.RawMessage `json:"format"`     // "json" or JSON schema
	Suffix       string          `json:"suffix"`     // FIM suffix
	KeepAlive    json.RawMessage `json:"keep_alive"` // duration string or seconds
	Options      json.RawMessage `json:"options"`    // Ollama-style generation params
}

// AgenticConfig selects which tool groups a chat request can use and how the
// coding agent behaves.
type AgenticConfig struct {
	WebSearch  bool     `json:"web_search"`  // enable web_search tool
	Builtins   bool     `json:"builtins"`    // enable calculator/time/read/write/list builtins
	MCP        bool     `json:"mcp"`         // expose connected MCP server tools
	Coding     bool     `json:"coding"`      // expose coding tools (file + shell)
	Media      bool     `json:"media"`       // expose model-as-tool media generation (image/video/audio/3d)
	Auto       bool     `json:"auto"`        // smart mode: never hard-error on unsupported tools; nudge the model to use tools when helpful
	Autonomous bool     `json:"autonomous"`  // goal-driven autonomous loop: many tool rounds until the objective is complete
	Skills     []string `json:"skills"`      // enabled skill IDs
	Mode       string   `json:"mode"`        // coding mode: "plan" | "ask" | "edits" | "auto"
	WorkingDir string   `json:"working_dir"` // root dir for coding tools
	SessionID  string   `json:"session_id"`  // correlates approval prompts
	// ApproveFunc, when set (CLI), is called synchronously to approve risky tools
	// instead of the HTTP approval flow. Not serialized.
	ApproveFunc func(tool, summary, args string) bool `json:"-"`
	// AskFunc, when set (CLI), answers ask_user synchronously in the terminal.
	AskFunc func(question string, options []string) string `json:"-"`
	// Policy applies allow/deny rules before the mode (CLI permissions).
	Policy PolicyFunc `json:"-"`
	// OnFileChange is called before a tool modifies a file (CLI undo checkpoints).
	OnFileChange func(path string, before []byte, existed bool) `json:"-"`
	// State keeps read-before-edit tracking and todos across turns (CLI session).
	State *CodingState `json:"-"`
	// Ctx cancels running tools (e.g. a shell command) when the turn is aborted.
	Ctx context.Context `json:"-"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── Router ────────────────────────────────────────────────────────────────────

func NewMux() *http.ServeMux {
	go getHardware() // warm up hardware detection
	go func() {
		if err := fb.Init(); err != nil {
			slog.Info("firebase disabled", "reason", err.Error())
		} else {
			slog.Info("firebase initialized", "project", "vortelio-3e7a8")
		}
	}()
	mux := http.NewServeMux()

	ca := func(h http.HandlerFunc) http.HandlerFunc { return withObservability(withCORS(withAuth(h))) }

	mux.HandleFunc("/", handleUI)
	mux.HandleFunc("/favicon.ico", handleFavicon)
	mux.HandleFunc("/manifest.webmanifest", handleManifest)
	mux.HandleFunc("/api/status", ca(handleStatus))
	mux.HandleFunc("/api/update/check", ca(handleUpdateCheck))
	mux.HandleFunc("/api/update/start", ca(handleUpdateStart))
	mux.HandleFunc("/api/engine", ca(handleEngineStatus))
	mux.HandleFunc("/api/engine/update", ca(handleEngineUpdate))
	mux.HandleFunc("/api/upload", ca(handleUpload))

	// Hub — models + download (pull rate limited)
	mux.HandleFunc("/api/models", ca(handleModels))
	mux.HandleFunc("/api/models/", ca(handleModelByName))
	mux.HandleFunc("/api/models/remove", ca(handleModelRemove))
	mux.HandleFunc("/api/models/rename", ca(handleModelRename))
	mux.HandleFunc("/api/models/info", ca(handleModelInfo))
	mux.HandleFunc("/api/models/mmproj", ca(handleModelMmProj))
	mux.HandleFunc("/api/pull", ca(withRateLimit(pullLimiter, handlePull)))
	mux.HandleFunc("/api/pull/cancel", ca(handlePullCancel))

	// Generate (rate limited)
	mux.HandleFunc("/api/generate", ca(withRateLimit(generateLimiter, handleGenerate)))

	// Agents
	mux.HandleFunc("/api/agents/proxy", ca(handleAgentProxy))
	mux.HandleFunc("/api/agents/check", ca(handleAgentCheck))
	mux.HandleFunc("/api/agents/catalog", ca(handleAgentCatalog))
	mux.HandleFunc("/api/agents/install", ca(handleAgentInstall))
	mux.HandleFunc("/api/agents/start", ca(handleAgentStart))
	mux.HandleFunc("/api/agents/stop", ca(handleAgentStop))
	mux.HandleFunc("/api/agents/uninstall", ca(handleAgentUninstall))
	mux.HandleFunc("/api/agents/health", ca(handleAgentHealth))
	mux.HandleFunc("/api/ollama/models", ca(handleOllamaModels))

	// CrewAI orchestration (legacy JSON CRUD)
	mux.HandleFunc("/api/crewai/crews", ca(handleCrewList))
	mux.HandleFunc("/api/crewai/crews/", ca(handleCrewDispatch))
	// CrewAI Studio proxy → Python server port 8500
	mux.HandleFunc("/api/crewai/studio/", ca(handleCrewStudioProxy))

	// Graceful shutdown (used by vortelio stop)
	mux.HandleFunc("/api/shutdown", handleShutdown)

	// History
	mux.HandleFunc("/api/history", ca(handleHistory))

	// ── Ollama-compatible API ─────────────────────────────────────────────────
	mux.HandleFunc("/api/version", ca(handleOllamaVersion))
	mux.HandleFunc("/api/ps", ca(handleOllamaPs))
	mux.HandleFunc("/api/tags", ca(handleOllamaTags))
	mux.HandleFunc("/api/show", ca(handleOllamaShow))
	mux.HandleFunc("/api/delete", ca(handleOllamaDelete))
	mux.HandleFunc("/api/chat", ca(withRateLimit(generateLimiter, handleOllamaChat)))
	mux.HandleFunc("/api/embed", ca(handleOllamaEmbed))
	mux.HandleFunc("/api/embeddings", ca(handleOllamaEmbeddings))
	mux.HandleFunc("/api/copy", ca(handleOllamaCopy))
	mux.HandleFunc("/api/create", ca(handleOllamaCreate))
	mux.HandleFunc("/api/push", ca(handleOllamaPush))
	mux.HandleFunc("/api/quantize", ca(handleOllamaQuantize))
	mux.HandleFunc("/api/blobs/", ca(handleOllamaBlobs))

	// ── OpenAI-compatible API ─────────────────────────────────────────────────
	mux.HandleFunc("/v1/models", ca(handleOpenAIModels))
	mux.HandleFunc("/v1/models/", ca(handleOpenAIModelByID))
	mux.HandleFunc("/v1/chat/completions", ca(withRateLimit(generateLimiter, handleOpenAIChatCompletions)))
	mux.HandleFunc("/v1/completions", ca(withRateLimit(generateLimiter, handleOpenAICompletions)))
	mux.HandleFunc("/v1/embeddings", ca(handleOpenAIEmbeddings))
	mux.HandleFunc("/v1/audio/transcriptions", ca(withRateLimit(generateLimiter, handleOpenAIAudioTranscriptions)))
	mux.HandleFunc("/v1/audio/translations", ca(withRateLimit(generateLimiter, handleOpenAIAudioTranslations)))
	mux.HandleFunc("/v1/audio/speech", ca(withRateLimit(generateLimiter, handleOpenAIAudioSpeech)))
	mux.HandleFunc("/v1/images/generations", ca(withRateLimit(generateLimiter, handleOpenAIImageGenerations)))

	// ── Advanced features (Vortelio-only) ────────────────────────────────────
	mux.HandleFunc("/api/route", ca(handleRoute))
	mux.HandleFunc("/api/compare", ca(withRateLimit(generateLimiter, handleCompare)))
	mux.HandleFunc("/api/structured", ca(withRateLimit(generateLimiter, handleStructured)))
	mux.HandleFunc("/api/summarize", ca(withRateLimit(generateLimiter, handleSummarize)))
	mux.HandleFunc("/api/think", ca(withRateLimit(generateLimiter, handleThink)))
	mux.HandleFunc("/api/gguf/inspect", ca(handleGGUFInspect))
	mux.HandleFunc("/api/hooks", ca(handleHooks))
	mux.HandleFunc("/api/hooks/", ca(handleHooks))
	mux.HandleFunc("/api/audit", ca(handleAudit))
	mux.HandleFunc("/api/rag/ingest", ca(handleRAGIngest))
	mux.HandleFunc("/api/rag/query", ca(handleRAGQuery))
	mux.HandleFunc("/api/import/ollama", ca(handleImportOllama))
	mux.HandleFunc("/api/config", ca(handleConfig))

	// ── Firebase Auth & user data ─────────────────────────────────────────────
	// /api/auth/verify is public (no Vortelio API key needed — client sends Firebase ID token)
	mux.HandleFunc("/api/auth/verify", withObservability(withCORS(withRateLimit(authLimiter, handleAuthVerify))))
	mux.HandleFunc("/api/auth/status", withObservability(withCORS(handleAuthStatus)))
	mux.HandleFunc("/api/user/profile", ca(handleUserProfile))
	mux.HandleFunc("/api/user/settings", ca(handleUserSettings))
	mux.HandleFunc("/api/user/apikeys", ca(handleAPIKeys))
	mux.HandleFunc("/api/user/apikeys/", ca(handleAPIKeys))
	mux.HandleFunc("/api/chats", ca(handleChats))
	mux.HandleFunc("/api/chats/", ca(handleChats))

	// ── Cloud proxy (OpenRouter) ──────────────────────────────────────────────
	mux.HandleFunc("/api/proxy/models", withObservability(withCORS(handleProxyModels)))
	mux.HandleFunc("/api/proxy/chat", withObservability(withCORS(withRateLimit(generateLimiter, handleProxyChat))))
	mux.HandleFunc("/api/proxy/usage", ca(handleProxyUsage))

	// ── BYOK cloud models (bring your own provider API key) ────────────────────
	mux.HandleFunc("/api/cloud/providers", ca(handleCloudProviders))
	mux.HandleFunc("/api/cloud/key", ca(handleCloudKey))
	mux.HandleFunc("/api/cloud/chat", ca(handleCloudChat))

	// ── MCP (Model Context Protocol) servers ──────────────────────────────────
	mux.HandleFunc("/api/mcp/servers", ca(handleMCPServers))
	mux.HandleFunc("/api/mcp/enable", ca(handleMCPEnable))
	mux.HandleFunc("/api/mcp/remove", ca(handleMCPRemove))

	// ── Agentic: skills + tool approvals ───────────────────────────────────────
	mux.HandleFunc("/api/skills", ca(handleSkills))
	mux.HandleFunc("/api/skills/delete", ca(handleSkillDelete))
	mux.HandleFunc("/api/agentic/approve", ca(handleAgenticApprove))
	mux.HandleFunc("/api/agentic/answer", ca(handleAgenticAnswer))
	mux.HandleFunc("/api/run-code", ca(handleRunCode))
	mux.HandleFunc("/api/media/providers", ca(handleMediaProviders))
	mux.HandleFunc("/api/media/key", ca(handleMediaKey))
	mux.HandleFunc("/api/hf/search", ca(handleHFSearch))
	mux.HandleFunc("/api/system/resources", ca(handleSystemResources))

	// ── Developer file explorer (read-only) ───────────────────────────────────
	mux.HandleFunc("/api/fs/list", ca(handleFsList))
	mux.HandleFunc("/api/fs/read", ca(handleFsRead))

	// ── Stripe payments ───────────────────────────────────────────────────────
	mux.HandleFunc("/api/stripe/checkout", ca(handleStripeCheckout))
	mux.HandleFunc("/api/stripe/webhook", withObservability(withCORS(handleStripeWebhook)))

	// Public observability/spec (no auth — required for monitoring tools)
	mux.HandleFunc("/metrics", withObservability(withCORS(handleMetrics)))
	mux.HandleFunc("/openapi.json", withObservability(withCORS(handleOpenAPI)))

	return mux
}

// ── Core handlers ─────────────────────────────────────────────────────────────

func handleUI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api") {
		http.NotFound(w, r)
		return
	}
	data, err := uiFS.ReadFile("ui.html")
	if err != nil {
		respond(w, 200, map[string]string{"name": "vortelio", "version": version.Version})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

var shutdownCh = make(chan struct{}, 1)

// ShutdownCh returns the channel closed by POST /api/shutdown.
func ShutdownCh() <-chan struct{} { return shutdownCh }

func handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	respond(w, 200, map[string]string{"status": "shutting down"})
	select {
	case shutdownCh <- struct{}{}:
	default:
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	store := hub.NewModelStore()
	models, _ := store.List()
	respond(w, 200, map[string]interface{}{
		"name": "vortelio", "version": version.Version,
		"model_count": len(models), "hardware": getHardware().String(),
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	r.ParseMultipartForm(100 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, 400, "no file: "+err.Error())
		return
	}
	defer file.Close()
	// Strip any path components from the client-supplied filename, and the "*"
	// that would change the CreateTemp pattern.
	safeName := filepath.Base(strings.NewReplacer("/", "_", "\\", "_", "*", "_").Replace(header.Filename))
	tmp, err := os.CreateTemp("", "vortelio-upload-*-"+safeName)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, file); err != nil {
		jsonError(w, 500, "upload failed: "+err.Error())
		return
	}
	respond(w, 200, map[string]string{"path": tmp.Name()})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func respond(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	if code >= 500 {
		slog.Error("server error", "code", code, "msg", msg)
	}
	respond(w, code, map[string]string{"error": msg})
}

func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		origin := r.Header.Get("Origin")
		if origin != "" {
			cfg := config.Get()
			allowed := false
			for _, o := range cfg.AllowOrigins {
				if o == "*" || o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				// Allow local origins only — match the parsed hostname exactly,
				// so "http://evil-localhost.example.com" cannot slip through.
				if u, err := url.Parse(origin); err == nil {
					switch u.Hostname() {
					case "localhost", "127.0.0.1", "::1":
						allowed = true
					}
				}
			}
			if allowed {
				if len(cfg.AllowOrigins) == 1 && cfg.AllowOrigins[0] == "*" {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Firebase-Token")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		h(w, r)
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).Round(time.Millisecond))
	}
}

func withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := config.Get().APIKey
		if key == "" {
			h(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "Bearer "+key || r.URL.Query().Get("api_key") == key {
			h(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="vortelio"`)
		jsonError(w, 401, "unauthorized")
	}
}
