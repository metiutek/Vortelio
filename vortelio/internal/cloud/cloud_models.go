package cloud

// ModelChoices is the curated model list per provider (model id, display label).
// Users can still send any model string via the API; this is just the picker.
var ModelChoices = map[string][][2]string{
	"openai": {
		{"gpt-4o", "GPT-4o"},
		{"gpt-4o-mini", "GPT-4o mini"},
		{"o3-mini", "o3-mini"},
		{"o1-mini", "o1-mini"},
	},
	"anthropic": {
		{"claude-3-5-sonnet-20241022", "Claude 3.5 Sonnet"},
		{"claude-3-5-haiku-20241022", "Claude 3.5 Haiku"},
		{"claude-3-opus-20240229", "Claude 3 Opus"},
	},
	"gemini": {
		{"gemini-2.0-flash", "Gemini 2.0 Flash"},
		{"gemini-2.5-flash", "Gemini 2.5 Flash"},
		{"gemini-2.5-pro", "Gemini 2.5 Pro"},
	},
	"groq": {
		{"llama-3.3-70b-versatile", "Llama 3.3 70B"},
		{"llama3-8b-8192", "Llama 3 8B"},
	},
	"mistral": {
		{"mistral-small-latest", "Mistral Small"},
		{"mistral-large-latest", "Mistral Large"},
	},
	"openrouter": {
		{"meta-llama/llama-3.1-8b-instruct:free", "Llama 3.1 8B (free)"},
		{"anthropic/claude-3.5-sonnet", "Claude 3.5 Sonnet"},
		{"openai/gpt-4o", "GPT-4o"},
		{"deepseek/deepseek-r1", "DeepSeek R1"},
	},
	"xai": {
		{"grok-2-latest", "Grok 2"},
		{"grok-2-vision-latest", "Grok 2 Vision"},
		{"grok-beta", "Grok Beta"},
	},
	"together": {
		{"meta-llama/Llama-3.3-70B-Instruct-Turbo", "Llama 3.3 70B Turbo"},
		{"Qwen/Qwen2.5-72B-Instruct-Turbo", "Qwen2.5 72B Turbo"},
		{"mistralai/Mixtral-8x7B-Instruct-v0.1", "Mixtral 8x7B"},
	},
	"deepseek": {
		{"deepseek-chat", "DeepSeek V3 (chat)"},
		{"deepseek-reasoner", "DeepSeek R1 (reasoner)"},
	},
	"perplexity": {
		{"sonar", "Sonar"},
		{"sonar-pro", "Sonar Pro"},
		{"sonar-reasoning", "Sonar Reasoning"},
	},
	"ollamacloud": {
		{"gpt-oss:120b", "gpt-oss 120B"},
		{"deepseek-v3.1:671b", "DeepSeek V3.1 671B"},
		{"qwen3-coder:480b", "Qwen3 Coder 480B"},
		{"kimi-k2:1t", "Kimi K2 1T"},
	},
}

// CloudModel is a ready-to-use cloud model (provider + model id + label).
type CloudModel struct {
	Provider     string
	ProviderName string
	Model        string
	Label        string
}

// ModelsWithKeys returns the cloud models the user can actually use — every
// provider that has at least one saved API key. Used by the unified /v1 gateway
// and the Open Code config generator.
func ModelsWithKeys() []CloudModel {
	var out []CloudModel
	for _, p := range Providers {
		if LoadKey(p.ID) == "" {
			continue
		}
		choices := ModelChoices[p.ID]
		if len(choices) == 0 {
			choices = [][2]string{{p.DefaultModel, p.DefaultModel}}
		}
		for _, c := range choices {
			out = append(out, CloudModel{Provider: p.ID, ProviderName: p.Name, Model: c[0], Label: c[1]})
		}
	}
	return out
}

// ChatCompletionsURL returns a provider's OpenAI-compatible /chat/completions
// endpoint, suitable for a transparent proxy that forwards a full OpenAI request
// (messages, tools, stream). Anthropic and Gemini expose dedicated
// OpenAI-compatible endpoints; the rest already point there.
func ChatCompletionsURL(p Provider) string {
	switch p.ID {
	case "anthropic":
		return "https://api.anthropic.com/v1/chat/completions"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	default:
		return p.BaseURL
	}
}
