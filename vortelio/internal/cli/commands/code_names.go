package commands

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Readable model names, the same rules as the GUI picker:
// "liquidai--lfm2-5-8b-a1b-gguf" + "q8-0" → "LFM2.5 8B A1B", "Q8_0".

var (
	modelBrands = map[string]string{"lfm": "LFM", "glm": "GLM", "gpt": "GPT", "oss": "OSS", "qwen": "Qwen", "llama": "Llama",
		"gemma": "Gemma", "phi": "Phi", "mistral": "Mistral", "deepseek": "DeepSeek", "smollm": "SmolLM", "granite": "Granite",
		"olmo": "OLMo", "moe": "MoE", "it": "IT", "vl": "VL", "coder": "Coder", "instruct": "Instruct"}
	quantTagRe   = regexp.MustCompile(`(?i)^(i?q\d|f16|bf16|f32|mxfp4)`)
	sizeWordRe   = regexp.MustCompile(`^a?\d+(\.\d+)?[bm]$`)
	dashVerRe    = regexp.MustCompile(`(\D)(\d)-(\d)(-|$)`)
	brandPrefRe  = regexp.MustCompile(`^([a-z]+)(.*)$`)
	sizeInTagRe  = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)([bm])\b`)
	modelWordsRe = regexp.MustCompile(`[-_ ]+`)
)

// prettyModel returns a display name and, when the tag is a quantization,
// the quantization ("" otherwise).
func prettyModel(name, tag string) (label, quant string) {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "--"); i > 0 {
		base = base[i+2:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, "-gguf"), "_gguf"), ".gguf")
	base = dashVerRe.ReplaceAllString(base, "$1$2.$3$4")
	var words []string
	for _, w := range modelWordsRe.Split(base, -1) {
		if w == "" {
			continue
		}
		k := strings.ToLower(w)
		switch m := brandPrefRe.FindStringSubmatch(k); {
		case modelBrands[k] != "":
			w = modelBrands[k]
		case sizeWordRe.MatchString(k):
			w = strings.ToUpper(k)
		case m != nil && modelBrands[m[1]] != "":
			w = modelBrands[m[1]] + m[2]
		default:
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		words = append(words, w)
	}
	label = strings.Join(words, " ")
	switch {
	case tag == "" || tag == "latest":
	case quantTagRe.MatchString(tag):
		quant = strings.ToUpper(strings.ReplaceAll(tag, "-", "_"))
	default:
		label += " " + sizeInTagRe.ReplaceAllStringFunc(tag, strings.ToUpper)
	}
	return label, quant
}

// homePath shortens a path under the home folder to ~/…
func homePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~" + string(filepath.Separator) + rel
		}
	}
	return p
}

// fitLeft keeps the end of s within n characters ("…" + tail).
func fitLeft(s string, n int) string {
	r := []rune(s)
	if n < 2 || len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}
