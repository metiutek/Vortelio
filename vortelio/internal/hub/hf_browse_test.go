package hub

import "testing"

func TestQuantOf(t *testing.T) {
	cases := map[string]string{
		"Qwen3.5-4B-Q4_K_M.gguf":                       "Q4_K_M",
		"Qwen3.5-0.8B-UD-IQ2_XXS.gguf":                 "UD-IQ2_XXS",
		"Q4_K_M/Qwen3-235B-Q4_K_M-00001-of-00003.gguf": "Q4_K_M",
		"gemma-4-E2B-it-BF16.gguf":                     "BF16",
		"gpt-oss-20b-MXFP4.gguf":                       "MXFP4",
		"model.gguf":                                   "",
	}
	for in, want := range cases {
		if got := QuantOf(in); got != want {
			t.Errorf("QuantOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripFrontMatter(t *testing.T) {
	in := "---\nlicense: apache-2.0\ntags:\n- gguf\n---\n\n# Model\nText"
	if got := stripFrontMatter(in); got != "# Model\nText" {
		t.Errorf("got %q", got)
	}
	if got := stripFrontMatter("# Plain"); got != "# Plain" {
		t.Errorf("got %q", got)
	}
}
