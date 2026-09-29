package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/runtime"
	"golang.org/x/term"
)

func handleModelloCloud() error {
	// Built-in providers, the user's custom endpoints, then "add custom".
	provs := cloud.AllProviders()
	labels := make([]string, 0, len(provs)+3)
	for _, p := range provs {
		l := p.Name
		if cloud.Configured(p.ID) {
			l += "  ✓"
		}
		labels = append(labels, l)
	}
	labels = append(labels,
		"+ Custom OpenAI-compatible endpoint (/v1)",
		"+ Custom Anthropic-compatible endpoint",
		"← Back")

	sel := selectMenu("Cloud Models", labels)
	if sel < 0 || sel == len(provs)+2 {
		return nil
	}
	if sel >= len(provs) {
		format := cloud.FormatOpenAI
		if sel == len(provs)+1 {
			format = cloud.FormatAnthropic
		}
		p, ok := addCustomEndpoint(format)
		if !ok {
			return nil
		}
		return runCloudChat(p)
	}
	return runCloudChat(provs[sel])
}

// addCustomEndpoint asks for an OpenAI/Anthropic-compatible server and saves it.
func addCustomEndpoint(format cloud.APIFormat) (cloud.Provider, bool) {
	fmt.Print("\033[H\033[2J")
	fmt.Printf("\n  Custom %s-compatible endpoint\n\n", format)
	fmt.Print("  Base URL (e.g. http://localhost:1234/v1): ")
	base := strings.TrimSpace(readLine())
	if base == "" {
		waitKey("  ❌  No URL entered. Operation cancelled.")
		return cloud.Provider{}, false
	}
	fmt.Print("  Name (enter = host): ")
	name := strings.TrimSpace(readLine())
	fmt.Print("  Default model id (optional): ")
	model := strings.TrimSpace(readLine())
	fmt.Print("  API key (enter = none): ")
	key := strings.TrimSpace(readLine())
	p, err := cloud.SaveCustomProvider(cloud.CustomProvider{Name: name, BaseURL: base, Format: format, DefaultModel: model, NoKey: key == ""})
	if err != nil {
		waitKey("  ❌  " + err.Error())
		return cloud.Provider{}, false
	}
	if key != "" {
		_ = cloud.SaveKey(p.ID, key)
	}
	return p, true
}

func runCloudChat(p cloud.Provider) error {
	// Ensure terminal is in normal (non-raw) mode for text input
	fmt.Print("\033[H\033[2J")
	fmt.Printf("\n  %s\n", p.Name)
	if p.DefaultModel != "" {
		fmt.Printf("  Model: %s  [enter = keep, or type another model id]: ", p.DefaultModel)
	} else {
		fmt.Print("  Model id: ")
	}
	if m := strings.TrimSpace(readLine()); m != "" {
		p = cloud.ForModel(p, m)
	}
	if p.DefaultModel == "" {
		waitKey("  ❌  No model selected. Operation cancelled.")
		return nil
	}
	fmt.Println()

	// Load or ask for API key
	apiKey := cloud.LoadKey(p.ID)
	if p.NoKey && apiKey == "" {
		// keyless custom endpoint
	} else if apiKey == "" {
		fmt.Printf("  No API key found for %s.\n", p.Name)
		fmt.Printf("  Get your key at: %s\n\n", p.KeyHint)
		fmt.Print("  Paste your API key: ")
		apiKey = readLine()
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			waitKey("  ❌  No API key entered. Operation cancelled.")
			return nil
		}
		if err := cloud.SaveKey(p.ID, apiKey); err != nil {
			fmt.Printf("  ⚠  Could not save key: %s\n", err)
		} else {
			fmt.Printf("  ✅  Key saved.\n\n")
		}
	} else {
		masked := maskKey(apiKey)
		fmt.Printf("  API key: %s  (saved)\n", masked)
		fmt.Printf("  [Press Enter to use it, or type a new key]: ")
		line := readLine()
		line = strings.TrimSpace(line)
		if line != "" {
			apiKey = line
			if err := cloud.SaveKey(p.ID, apiKey); err != nil {
				fmt.Printf("  ⚠  Could not save key: %s\n", err)
			} else {
				fmt.Printf("  ✅  New key saved.\n\n")
			}
		}
	}

	// Chat loop. Use all stored keys for failover (the active key is saved above
	// so it's first); fall back to the just-entered one if nothing is stored.
	keys := cloud.KeysFor(p.ID)
	if len(keys) == 0 {
		keys = []string{apiKey}
	}
	var history []cloud.Message
	fmt.Printf("\n  Chat with %s  (type 'exit' to quit)\n", p.Name)
	fmt.Println("  " + strings.Repeat("─", 50))

	for {
		fmt.Print("\n  You: ")
		input := readLine()
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.ToLower(input) == "exit" || strings.ToLower(input) == "quit" {
			break
		}

		history = append(history, cloud.Message{Role: "user", Content: input})

		fmt.Printf("\n  %s:\n  ", p.Name)

		toolOpts := &cloud.ToolCallOptions{
			Tools:    runtime.BuiltinTools(),
			ExecTool: runtime.ExecuteTool,
		}
		response, err := cloud.ChatWithToolsFailover(p, keys, history, toolOpts, func(tok string) {
			tok = strings.ReplaceAll(tok, "\n", "\n  ")
			fmt.Print(tok)
		})
		fmt.Println()

		if err != nil {
			fmt.Printf("\n  ❌  Error: %s\n", err)
			// Remove the user message we added since the request failed
			history = history[:len(history)-1]
			fmt.Print("\n  Retry? (enter=yes / exit=no): ")
			ans := readLine()
			if strings.ToLower(strings.TrimSpace(ans)) == "exit" {
				break
			}
			continue
		}

		history = append(history, cloud.Message{Role: "assistant", Content: response})
		fmt.Println("  " + strings.Repeat("─", 50))
	}

	waitKey(fmt.Sprintf("  Chat with %s ended.", p.Name))
	return nil
}

// stdinReader is a single shared buffered reader. Allocating a new
// bufio.Scanner per readLine() call (the previous approach) discarded any bytes
// it buffered past the newline, losing input on multi-line paste. One shared
// reader keeps that remainder for the next call.
var stdinReader = bufio.NewReader(os.Stdin)

// readLine reads a full line in normal (non-raw) terminal mode.
func readLine() string {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		// Restore normal mode temporarily (it may already be normal, that's fine)
		old, err := term.GetState(fd)
		if err == nil {
			term.Restore(fd, old)
		}
	}
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// maskKey shows only the first 4 and last 4 chars.
func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}
