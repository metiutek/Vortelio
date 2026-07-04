package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/config"
)

//go:embed runners/crewai_server.py
var crewaiServerScript []byte

var validAgentID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ── Catalog ───────────────────────────────────────────────────────────────────

type InstallMethod string

const (
	MethodNPM    InstallMethod = "npm"    // npm install -g <pkg>
	MethodPip    InstallMethod = "pip"    // pip install <pkg>
	MethodBinary InstallMethod = "binary" // direct exe download
	MethodUV     InstallMethod = "uv"     // uv run --with <deps> (no separate install step)
)

type CatalogEntry struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Version       string        `json:"version"`
	DefaultPort   int           `json:"default_port"`
	DefaultURL    string        `json:"default_url"`
	InstallMethod InstallMethod `json:"install_method"`
	NPMPackage    string        `json:"npm_package,omitempty"`
	BinCommand    string        `json:"bin_command"`
	StartArgs     []string      `json:"start_args"`
	// EnvVars are extra environment variables injected at start time.
	// Use {{VORTELIO_URL}} as placeholder — replaced with the actual Vortelio base URL.
	EnvVars        []string `json:"env_vars,omitempty"`
	HealthPath     string   `json:"health_path"`
	Tags           []string `json:"tags"`
	RequiresAPIKey bool     `json:"requires_api_key"`
	APIKeyHint     string   `json:"api_key_hint,omitempty"`
	// Script-based agents: extract embedded script, run via python interpreter.
	PipDeps    []string `json:"pip_deps,omitempty"`    // pip packages to install (MethodPip)
	UVDeps     []string `json:"uv_deps,omitempty"`     // packages for uv run --with (MethodUV)
	ScriptName string   `json:"script_name,omitempty"` // filename for extracted runner script
}

// vortURL returns the base URL of the local Vortelio server.
func vortURL() string {
	return fmt.Sprintf("http://localhost:%d", config.Get().Port)
}

