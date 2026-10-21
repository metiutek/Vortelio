package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/pkg/progress"
)

type PullCommand struct{}

func NewPullCommand() *PullCommand { return &PullCommand{} }

func (c *PullCommand) Name() string { return "pull" }

func (c *PullCommand) Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: vortelio pull <model> [model2 ...] [--file <path> <type>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Single model:")
		fmt.Fprintln(os.Stderr, "  vortelio pull llm/mistral:7b")
		fmt.Fprintln(os.Stderr, "  vortelio pull image/z-image:turbo")
		fmt.Fprintln(os.Stderr, "  vortelio pull llm/hf.co/unsloth/Qwen3.5-4B-Instruct-GGUF:Q4_K_M")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Multiple models at once:")
		fmt.Fprintln(os.Stderr, "  vortelio pull llm/mistral:7b image/z-image:turbo audio/whisper:base")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "From a local file:")
		fmt.Fprintln(os.Stderr, "  vortelio pull --file ./my-model.gguf llm")
		return nil
	}

	// Check for --file flag
	for i, a := range args {
		if a == "--file" || a == "-f" {
			if i+2 > len(args)-1 {
				return fmt.Errorf("--file requires: --file <path> <type>")
			}
			return c.importLocal(args[i+1], args[i+2])
		}
	}

	// Collect all model refs (skip flags)
	var refs []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			refs = append(refs, a)
		}
	}

	if len(refs) == 0 {
		return fmt.Errorf("no model specified")
	}

	// Download each model
	failed := 0
	for idx, modelRef := range refs {
		if len(refs) > 1 {
			fmt.Printf("\n[%d/%d] %s\n", idx+1, len(refs), modelRef)
			fmt.Println(strings.Repeat("─", 50))
		}

		// Validate / prompt for type prefix
		if !strings.Contains(modelRef, "/") {
			fmt.Fprintf(os.Stderr, "\n⚠️   Missing type prefix for %q\n", modelRef)
			fmt.Fprintf(os.Stderr, "   Valid types: llm | image | audio | video | 3d\n")
			fmt.Fprintf(os.Stderr, "   Enter type: ")
			var typInput string
			if _, scanErr := fmt.Fscan(os.Stdin, &typInput); scanErr == nil && typInput != "" {
				modelRef = typInput + "/" + modelRef
			} else {
				fmt.Fprintf(os.Stderr, "❌  type not provided, skipping\n")
				failed++
				continue
			}
		}
		ref, err := hub.ParseModelRef(modelRef)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌  %s: %v\n", modelRef, err)
			failed++
			continue
		}

		fmt.Printf("🔍  %s/%s:%s\n", ref.Type, ref.Name, ref.Tag)

		downloader := hub.NewDownloader()
		bar := progress.NewBar(fmt.Sprintf("Pulling %s", modelRef))

		err = downloader.Pull(ref, func(downloaded, total int64) {
			bar.Update(downloaded, total)
		})
		if err != nil {
			bar.Fail()
			fmt.Fprintf(os.Stderr, "❌  %s: %v\n", modelRef, err)
			failed++
			continue
		}
		bar.Done()
		fmt.Printf("✅  Pronto: %s\n", modelRef)
	}

	if len(refs) > 1 {
		fmt.Printf("\n%d/%d models downloaded successfully.\n", len(refs)-failed, len(refs))
	}
	if failed > 0 {
		return fmt.Errorf("%d downloads failed", failed)
	}
	return nil
}

func (c *PullCommand) importLocal(path, modelType string) error {
	if modelType == "" {
		return fmt.Errorf("--file requires the type as second argument (llm, image, audio, video, 3d)")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("file not found: %s", path)
	}
	fmt.Printf("📦  Importing local model from %s (type=%s)...\n", path, modelType)
	importer := hub.NewLocalImporter()
	if err := importer.Import(path, modelType); err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	fmt.Println("✅  Model imported.")
	fmt.Println("    Run: vortelio list")
	return nil
}
