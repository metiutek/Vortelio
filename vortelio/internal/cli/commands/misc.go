package commands

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/server"
)

// ─── LIST ─────────────────────────────────────────────────────────────────────

type ListCommand struct{}

func NewListCommand() *ListCommand  { return &ListCommand{} }
func (c *ListCommand) Name() string { return "list" }
func (c *ListCommand) Run(args []string) error {
	store := hub.NewModelStore()
	models, err := store.List()
	if err != nil {
		return fmt.Errorf("failed to list models: %w", err)
	}
	if len(models) == 0 {
		fmt.Println("No models downloaded yet.")
		fmt.Println("Try: vortelio pull llm/mistral:7b")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSIZE\tFORMAT\tDOWNLOADED")
	fmt.Fprintln(w, "────\t────\t────\t──────\t─────────")
	for _, m := range models {
		fmt.Fprintf(w, "%s/%s:%s\t%s\t%s\t%s\t%s\n",
			m.Type, m.Name, m.Tag, m.Type, m.SizeHuman(), m.Format,
			m.DownloadedAt.Format("2006-01-02"))
	}
	w.Flush()
	return nil
}

// ─── REMOVE ───────────────────────────────────────────────────────────────────

type RemoveCommand struct{}

func NewRemoveCommand() *RemoveCommand { return &RemoveCommand{} }
func (c *RemoveCommand) Name() string  { return "remove" }
func (c *RemoveCommand) Run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vortelio remove <model> [model2 ...] [--all]")
	}
	store := hub.NewModelStore()
	if len(args) == 1 && args[0] == "--all" {
		models, err := store.List()
		if err != nil || len(models) == 0 {
			fmt.Println("No models installed.")
			return nil
		}
		fmt.Printf("⚠️   You are about to remove %d models:\n", len(models))
		for _, m := range models {
			fmt.Printf("    • %s/%s:%s (%s)\n", m.Type, m.Name, m.Tag, m.SizeHuman())
		}
		fmt.Print("\nConfermi? [y/N]: ")
		var answer string
		fmt.Scanln(&answer)
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			fmt.Println("Cancelled.")
			return nil
		}
		removed := 0
		for _, m := range models {
			ref := &hub.ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
			if err := store.Remove(ref); err != nil {
				fmt.Fprintf(os.Stderr, "❌  %s/%s:%s: %v\n", m.Type, m.Name, m.Tag, err)
			} else {
				fmt.Printf("🗑️   Removed %s/%s:%s\n", m.Type, m.Name, m.Tag)
				removed++
			}
		}
		fmt.Printf("\n✅  %d models removed.\n", removed)
		return nil
	}
	failed := 0
	for _, arg := range args {
		ref, err := hub.ParseModelRef(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌  %s: %v\n", arg, err)
			failed++
			continue
		}
		if err := store.Remove(ref); err != nil {
			fmt.Fprintf(os.Stderr, "❌  %s: %v\n", arg, err)
			failed++
		} else {
			fmt.Printf("🗑️   Removed %s/%s:%s\n", ref.Type, ref.Name, ref.Tag)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d models not removed", failed)
	}
	return nil
}

// ─── INFO ─────────────────────────────────────────────────────────────────────

type InfoCommand struct{}

func NewInfoCommand() *InfoCommand  { return &InfoCommand{} }
func (c *InfoCommand) Name() string { return "info" }
func (c *InfoCommand) Run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vortelio info <type/model[:tag]>")
	}
	ref, err := hub.ParseModelRef(args[0])
	if err != nil {
		return err
	}
	store := hub.NewModelStore()
	m, err := store.Resolve(ref)
	if err != nil {
		return fmt.Errorf("model not found: %w", err)
	}
	fmt.Printf("📦  Model Info\n")
	fmt.Printf("    Name:        %s/%s:%s\n", m.Type, m.Name, m.Tag)
	fmt.Printf("    Type:        %s\n", m.Type)
	fmt.Printf("    Format:      %s\n", m.Format)
	fmt.Printf("    Size:        %s\n", m.SizeHuman())
	fmt.Printf("    Path:        %s\n", m.LocalPath)
	fmt.Printf("    Source:      %s\n", m.Source)
	fmt.Printf("    Downloaded:  %s\n", m.DownloadedAt.Format("2006-01-02 15:04:05"))
	return nil
}

// ─── GUI ──────────────────────────────────────────────────────────────────────

type GUICommand struct{}

func NewGUICommand() *GUICommand   { return &GUICommand{} }
func (c *GUICommand) Name() string { return "gui" }
func (c *GUICommand) Run(args []string) error {
	port := "11500"
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--port" || args[i] == "-p" {
			port = args[i+1]
		}
	}
	url := "http://localhost:" + port
	_, err := EnsureServiceRunning(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌  %v\n", err)
		fmt.Fprintf(os.Stderr, "    Prova: vortelio serve --port %s\n", port)
	}
	fmt.Printf("🌐  Apertura GUI: %s\n", url)
	openDesktopWindow(url)
	return nil
}

// ─── SERVE ────────────────────────────────────────────────────────────────────

type ServeCommand struct{}