var Catalog = []CatalogEntry{
	{
		ID:            "openclaw",
		Name:          "OpenClaw",
		Description:   "Multi-channel AI gateway (WhatsApp, Telegram, Discord). Uses Vortelio's local models.",
		Version:       "latest",
		DefaultPort:   18789,
		DefaultURL:    "http://localhost:18789",
		InstallMethod: MethodNPM,
		NPMPackage:    "openclaw",
		BinCommand:    "openclaw",
		StartArgs:     []string{"gateway", "--port", "18789"},
		// Point OpenClaw at Vortelio's Ollama-compatible API
		EnvVars: []string{
			"OLLAMA_HOST={{VORTELIO_URL}}",
			"OLLAMA_BASE_URL={{VORTELIO_URL}}",
			"OPENAI_BASE_URL={{VORTELIO_URL}}/v1",
			"OPENAI_API_BASE={{VORTELIO_URL}}/v1",
			"OPENAI_API_KEY=vortelio",
		},
		HealthPath:     "/",
		Tags:           []string{"chat", "gateway", "whatsapp", "telegram", "discord"},
		RequiresAPIKey: false,
	},
	{
		ID:            "opencode",
		Name:          "Open Code",
		Description:   "AI agent for developers: coding, refactoring, debugging. Web UI backed by Vortelio's local models.",
		Version:       "latest",
		DefaultPort:   4096,
		DefaultURL:    "http://localhost:4096",
		InstallMethod: MethodNPM,
		NPMPackage:    "opencode-ai",
		BinCommand:    "opencode",
		// `opencode web` serves the same agent as a browser UI (no terminal
		// needed). Vortelio wires it to its own OpenAI-compatible API via a
		// generated OPENCODE_CONFIG file (see writeOpenCodeConfig in Start).
		StartArgs: []string{"web", "--port", "4096", "--hostname", "127.0.0.1"},
		EnvVars: []string{
			"OPENAI_BASE_URL={{VORTELIO_URL}}/v1",
			"OPENAI_API_BASE={{VORTELIO_URL}}/v1",
			"OPENAI_API_KEY=vortelio",
		},
		HealthPath:     "/",
		Tags:           []string{"coding", "git", "debug", "refactor"},
		RequiresAPIKey: false,
	},
	{
		ID:            "open-webui",
		Name:          "Open WebUI",
		Description:   "Full web interface for chatting with local models. Ollama-compatible. Requires Python 3.11+.",
		Version:       "latest",
		DefaultPort:   3000,
		DefaultURL:    "http://localhost:3000",
		InstallMethod: MethodPip,
		NPMPackage:    "open-webui",
		BinCommand:    "open-webui",
		StartArgs:     []string{"serve", "--host", "0.0.0.0", "--port", "3000"},
		EnvVars: []string{
			"OLLAMA_BASE_URL={{VORTELIO_URL}}",
			"OPENAI_API_BASE_URL={{VORTELIO_URL}}/v1",
			"OPENAI_API_KEY=vortelio",
		},
		HealthPath:     "/health",
		Tags:           []string{"chat", "web", "ui", "ollama"},
		RequiresAPIKey: false,
	},
	{
		ID:            "flowise",
		Name:          "Flowise",
		Description:   "Visual AI flow builder: create agents, RAG and workflows with drag-and-drop. Port 3002.",
		Version:       "latest",
		DefaultPort:   3002,
		DefaultURL:    "http://localhost:3002",
		InstallMethod: MethodNPM,
		NPMPackage:    "flowise",
		BinCommand:    "flowise",
		StartArgs:     []string{"start"},
		EnvVars: []string{
			"PORT=3002",
			"FLOWISE_PORT=3002",
			"OPENAI_API_KEY=vortelio",
			"OPENAI_API_BASE={{VORTELIO_URL}}/v1",
		},
		HealthPath:     "/api/v1/ping",
		Tags:           []string{"flow", "agenti", "rag", "visual"},
		RequiresAPIKey: false,
	},
	{
		ID:            "crewai",
		Name:          "CrewAI Studio",
		Description:   "Multi-agent orchestration: build teams of collaborative AI agents for complex tasks. Requires uv (astral.sh/uv).",
		Version:       "latest",
		DefaultPort:   8500,
		DefaultURL:    "http://localhost:8500",
		InstallMethod: MethodUV,
		BinCommand:    "uv",
		ScriptName:    "crewai_server.py",
		UVDeps:        []string{"crewai", "crewai-tools", "fastapi", "uvicorn", "sse-starlette", "tomli-w"},
		StartArgs:     []string{},
		EnvVars: []string{
			"VORTELIO_URL={{VORTELIO_URL}}",
			"OPENAI_API_BASE={{VORTELIO_URL}}/v1",
			"OPENAI_API_KEY=vortelio",
		},
		HealthPath:     "/v1/models",
		Tags:           []string{"orchestration", "multi-agent", "crew", "automation", "uv"},
		RequiresAPIKey: false,
	},
}

// ── State ─────────────────────────────────────────────────────────────────────

type AgentState struct {
	ID            string        `json:"id"`
	Installed     bool          `json:"installed"`
	Running       bool          `json:"running"`
	PID           int           `json:"pid,omitempty"`
	Port          int           `json:"port"`
	NodeFound     bool          `json:"node_found"`
	PipFound      bool          `json:"pip_found"`
	UVFound       bool          `json:"uv_found"`
	InstallMethod InstallMethod `json:"install_method"`
	Error         string        `json:"error,omitempty"`
}

var (
	mu      sync.Mutex
	procMap = map[string]*exec.Cmd{}
)

// nodeAvailable checks if node and npm are in PATH.
func nodeAvailable() bool {
	_, err := exec.LookPath("node")
	return err == nil
}

func npmAvailable() bool {
	_, err := exec.LookPath("npm")
	return err == nil
}

