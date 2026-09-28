package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	vrt "github.com/vortelio/vortelio/internal/runtime"
)

// SetupCommand downloads and installs llama.cpp automatically.
type SetupCommand struct{}

func NewSetupCommand() *SetupCommand { return &SetupCommand{} }

func (c *SetupCommand) Name() string { return "setup" }

func (c *SetupCommand) Run(args []string) error {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}

	fmt.Println("🔧  Vortelio Setup — configurazione automatica")
	fmt.Println()

	// ── Step 1: llama.cpp (installed, and kept up to date) ───
	fmt.Print("1. Checking llama.cpp... ")
	st := vrt.CheckLlama()
	switch {
	case st.Installed && !st.UpdateAvailable && !force:
		fmt.Printf("✅  build %d (%s)\n", st.Build, st.Path)
	case st.Installed && !st.Managed && !force:
		fmt.Printf("✅  build %d (%s — not managed by Vortelio, left as is)\n", st.Build, st.Path)
	default:
		switch {
		case force:
			fmt.Println("(re-installing)")
		case st.Installed:
			fmt.Printf("⬆️  build %d → %d\n", st.Build, st.LatestBuild)
		default:
			fmt.Println("❌  not found")
		}
		vrt.GlobalModelManager.UnloadAll()
		if build, err := vrt.InstallLlamaCpp(nil, vrt.CLIProgress()); err != nil {
			fmt.Printf("\n   ⚠️   Download failed: %v\n", err)
			fmt.Println("   Install manually from: https://github.com/ggml-org/llama.cpp/releases")
		} else {
			fmt.Printf("\n   ✅  llama.cpp build %d installed in %s\n", build, vrt.LlamaDir())
		}
	}

	// ── Step 2: Verifica Python ──────────────────────────────
	fmt.Print("2. Checking Python 3... ")
	py := findPython()
	if py != "" {
		out, _ := exec.Command(py, "--version").CombinedOutput()
		fmt.Printf("✅  %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("⚠️   not found")
		fmt.Println("   Necessario per immagini, audio e video.")
		printPythonInstallHint()
	}

	// ── Step 3: Verifica pacchetti Python ────────────────────
	if py != "" {
		fmt.Print("3. Checking Python packages (diffusers, torch, whisper)... ")
		missing := checkPythonPackages(py)
		if len(missing) == 0 {
			fmt.Println("✅  all installed")
		} else {
			fmt.Printf("⚠️   mancanti: %s\n", strings.Join(missing, ", "))
			fmt.Println("   → Installing...")
			installPythonPackages(py, missing)
		}
	}

	// ── Step 4: PATH check ───────────────────────────────────
	fmt.Print("4. Checking system PATH... ")
	selfDir := ""
	if exe, err := os.Executable(); err == nil {
		selfDir = filepath.Dir(exe)
	}
	if isInPath(selfDir) {
		fmt.Println("✅  vortelio available globally")
	} else {
		fmt.Printf("⚠️   %s is not in PATH\n", selfDir)
		if runtime.GOOS == "windows" {
			fmt.Println("   → Adding to system PATH...")
			addToPathWindows(selfDir)
		} else {
			fmt.Printf("   Add manually: export PATH=\"%s:$PATH\"\n", selfDir)
		}
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("✅  Setup complete! Open a new terminal and try:")
	fmt.Println()
	fmt.Println("   vortelio pull llm/mistral:7b")
	fmt.Println("   vortelio run llm/mistral:7b \"hello!\"")
	fmt.Println()
	fmt.Println("   vortelio pull image/sdxl")
	fmt.Println("   vortelio run image/sdxl \"a sunset over the sea\"")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return nil
}

// ── Helpers ────────────────────────────────────────────────

func findPython() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func checkPythonPackages(py string) []string {
	packages := []string{"diffusers", "torch", "whisper", "transformers"}
	var missing []string
	for _, pkg := range packages {
		out, _ := exec.Command(py, "-c", "import "+pkg).CombinedOutput()
		if len(out) > 0 {
			missing = append(missing, pkg)
		}
	}
	return missing
}

func installPythonPackages(py string, pkgs []string) {
	args := append([]string{"-m", "pip", "install", "--quiet"}, pkgs...)
	cmd := exec.Command(py, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("   ⚠️   Partial installation: %v\n", err)
	} else {
		fmt.Println("   ✅  Packages installed")
	}
}

func isInPath(dir string) bool {
	pathEnv := os.Getenv("PATH")
	if runtime.GOOS == "windows" {
		pathEnv = strings.ToLower(pathEnv)
		dir = strings.ToLower(dir)
	}
	for _, p := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}

func addToPathWindows(dir string) {
	cmd := exec.Command("powershell", "-NonInteractive", "-Command",
		fmt.Sprintf(`$p=[Environment]::GetEnvironmentVariable("Path","Machine"); if($p -notlike "*%s*"){[Environment]::SetEnvironmentVariable("Path","$p;%s","Machine"); Write-Host "PATH updated"}else{Write-Host "PATH already configured"}`, dir, dir),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func printPythonInstallHint() {
	switch runtime.GOOS {
	case "windows":
		fmt.Println("   Download from: https://www.python.org/downloads/")
		fmt.Println("   Oppure: winget install Python.Python.3.12")
	case "darwin":
		fmt.Println("   brew install python@3.12")
	default:
		fmt.Println("   sudo apt install python3 python3-pip  # Ubuntu/Debian")
	}
}