func NewServeCommand() *ServeCommand { return &ServeCommand{} }
func (c *ServeCommand) Name() string { return "serve" }
func (c *ServeCommand) Run(args []string) error {
	port := ""
	host := ""
	apiKey := ""
	noBrowser := false
	remote := false
	background := false
	var bgArgs []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			if i+1 < len(args) {
				port = args[i+1]
				bgArgs = append(bgArgs, args[i], args[i+1])
				i++
			}
		case "--host":
			if i+1 < len(args) {
				host = args[i+1]
				bgArgs = append(bgArgs, args[i], args[i+1])
				i++
			}
		case "--remote":
			remote = true
			bgArgs = append(bgArgs, args[i])
		case "--api-key":
			if i+1 < len(args) {
				apiKey = args[i+1]
				bgArgs = append(bgArgs, args[i], args[i+1])
				i++
			}
		case "--no-browser":
			noBrowser = true
		case "--bg", "--background":
			background = true
		}
	}

	if background {
		pid, err := LaunchServiceDetachedWithArgs(bgArgs)
		if err != nil {
			return fmt.Errorf("background start failed: %w", err)
		}
		portDisp := port
		if portDisp == "" {
			portDisp = "11500"
		}
		fmt.Printf("Vortelio started in background (PID: %d)\n", pid)
		fmt.Printf("GUI: http://localhost:%s\n", portDisp)
		fmt.Printf("Stop with: vortelio stop\n")
		return nil
	}

	cfg := config.Load()
	if port != "" {
		var p int
		fmt.Sscanf(port, "%d", &p)
		if p > 0 {
			cfg.Port = p
		}
	}
	if host != "" {
		cfg.BindAddr = host
	}
	if remote && cfg.BindAddr == "127.0.0.1" {
		cfg.BindAddr = "0.0.0.0"
	}
	if apiKey != "" {
		cfg.APIKey = apiKey
	}

	portStr := fmt.Sprintf("%d", cfg.Port)
	addr := cfg.BindAddr + ":" + portStr
	localURL := "http://localhost:" + portStr
	mux := server.NewMux()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if strings.Contains(err.Error(), "bind") || strings.Contains(err.Error(), "address already in use") {
			fmt.Printf("ℹ️   Server already running at %s\n", localURL)
			if !noBrowser {
				fmt.Println("    Apertura GUI...")
				openDesktopWindow(localURL)
			}
			return nil
		}
		return err
	}

	fmt.Printf("🌐  Vortelio Web UI → %s\n", localURL)
	if cfg.BindAddr != "127.0.0.1" && cfg.BindAddr != "localhost" {
		hostname, _ := os.Hostname()
		fmt.Printf("🔗  Accesso remoto  → http://%s:%s\n", hostname, portStr)
		if cfg.APIKey != "" {
			fmt.Printf("🔑  API key attiva  (Authorization: Bearer %s)\n", cfg.APIKey)
		} else {
			fmt.Printf("⚠️   No API key — remote access open to everyone\n")
		}
	}
	fmt.Printf("    Press Ctrl+C to stop.\n\n")
	// Install llama.cpp if missing / keep it current, in the background.
	server.AutoUpdateEngine(func(format string, args ...any) {
		fmt.Printf("⚙️   "+format+"\n", args...)
	})

	server.InitLogger("info")

	if !noBrowser {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openDesktopWindow(localURL)
		}()
	}

	srv := &http.Server{Handler: mux}

	// Graceful shutdown su SIGINT/SIGTERM o /api/shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-quit:
		case <-server.ShutdownCh():
		}
		fmt.Println("\n⏹️   Arresto server...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Println("✅  Server stopped.")
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ─── STOP ─────────────────────────────────────────────────────────────────────

type StopCommand struct{}

func NewStopCommand() *StopCommand  { return &StopCommand{} }
func (c *StopCommand) Name() string { return "stop" }
func (c *StopCommand) Run(args []string) error {
	port := "11500"
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--port" || args[i] == "-p" {
			port = args[i+1]
		}
	}

	// Try graceful HTTP shutdown first
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post("http://localhost:"+port+"/api/shutdown", "application/json", nil)
	if err == nil {
		resp.Body.Close()
		fmt.Println("Vortelio stopped.")
		return nil
	}

	// Fallback: PID file
	pidPath := filepath.Join(config.HomeDir(), "vortelio.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("server not running (PID file not found)")
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	if pid == 0 {
		return fmt.Errorf("invalid PID in file %s", pidPath)
	}
	if err := killProcess(pid); err != nil {
		return fmt.Errorf("could not stop process (PID %d): %w", pid, err)
	}
	os.Remove(pidPath)
	fmt.Printf("Vortelio stopped (PID %d)\n", pid)
	return nil
}

func openDesktopWindow(url string) {
	if runtime.GOOS != "windows" {
		openBrowser(url)
		return
	}
	edgePaths := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	chromePaths := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	home, _ := os.UserHomeDir()
	// Bumped to a fresh profile so Edge/Chrome re-derives the window/taskbar icon
	// from the now-served favicon (the old profile had cached "no favicon").
	profileDir := filepath.Join(home, ".vortelio", "app-profile2")
	os.MkdirAll(profileDir, 0755)
	appArgs := func(exe string) *exec.Cmd {
		return exec.Command(exe,
			"--app="+url,
			"--user-data-dir="+profileDir,
			"--window-size=1280,820",
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-extensions",
		)
	}
	for _, p := range append(edgePaths, chromePaths...) {
		if _, err := os.Stat(p); err == nil {
			if err := appArgs(p).Start(); err == nil {
				return
			}
		}
	}
	openBrowser(url)
}