func pipAvailable() bool {
	for _, bin := range []string{"pip3", "pip"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

func pythonBin() string {
	if cfg := config.Get(); cfg.PythonBin != "" {
		return cfg.PythonBin
	}
	candidates := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		candidates = []string{"python", "python3"}
	}
	for _, bin := range candidates {
		if _, err := exec.LookPath(bin); err == nil {
			return bin
		}
	}
	return "python"
}

func pipBin() string {
	candidates := []string{"pip3", "pip"}
	if runtime.GOOS == "windows" {
		candidates = []string{"pip", "pip3"}
	}
	for _, bin := range candidates {
		if _, err := exec.LookPath(bin); err == nil {
			return bin
		}
	}
	return "pip"
}

func uvAvailable() bool {
	_, err := exec.LookPath("uv")
	return err == nil
}

func uvBin() string {
	if _, err := exec.LookPath("uv"); err == nil {
		return "uv"
	}
	// common install location on Windows
	home, _ := os.UserHomeDir()
	candidate := filepath.Join(home, ".local", "bin", "uv.exe")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "uv"
}

// uvVenvPython returns the path to the python binary inside the uv venv for an agent.
func uvVenvPython(id string) string {
	venvDir := filepath.Join(config.HomeDir(), "venvs", id)
	if runtime.GOOS == "windows" {
		return filepath.Join(venvDir, "Scripts", "python.exe")
	}
	return filepath.Join(venvDir, "bin", "python")
}

// isBinInstalled checks if the agent binary is available.
func isBinInstalled(entry CatalogEntry) bool {
	if entry.InstallMethod == MethodUV {
		// Installed = uv venv exists and has a python binary
		py := uvVenvPython(entry.ID)
		_, err := os.Stat(py)
		return err == nil
	}
	if entry.InstallMethod == MethodPip {
		// Script-based pip agents: check python + primary pip dep importable
		if entry.ScriptName != "" {
			py := pythonBin()
			if _, err := exec.LookPath(py); err != nil {
				return false
			}
			pkg := entry.NPMPackage
			if pkg == "" && len(entry.PipDeps) > 0 {
				pkg = entry.PipDeps[0]
			}
			if pkg == "" {
				return false
			}
			importName := strings.ReplaceAll(pkg, "-", "_")
			cmd := exec.Command(py, "-c", "import "+importName)
			return cmd.Run() == nil
		}
		_, err := exec.LookPath(entry.BinCommand)
		if err == nil {
			return true
		}
		if runtime.GOOS == "windows" {
			_, err = exec.LookPath(entry.BinCommand + ".exe")
			return err == nil
		}
		return false
	}
	if entry.InstallMethod != MethodNPM {
		return false
	}
	// On Windows npm commands need .cmd suffix
	npmCmd := "npm"
	if runtime.GOOS == "windows" {
		npmCmd = "npm.cmd"
	}
	// Primary: npm list -g --depth=0 (most reliable)
	cmd := exec.Command(npmCmd, "list", "-g", "--depth=0", "--parseable")
	out, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			base := filepath.Base(strings.TrimSpace(line))
			if strings.EqualFold(base, entry.NPMPackage) ||
				strings.HasSuffix(strings.ToLower(line), "/"+strings.ToLower(entry.NPMPackage)) ||
				strings.HasSuffix(strings.ToLower(line), "\\"+strings.ToLower(entry.NPMPackage)) {
				return true
			}
		}
	}
	// Secondary: check the binary exists in npm global bin (LookPath is sufficient).
	binToCheck := entry.BinCommand
	if runtime.GOOS == "windows" {
		binToCheck = entry.BinCommand + ".cmd"
	}
	_, pathErr := exec.LookPath(binToCheck)
	return pathErr == nil
}

// GetState returns the current state of an agent.
func GetState(id string) AgentState {
	entry, ok := findEntry(id)
	if !ok {
		return AgentState{ID: id, Error: "unknown agent"}
	}
	installed := isBinInstalled(entry)

	mu.Lock()
	cmd, running := procMap[id]
	mu.Unlock()

	pid := 0
	if running && cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	return AgentState{
		ID:            id,
		Installed:     installed,
		Running:       running,
		PID:           pid,
		Port:          entry.DefaultPort,
		NodeFound:     nodeAvailable(),
		PipFound:      pipAvailable(),
		UVFound:       uvAvailable(),
		InstallMethod: entry.InstallMethod,
	}
}

// GetAllStates returns states for all catalog entries.
func GetAllStates() []AgentState {
	states := make([]AgentState, len(Catalog))
	for i, e := range Catalog {
		states[i] = GetState(e.ID)
	}
	return states
}

// ── Install ───────────────────────────────────────────────────────────────────

