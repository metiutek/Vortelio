package runtime

import (
	"os"
	"strings"
	"sync"

	"github.com/vortelio/vortelio/internal/gguf"
	"github.com/vortelio/vortelio/internal/hub"
)

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

// ModelSupportsTools reports whether the model (a ref like "llm/qwen3.5:4b" or
// "qwen3.5:4b") can call tools. A local GGUF answers from its own chat
// template — the one llama.cpp runs with --jinja — so any model whose template
// handles tools counts, whatever its name; the family list is the fallback
// for other formats, and cloud models always can.
func ModelSupportsTools(modelRef string) bool {
	n := strings.ToLower(modelRef)
	if strings.HasPrefix(n, "cloud") {
		return true
	}
	if ok, known := templateSupportsTools(modelRef); known {
		return ok
	}
	for _, fam := range toolCapableFamilies {
		if strings.Contains(n, fam) {
			return true
		}
	}
	return false
}

var toolTemplateCache = struct {
	sync.Mutex
	m map[string]bool // GGUF path + mtime → template handles tools
}{m: map[string]bool{}}

// templateSupportsTools reads tokenizer.chat_template from the model's GGUF.
// known is false when the model is not a local GGUF with a template.
func templateSupportsTools(modelRef string) (ok, known bool) {
	ref := modelRef
	if !strings.Contains(ref, "/") {
		ref = "llm/" + ref
	}
	r, err := hub.ParseModelRef(ref)
	if err != nil {
		return false, false
	}
	m, err := hub.NewModelStore().Resolve(r)
	if err != nil || !strings.HasSuffix(strings.ToLower(m.LocalPath), ".gguf") {
		return false, false
	}
	st, err := os.Stat(m.LocalPath)
	if err != nil {
		return false, false
	}
	key := m.LocalPath + "|" + st.ModTime().String()
	toolTemplateCache.Lock()
	v, hit := toolTemplateCache.m[key]
	toolTemplateCache.Unlock()
	if hit {
		return v, true
	}
	g, err := gguf.Read(m.LocalPath, 0)
	if err != nil {
		return false, false
	}
	t := g.String("tokenizer.chat_template")
	if t == "" {
		return false, false
	}
	v = templateHandlesTools(t)
	toolTemplateCache.Lock()
	toolTemplateCache.m[key] = v
	toolTemplateCache.Unlock()
	return v, true
}

// templateHandlesTools spots the tool section of a Jinja chat template.
func templateHandlesTools(t string) bool {
	for _, s := range []string{"tools", "tool_call", "tool_calls", "<|tool", "function_call", "available_tools"} {
		if strings.Contains(t, s) {
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
