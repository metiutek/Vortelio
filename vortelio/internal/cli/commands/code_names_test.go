package commands

import "testing"

func TestPrettyModel(t *testing.T) {
	cases := []struct{ name, tag, label, quant string }{
		{"liquidai--lfm2-5-8b-a1b-gguf", "q8-0", "LFM2.5 8B A1B", "Q8_0"},
		{"qwen3.5", "4b", "Qwen3.5 4B", ""},
		{"paolonesborracchione--emma-6-gguf", "q8-0", "Emma 6", "Q8_0"},
		{"gpt-oss", "20b", "GPT OSS 20B", ""},
		{"gemma4", "latest", "Gemma4", ""},
		{"unsloth--qwen3-5-0-8b-gguf", "ud-iq2-xxs", "Qwen3.5 0.8B", "UD_IQ2_XXS"},
		{"unsloth--qwen3-coder-30b-a3b-instruct-gguf", "q4-k-m", "Qwen3 Coder 30B A3B Instruct", "Q4_K_M"},
	}
	for _, c := range cases {
		l, q := prettyModel(c.name, c.tag)
		if l != c.label || q != c.quant {
			t.Errorf("prettyModel(%q, %q) = %q, %q; want %q, %q", c.name, c.tag, l, q, c.label, c.quant)
		}
	}
}