// Install installs the agent. For npm packages, streams npm output as progress.
func Install(ctx context.Context, id string, progress func(line string)) error {
	if !validAgentID.MatchString(id) {
		return fmt.Errorf("invalid agent id: %q", id)
	}
	entry, ok := findEntry(id)
	if !ok {
		return fmt.Errorf("unknown agent: %s", id)
	}

	switch entry.InstallMethod {
	case MethodNPM:
		return installNPM(ctx, entry, progress)
	case MethodPip:
		if len(entry.PipDeps) > 0 {
			for _, dep := range entry.PipDeps {
				if progress != nil {
					progress("Installing " + dep + "…")
				}
				if err := installPipPackage(ctx, dep, progress); err != nil {
					return err
				}
			}
			return nil
		}
		return installPip(ctx, entry, progress)
	case MethodUV:
		return installUV(ctx, entry, progress)
	case MethodBinary:
		return fmt.Errorf("binary install not supported for this agent")
	default:
		return fmt.Errorf("unknown install method")
	}
}

func installNPM(ctx context.Context, entry CatalogEntry, progress func(line string)) error {
	if !nodeAvailable() {
		return fmt.Errorf(
			"node not found in PATH.\n" +
				"Install Node.js from https://nodejs.org (LTS version recommended),\n" +
				"then restart Vortelio and try again",
		)
	}
	if !npmAvailable() {
		return fmt.Errorf(
			"npm not found in PATH.\n" +
				"Make sure npm is installed together with Node.js:\n" +
				"https://nodejs.org",
		)
	}

	npmCmd := "npm"
	if runtime.GOOS == "windows" {
		npmCmd = "npm.cmd"
	}

	args := []string{"install", "-g", entry.NPMPackage, "--prefer-online"}
	cmd := exec.CommandContext(ctx, npmCmd, args...)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start npm: %w", err)
	}

	// Stream both stdout and stderr as progress lines
	done := make(chan struct{}, 2)
	streamLines := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		var partial string
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := partial + string(buf[:n])
				lines := strings.Split(chunk, "\n")
				partial = lines[len(lines)-1]
				for _, l := range lines[:len(lines)-1] {
					l = strings.TrimSpace(l)
					if l != "" && progress != nil {
						progress(l)
					}
				}
			}
			if err != nil {
				break
			}
		}
	}
	go streamLines(stdout)
	go streamLines(stderr)
	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("npm install failed: %w\n\nIf the problem persists, run manually:\n  npm install -g %s", err, entry.NPMPackage)
	}
	return nil
}

func installPipPackage(ctx context.Context, pkg string, progress func(line string)) error {
	pipCmd := pipBin()
	if _, err := exec.LookPath(pipCmd); err != nil {
		return fmt.Errorf(
			"pip not found in PATH.\n" +
				"Install Python 3.10+ from https://python.org,\n" +
				"then restart Vortelio and try again",
		)
	}
	args := []string{"install", "--upgrade", pkg}
	cmd := exec.CommandContext(ctx, pipCmd, args...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start pip: %w", err)
	}
	done := make(chan struct{}, 2)
	streamLines := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		var partial string
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := partial + string(buf[:n])
				lines := strings.Split(chunk, "\n")
				partial = lines[len(lines)-1]
				for _, l := range lines[:len(lines)-1] {
					l = strings.TrimSpace(l)
					if l != "" && progress != nil {
						progress(l)
					}
				}
			}
			if err != nil {
				break
			}
		}
	}
	go streamLines(stdout)
	go streamLines(stderr)
	<-done
	<-done
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("pip install %s failed: %w", pkg, err)
	}
	return nil
}

func installPip(ctx context.Context, entry CatalogEntry, progress func(line string)) error {
	pipCmd := pipBin()
	if _, err := exec.LookPath(pipCmd); err != nil {
		return fmt.Errorf(
			"pip not found in PATH.\n" +
				"Install Python 3.10+ from https://python.org,\n" +
				"then restart Vortelio and try again",
		)
	}

	args := []string{"install", "--upgrade", entry.NPMPackage}
	cmd := exec.CommandContext(ctx, pipCmd, args...)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start pip: %w", err)
	}

	done := make(chan struct{}, 2)
	streamLines := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		var partial string
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := partial + string(buf[:n])
				lines := strings.Split(chunk, "\n")
				partial = lines[len(lines)-1]
				for _, l := range lines[:len(lines)-1] {
					l = strings.TrimSpace(l)
					if l != "" && progress != nil {
						progress(l)
					}
				}
			}
			if err != nil {
				break
			}
		}
	}
	go streamLines(stdout)
	go streamLines(stderr)
	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("pip install failed: %w\n\nIf the problem persists, run manually:\n  pip install %s", err, entry.NPMPackage)
	}
	return nil
}

