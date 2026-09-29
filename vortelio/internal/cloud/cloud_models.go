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
	// Ollama Cloud: the live catalog is fetched by Choices(); this is the
	// offline fallback (2026-09), cheapest first. Free accounts get starter
	// credits usable on the cheaper models; buying credits unlocks the rest.
	"ollamacloud": {
		{"gpt-oss:20b", "gpt-oss 20B"},
		{"nemotron-3-nano:30b", "Nemotron 3 Nano 30B"},
		{"nemotron-3-super", "Nemotron 3 Super"},
		{"gemma4:31b", "Gemma 4 31B"},
		{"gpt-oss:120b", "gpt-oss 120B"},
		{"glm-5.3-flash", "GLM 5.3 Flash"},
		{"deepseek-v4.1-flash", "DeepSeek V4.1 Flash"},
		{"minimax-m2.7", "MiniMax M2.7"},
		{"mistral-large-3:675b", "Mistral Large 3 675B"},
		{"minimax-m3", "MiniMax M3"},
		{"kimi-k2.6", "Kimi K2.6"},
		{"kimi-k2.7-code", "Kimi K2.7 Code"},
		{"deepseek-v4-pro:0813", "DeepSeek V4 Pro"},
		{"glm-5.3", "GLM 5.3"},
		{"glm-5.2", "GLM 5.2"},
		{"nemotron-3-ultra", "Nemotron 3 Ultra"},
		{"kimi-k3", "Kimi K3"},
	},
}

// freeChoices is what the `vortelio code` picker shows: only models usable on
// each provider's free plan (checked 2026-09 against the providers' pricing and
// rate-limit pages). Providers without a free plan (OpenAI, Anthropic, xAI,
// DeepSeek, Perplexity, Together) list nothing; any model, on any provider, can
// still be added as a custom one.
var freeChoices = map[string][][2]string{
	"gemini": {
		{"gemini-3.8-flash", "Gemini 3.8 Flash"},
		{"gemini-3.5-flash", "Gemini 3.5 Flash"},
		{"gemini-2.5-pro", "Gemini 2.5 Pro"},
		{"gemini-2.5-flash", "Gemini 2.5 Flash"},
		{"gemini-2.5-flash-lite", "Gemini 2.5 Flash-Lite"},
	},
	"groq": {
		{"openai/gpt-oss-120b", "gpt-oss 120B"},
		{"openai/gpt-oss-20b", "gpt-oss 20B"},
		{"qwen/qwen3.8-27b", "Qwen3.8 27B"},
	},
	// Mistral's free Experiment plan covers every model; these are the main ones.
	"mistral": {
		{"mistral-large-latest", "Mistral Large"},
		{"mistral-medium-latest", "Mistral Medium"},
		{"mistral-small-latest", "Mistral Small"},
		{"codestral-latest", "Codestral"},
	},
	// OpenRouter's free models rotate: entries that disappeared from the live
	// catalog are dropped (see FeaturedChoices).
	"openrouter": {
		{"openrouter/free", "Auto (best free model)"},
		{"qwen/qwen3.8-27b:free", "Qwen3.8 27B (free)"},
		{"nvidia/nemotron-3-super-120b-a12b:free", "Nemotron 3 Super 120B (free)"},
		{"google/gemma-4-31b-it:free", "Gemma 4 31B (free)"},
		{"poolside/laguna-s-2.1:free", "Laguna S 2.1 (free)"},
		{"cohere/north-mini-code:free", "North Mini Code (free)"},
	},
	// Usable with a free account's starter credits.
	"ollamacloud": {
		{"gpt-oss:20b", "gpt-oss 20B"},
		{"gpt-oss:120b", "gpt-oss 120B"},
		{"nemotron-3-nano:30b", "Nemotron 3 Nano 30B"},
		{"gemma4:31b", "Gemma 4 31B"},
		{"glm-5.3-flash", "GLM 5.3 Flash"},
	},
}

// FeaturedChoices is the compact picker list for the CLI: the provider's
// free-plan models, or nil when it has no free plan.
func FeaturedChoices(providerID string) [][2]string {
	all := freeChoices[providerID]
	if providerID != "openrouter" {
		return all
	}
	live := openRouterFreeIDs()
	if len(live) == 0 {
		return all // offline: keep the curated list
	}
	var out [][2]string
	for _, c := range all {
		if live[c[0]] {
			out = append(out, c)
		}
	}
	return out
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
		choices := Choices(p.ID)
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
