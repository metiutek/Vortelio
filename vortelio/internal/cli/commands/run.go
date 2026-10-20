package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/pkg/progress"
)

// RunCommand executes a model with given input.
type RunCommand struct{}

func NewRunCommand() *RunCommand { return &RunCommand{} }

func (c *RunCommand) Name() string { return "run" }

func (c *RunCommand) Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: vortelio run <type/model[:tag]> [input] [flags]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Flags:")
		fmt.Fprintln(os.Stderr, "  --input  <path>      Input file (audio, video, image)")
		fmt.Fprintln(os.Stderr, "  --output <path>      Output file path")
		fmt.Fprintln(os.Stderr, "  --steps  <n>         Diffusion steps (image/video, default: 20)")
		fmt.Fprintln(os.Stderr, "  --gpu    <id>        GPU device index (default: 0)")
		fmt.Fprintln(os.Stderr, "  --cpu                Force CPU-only mode")
		fmt.Fprintln(os.Stderr, "  --stream             Stream output (LLM only)")
		fmt.Fprintln(os.Stderr, "  --no-stream          Disable streaming")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  vortelio run llm/mistral:7b \"Tell me a joke\"")
		fmt.Fprintln(os.Stderr, "  vortelio run image/sdxl \"a cat in space\" --output ./cat.png")
		fmt.Fprintln(os.Stderr, "  vortelio run audio/whisper:large --input ./speech.mp3")
		fmt.Fprintln(os.Stderr, "  vortelio run video/cogvideo \"a horse running\" --output ./horse.mp4")
		return nil
	}

	modelRef := args[0]
	rest := args[1:]

	// Parse flags and positional prompt
	opts := &runtime.RunOptions{
		GPU:    0,
		Steps:  20,
		Stream: true, // default for LLM
	}
	var promptParts []string

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--input", "-i":
			if i+1 >= len(rest) {
				return fmt.Errorf("--input requires a path")
			}
			opts.InputFile = rest[i+1]
			i++
		case "--output", "-o":
			if i+1 >= len(rest) {
				return fmt.Errorf("--output requires a path")
			}
			opts.OutputFile = rest[i+1]
			i++
		case "--steps":
			if i+1 >= len(rest) {
				return fmt.Errorf("--steps requires a number")
			}
			fmt.Sscanf(rest[i+1], "%d", &opts.Steps)
			i++
		case "--gpu":
			if i+1 >= len(rest) {
				return fmt.Errorf("--gpu requires a device index")
			}
			fmt.Sscanf(rest[i+1], "%d", &opts.GPU)
			i++
		case "--cpu":
			opts.ForceCPU = true
		case "--stream":
			opts.Stream = true
		case "--no-stream":
			opts.Stream = false
		default:
			if !strings.HasPrefix(rest[i], "--") {
				promptParts = append(promptParts, rest[i])
			}
		}
	}
	opts.Prompt = strings.Join(promptParts, " ")

	// Parse model reference
	ref, err := hub.ParseModelRef(modelRef)
	if err != nil {
		return fmt.Errorf("invalid model reference: %w", err)
	}

	// Resolve model from local store — auto-pull if not found
	store := hub.NewModelStore()
	model, err := store.Resolve(ref)
	if err != nil {
		fmt.Printf("📦  Model %q not found locally. Auto-downloading...\n\n", modelRef)
		dl := hub.NewDownloader()
		bar := progress.NewBar(modelRef)
		pullErr := dl.Pull(ref, func(downloaded, total int64) {
			bar.Update(downloaded, total)
		})
		if pullErr != nil {
			return fmt.Errorf("download failed: %w\n  Try manually: vortelio pull %s", pullErr, modelRef)
		}
		bar.Done()
		model, err = store.Resolve(ref)
		if err != nil {
			return fmt.Errorf("model unavailable after download: %w", err)
		}
	}

	// Detect hardware
	hw := runtime.DetectHardware()
	if opts.ForceCPU {
		hw.Backend = runtime.BackendCPU
	}
	fmt.Printf("⚙️   Hardware: %s\n", hw.String())

	// Dispatch to appropriate runtime
	runner, err := runtime.NewRunner(model, hw)
	if err != nil {
		return fmt.Errorf("failed to initialize runner: %w", err)
	}

	fmt.Printf("🚀  Running %s/%s:%s\n\n", ref.Type, ref.Name, ref.Tag)
	// Native media engines run as child servers: stop them before exiting,
	// or they outlive the command and keep the model in memory.
	defer runtime.StopImageServer()
	defer runtime.StopAudioServers()
	return runner.Run(opts)
}