func installUV(ctx context.Context, entry CatalogEntry, progress func(string)) error {
	uv := uvBin()
	if _, err := exec.LookPath(uv); err != nil {
		return fmt.Errorf(
			"uv not found in PATH.\n" +
				"Install it with: pip install uv  or  curl -LsSf https://astral.sh/uv/install.sh | sh\n" +
				"then restart Vortelio and try again",
		)
	}

	venvDir := filepath.Join(config.HomeDir(), "venvs", entry.ID)

	// Step 1: create venv
	if progress != nil {
		progress("Creazione virtual environment in " + venvDir + "…")
	}
	venvCmd := exec.CommandContext(ctx, uv, "venv", venvDir)
	if out, err := venvCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uv venv failed: %w\n%s", err, string(out))
	}

	// Step 2: install each dep into the venv
	for _, dep := range entry.UVDeps {
		if progress != nil {
			progress("Installing " + dep + "…")
		}
		args := []string{"pip", "install", "--python", venvDir, "--upgrade", dep}
		cmd := exec.CommandContext(ctx, uv, args...)
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("uv pip install %s failed: %w", dep, err)
		}
		done := make(chan struct{}, 2)
		streamLines := func(r io.Reader) {
			defer func() { done <- struct{}{} }()
			buf := make([]byte, 4096)
			var partial string
			for {
				n, err := r.Read(buf)
				if n > 0 {
					chunk := partial + string(buf[:n])
					lines := strings.Split(chunk, "\n")
					partial = lines[len(lines)-1]
					for _, l := range lines[:len(lines)-1] {
						l = strings.TrimSpace(l)
						if l != "" && progress != nil {
							progress(l)
						}
					}
				}
				if err != nil {
					break
				}
			}
		}
		go streamLines(stdout)
		go streamLines(stderr)
		<-done
		<-done
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("uv pip install %s failed: %w", dep, err)
		}
	}
	return nil
}

// ── Start ─────────────────────────────────────────────────────────────────────

