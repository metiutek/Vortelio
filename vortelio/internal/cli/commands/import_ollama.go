package commands

import (
	"fmt"

	"github.com/vortelio/vortelio/internal/hub"
)

type ImportOllamaCommand struct{}

func NewImportOllamaCommand() *ImportOllamaCommand { return &ImportOllamaCommand{} }
func (c *ImportOllamaCommand) Name() string        { return "import-ollama" }

func (c *ImportOllamaCommand) Run(args []string) error {
	dryRun := false
	customPath := ""
	for i, a := range args {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		case "--path":
			if i+1 < len(args) {
				customPath = args[i+1]
			}
		}
	}

	if customPath == "" {
		customPath = hub.OllamaDefaultDir()
	}
	fmt.Printf("📦 Importing Ollama models from: %s\n", customPath)
	if dryRun {
		fmt.Println("    (dry run — no changes)")
	}

	// Import directly — no running server needed. The models are registered in
	// the same store the server uses, so they show up in the GUI immediately.
	res, err := hub.ImportOllama(customPath, dryRun)
	if err != nil {
		return err
	}

	for _, it := range res.Imported {
		mark := "✅"
		if dryRun {
			mark = "🔎"
		}
		fmt.Printf("    %s %s\n", mark, it.Model)
	}
	for _, it := range res.Skipped {
		fmt.Printf("    ⏭  %s — %s\n", it.Model, it.Reason)
	}
	fmt.Printf("\n%d modelli importati.\n", res.Count)
	return nil
}
