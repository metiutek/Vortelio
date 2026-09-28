package runtime

import "strings"

// toolCapableFamilies lists model name fragments whose chat templates are known to
// support native function/tool calling in llama.cpp (with --jinja) or via cloud APIs.
// Matching is case-insensitive substring matching against the model name/ref.
var toolCapableFamilies = []string{
	"llama-3.1", "llama3.1", "llama-3.2", "llama3.2", "llama-3.3", "llama3.3",
	"qwen2", "qwen2.5", "qwen3", "qwq",
	"mistral", "mixtral", "ministral", "magistral", "devstral", "codestral",
	"hermes", "nous-hermes",
	"command-r", "command-a",
	"firefunction", "functionary", "gorilla", "watt-tool", "groq-tool",
	"granite-3", "granite3",
	"phi-4", "phi4",
	"gpt-oss", "gpt-4", "gpt-4o", "gpt-3.5", "o1", "o3", "o4",
	"claude", "gemini", "gemma-3", "gemma3", "gemma-4", "gemma4",
	"deepseek-v3", "deepseek-chat", "deepseek-r1",
	"smollm2", "llama-4", "llama4",
}

// ModelSupportsTools reports whether the model (by name or ref like "llm/qwen2.5:7b")
// is expected to support native tool/function calling. It is a heuristic based on the
// model family; callers should let the user force-enable when they know better.
func ModelSupportsTools(modelRef string) bool {
	n := strings.ToLower(modelRef)
	for _, fam := range toolCapableFamilies {
		if strings.Contains(n, fam) {
			return true
		}
	}
	return false
}

// TruncateForContext caps a tool result that gets fed back into the model's
// context so large outputs (e.g. web_search JSON) don't waste the context
// window. The UI still receives the full result via the tool event.
func TruncateForContext(s string, max int) string {
	if max <= 0 {
		max = 2000
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[risultato troncato per risparmiare contesto]"
}

// ToolSupportMessage returns a user-facing explanation when a model does not
// support native tool calling.
func ToolSupportMessage(modelRef string) string {
	return "Model \"" + modelRef + "\" does not appear to support native tool/function calling. " +
		"Agentic features (web search, skills, MCP, coding tools) require a tool-capable model " +
		"such as Llama 3.1+, Qwen2.5/3, Mistral, Hermes, Command-R, or a cloud model (GPT-4o, Claude, Gemini). " +
		"Select a compatible model, or disable tools for this chat."
}