func Start(id string) error {
	if !validAgentID.MatchString(id) {
		return fmt.Errorf("invalid agent id: %q", id)
	}
	entry, ok := findEntry(id)
	if !ok {
		return fmt.Errorf("unknown agent: %s", id)
	}

	// Reserve the slot atomically so two concurrent Starts can't both pass the
	// "already running" check and spawn two processes.
	mu.Lock()
	if _, already := procMap[id]; already {
		mu.Unlock()
		return nil
	}
	procMap[id] = nil // reservation; replaced with the real cmd below
	mu.Unlock()
	failed := true
	defer func() {
		if failed {
			mu.Lock()
			delete(procMap, id)
			mu.Unlock()
		}
	}()

	if !isBinInstalled(entry) {
		if entry.InstallMethod == MethodUV {
			return fmt.Errorf("agent %q requires uv — install it with: pip install uv", entry.Name)
		}
		return fmt.Errorf("agent %q not installed — install it first via the Agents panel", entry.Name)
	}

	// Extract embedded script to disk if needed
	var scriptPath string
	if entry.ScriptName != "" {
		runnersDir := filepath.Join(config.HomeDir(), "runners")
		if err := os.MkdirAll(runnersDir, 0755); err != nil {
			return fmt.Errorf("could not create runners directory: %w", err)
		}
		scriptPath = filepath.Join(runnersDir, entry.ScriptName)
		var scriptData []byte
		switch entry.ScriptName {
		case "crewai_server.py":
			scriptData = crewaiServerScript
		}
		if scriptData != nil {
			if err := os.WriteFile(scriptPath, scriptData, 0644); err != nil {
				return fmt.Errorf("could not extract script: %w", err)
			}
		}
	}

	portStr := fmt.Sprintf("%d", entry.DefaultPort)
	extraArgs := append([]string{}, entry.StartArgs...)

	var cmd *exec.Cmd

	switch {
	case entry.InstallMethod == MethodUV && scriptPath != "":
		// Use python from the dedicated uv venv
		venvPython := uvVenvPython(entry.ID)
		pyArgs := []string{scriptPath, "--port", portStr, "--vortelio-url", vortURL(), "--home", config.HomeDir()}
		pyArgs = append(pyArgs, extraArgs...)
		cmd = exec.Command(venvPython, pyArgs...)

	case scriptPath != "":
		// Legacy pip-based script
		pyArgs := []string{scriptPath, "--port", portStr, "--vortelio-url", vortURL(), "--home", config.HomeDir()}
		pyArgs = append(pyArgs, extraArgs...)
		cmd = exec.Command(pythonBin(), pyArgs...)

	case runtime.GOOS == "windows":
		cmdArgs := append([]string{"/c", entry.BinCommand + ".cmd"}, extraArgs...)
		cmd = exec.Command("cmd", cmdArgs...)

	default:
		cmd = exec.Command(entry.BinCommand, extraArgs...)
	}
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Build env: inherit system env + inject agent-specific vars
	env := os.Environ()
	for _, kv := range entry.EnvVars {
		resolved := strings.ReplaceAll(kv, "{{VORTELIO_URL}}", vortURL())
		env = append(env, resolved)
	}
	// OpenCode reads its model provider from a config file, not from
	// OPENAI_* env vars. Generate a Vortelio-managed opencode.json pointing at
	// our OpenAI-compatible API and hand it over via OPENCODE_CONFIG so the
	// user's own global config is left untouched.
	if entry.ID == "opencode" {
		cfgPath := openCodeConfigPath()
		if err := writeOpenCodeConfig(cfgPath); err != nil {
			return fmt.Errorf("could not write opencode config: %w", err)
		}
		env = append(env, "OPENCODE_CONFIG="+cfgPath)
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start of %s failed: %w", entry.Name, err)
	}

	mu.Lock()
	procMap[id] = cmd
	mu.Unlock()
	failed = false

	go func() {
		cmd.Wait()
		mu.Lock()
		// Only remove our own entry — after a Stop+Start the slot may already
		// belong to a newer process.
		if procMap[id] == cmd {
			delete(procMap, id)
		}
		mu.Unlock()
	}()

	return nil
}

// RunForeground runs an interactive (TUI) agent attached to the current terminal
// and blocks until it exits. Used for agents like Open Code that have no HTTP
// port and need a real terminal to draw their UI.
func RunForeground(id string) error {
	if !validAgentID.MatchString(id) {
		return fmt.Errorf("invalid agent id: %q", id)
	}
	entry, ok := findEntry(id)
	if !ok {
		return fmt.Errorf("unknown agent: %s", id)
	}
	if !isBinInstalled(entry) {
		return fmt.Errorf("agent %q non installato", entry.Name)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/c", entry.BinCommand + ".cmd"}, entry.StartArgs...)...)
	} else {
		cmd = exec.Command(entry.BinCommand, entry.StartArgs...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	for _, kv := range entry.EnvVars {
		env = append(env, strings.ReplaceAll(kv, "{{VORTELIO_URL}}", vortURL()))
	}
	cmd.Env = env
	return cmd.Run()
}

// openCodeConfigPath is the Vortelio-managed opencode config file. It lives
// under Vortelio's home dir so it never clobbers the user's global
// ~/.config/opencode/opencode.json.
func openCodeConfigPath() string {
	return filepath.Join(config.HomeDir(), "opencode", "opencode.json")
}

// writeOpenCodeConfig generates an opencode.json that points OpenCode at
// Vortelio's OpenAI-compatible API and lists the installed LLM models, so the
// web UI works out of the box with local models (the same idea as
// `ollama launch opencode`). Handed to OpenCode via OPENCODE_CONFIG.
func writeOpenCodeConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	// Pull installed models from the running Vortelio server.
	models := map[string]map[string]string{}
	defaultModel := ""
	func() {
		cl := &http.Client{Timeout: 4 * time.Second}
		resp, err := cl.Get(vortURL() + "/api/models")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var payload struct {
			Models []struct {
				Name        string `json:"name"`
				Tag         string `json:"tag"`
				Type        string `json:"type"`
				DisplayName string `json:"display_name"`
			} `json:"models"`
		}
		if json.NewDecoder(resp.Body).Decode(&payload) != nil {
			return
		}
		for _, m := range payload.Models {
			if m.Type != "" && m.Type != "llm" {
				continue // only chat models are useful for a coding agent
			}
			id := m.Name
			if m.Tag != "" {
				id = m.Name + ":" + m.Tag
			}
			label := m.DisplayName
			if label == "" {
				label = id
			}
			models[id] = map[string]string{"name": label}
			if defaultModel == "" {
				defaultModel = "vortelio/" + id
			}
		}
	}()

	// Add every configured cloud model too — the unified /v1 gateway proxies
	// "provider/model" ids to the right cloud provider, so Open Code can use
	// ALL of Vortelio's models (local + cloud) under the one "vortelio" provider.
	for _, cm := range cloud.ModelsWithKeys() {
		id := cm.Provider + "/" + cm.Model
		label := cm.Label
		if label == "" {
			label = id
		}
		models[id] = map[string]string{"name": cm.ProviderName + " · " + label}
		if defaultModel == "" {
			defaultModel = "vortelio/" + id
		}
	}

	cfg := map[string]interface{}{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]interface{}{
			"vortelio": map[string]interface{}{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Vortelio (local)",
				"options": map[string]string{
					"baseURL": vortURL() + "/v1",
					"apiKey":  "vortelio",
				},
				"models": models,
			},
		},
	}
	if defaultModel != "" {
		cfg["model"] = defaultModel
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// IsInteractive reports whether an agent is a terminal TUI (no HTTP server).
func IsInteractive(id string) bool {
	entry, ok := findEntry(id)
	return ok && entry.DefaultPort == 0 && entry.DefaultURL == ""
}

// Stop terminates the agent process.
func Stop(id string) error {
	mu.Lock()
	cmd, ok := procMap[id]
	mu.Unlock()
	if !ok {
		return nil
	}
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	mu.Lock()
	delete(procMap, id)
	mu.Unlock()
	return nil
}

// Uninstall removes the npm global package.
func Uninstall(id string) error {
	if !validAgentID.MatchString(id) {
		return fmt.Errorf("invalid agent id: %q", id)
	}
	Stop(id)
	entry, ok := findEntry(id)
	if !ok {
		return fmt.Errorf("unknown agent")
	}
	switch entry.InstallMethod {
	case MethodNPM:
		npmCmd := "npm"
		if runtime.GOOS == "windows" {
			npmCmd = "npm.cmd"
		}
		return exec.Command(npmCmd, "uninstall", "-g", entry.NPMPackage).Run()
	case MethodPip:
		pipCmd := "pip"
		if _, err := exec.LookPath("pip3"); err == nil {
			pipCmd = "pip3"
		}
		return exec.Command(pipCmd, "uninstall", "-y", entry.NPMPackage).Run()
	case MethodUV:
		// Remove dedicated venv directory
		venvDir := filepath.Join(config.HomeDir(), "venvs", entry.ID)
		return os.RemoveAll(venvDir)
	default:
		return fmt.Errorf("uninstall not supported for this agent type")
	}
}

// Health checks if the agent HTTP server is responding.
func Health(id string) (bool, string) {
	entry, ok := findEntry(id)
	if !ok {
		return false, "unknown agent"
	}
	if entry.DefaultURL == "" {
		// TUI agent — check if process is alive
		mu.Lock()
		_, alive := procMap[id]
		mu.Unlock()
		if alive {
			return true, "process running (no HTTP endpoint)"
		}
		return false, "not running"
	}
	url := entry.DefaultURL + entry.HealthPath
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode < 400, string(body)
}

// AgentBinDir returns the agents directory inside VORTELIO_HOME.
func AgentBinDir() string {
	return filepath.Join(config.HomeDir(), "agents")
}

// ── JSON catalog ──────────────────────────────────────────────────────────────

func CatalogJSON() []byte {
	type entry struct {
		CatalogEntry
		State AgentState `json:"state"`
	}
	out := make([]entry, len(Catalog))
	for i, e := range Catalog {
		out[i] = entry{CatalogEntry: e, State: GetState(e.ID)}
	}
	b, _ := json.Marshal(out)
	return b
}

func findEntry(id string) (CatalogEntry, bool) {
	for _, e := range Catalog {
		if e.ID == id {
			return e, true
		}
	}
	return CatalogEntry{}, false
}
