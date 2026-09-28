package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/config"
)

// ─── MODEL REFERENCE ─────────────────────────────────────────────────────────

var validTypes = map[string]bool{
	"llm": true, "image": true, "audio": true, "video": true, "3d": true,
}

// HFDirectRef holds info for a direct HuggingFace pull like:
//
//	llm/hf.co/unsloth/Qwen3.5-0.8B-GGUF:UD-IQ2_XXS
type HFDirectRef struct {
	ModelType string
	Owner     string
	Repo      string
	FileHint  string // prefix/pattern to pick file, e.g. "UD-IQ2_XXS"
}

// ModelRef is a parsed reference like "llm/mistral:7b"
type ModelRef struct {
	Type     string       // llm | image | audio | video
	Name     string       // normalized name for local storage
	Tag      string       // e.g. 7b, large, latest, UD-IQ2_XXS
	HFDirect *HFDirectRef // non-nil when parsed from hf.co URL
}

func (r *ModelRef) String() string {
	return fmt.Sprintf("%s/%s:%s", r.Type, r.Name, r.Tag)
}

// ParseModelRef parses both:
//
//	llm/mistral:7b
//	llm/hf.co/unsloth/Qwen3.5-0.8B-GGUF:UD-IQ2_XXS
func ParseModelRef(raw string) (*ModelRef, error) {
	raw = strings.TrimPrefix(raw, "pullai/")

	// ── Normalize full HuggingFace URLs pasted by user ───────────────────────
	// e.g. image/https://huggingface.co/calcuis/illustrious?show_file_info=file.gguf
	// e.g. image/huggingface.co/calcuis/illustrious:file.gguf
	for _, prefix := range []string{"https://huggingface.co/", "http://huggingface.co/", "huggingface.co/"} {
		if idx := strings.Index(raw, prefix); idx >= 0 {
			// Extract modelType before the URL
			modeType := ""
			if idx > 0 {
				modeType = raw[:idx-1] // strip trailing /
			}
			rest := raw[idx+len(prefix):]
			// Strip HF navigation suffixes, capturing filename from /blob/main/<file> and /resolve/main/<file>
			var pathHint string
			for _, sfx := range []string{"/blob/main/", "/resolve/main/"} {
				if si := strings.Index(rest, sfx); si >= 0 {
					after := rest[si+len(sfx):]
					if after != "" {
						pathHint = after
					}
					rest = rest[:si]
					break
				}
			}
			for _, sfx := range []string{"/tree/main", "/blob/main", "/resolve/main", "/tree/", "/blob/"} {
				if si := strings.Index(rest, sfx); si >= 0 {
					rest = rest[:si]
					break
				}
			}
			// rest = "owner/repo?show_file_info=file.gguf" or "owner/repo:file.gguf"
			var owner, repo, hint string
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) == 2 {
				owner = parts[0]
				repoFull := parts[1]
				// Handle ?query=file.gguf
				if qi := strings.Index(repoFull, "?"); qi >= 0 {
					query := repoFull[qi+1:]
					repo = repoFull[:qi]
					for _, part := range strings.Split(query, "&") {
						if eqIdx := strings.Index(part, "="); eqIdx >= 0 {
							hint = part[eqIdx+1:]
						} else if part != "" {
							hint = part
						}
						if hint != "" {
							break
						}
					}
				} else {
					repo, hint, _ = strings.Cut(repoFull, ":")
				}
			} else {
				owner = rest
			}
			if hint == "" {
				hint = pathHint
			}
			if modeType == "" {
				modeType = "llm" // default
			}
			if !validTypes[modeType] {
				return nil, fmt.Errorf("unknown type %q (valid: llm, image, audio, video, 3d)", modeType)
			}
			localName := strings.ToLower(owner + "__" + repo)
			localName = strings.NewReplacer(" ", "-", ".", "-", "_", "-").Replace(localName)
			tag := strings.NewReplacer(" ", "-", ".", "-", "_", "-").Replace(strings.ToLower(hint))
			if tag == "" {
				tag = "latest"
			}
			return &ModelRef{
				Type: modeType, Name: localName, Tag: tag,
				HFDirect: &HFDirectRef{ModelType: modeType, Owner: owner, Repo: repo, FileHint: hint},
			}, nil
		}
	}

	// ── Direct HuggingFace URL: type/hf.co/owner/repo[:file] ────────────────
	// Detect by checking if second segment is "hf.co"
	segments := strings.SplitN(raw, "/", 4)
	if len(segments) >= 3 && segments[1] == "hf.co" {
		modelType := segments[0]
		if !validTypes[modelType] {
			return nil, fmt.Errorf("unknown type %q (valid: llm, image, audio, video, 3d)", modelType)
		}
		if len(segments) < 4 {
			return nil, fmt.Errorf("incomplete direct HF format.\nExample: llm/hf.co/unsloth/Qwen3.5-0.8B-GGUF:UD-IQ2_XXS")
		}
		owner := segments[2]
		repoAndFile := segments[3]

		// Strip full HuggingFace URL prefix if user pasted the full URL
		// e.g. https://huggingface.co/calcuis/illustrious?show_file_info=fast-q2_k.gguf
		// → strip everything before the last / and treat ?key=value as file hint
		if idx := strings.Index(repoAndFile, "?"); idx >= 0 {
			// ?show_file_info=filename.gguf  or  ?filename=...  → treat value as hint
			query := repoAndFile[idx+1:]
			repoAndFile = repoAndFile[:idx]
			// Extract value from key=value pairs as file hint
			for _, part := range strings.Split(query, "&") {
				if eqIdx := strings.Index(part, "="); eqIdx >= 0 {
					val := part[eqIdx+1:]
					if val != "" && repoAndFile != "" {
						repoAndFile = repoAndFile + ":" + val
						break
					}
				} else if part != "" {
					repoAndFile = repoAndFile + ":" + part
					break
				}
			}
		}

		repo, fileHint, _ := strings.Cut(repoAndFile, ":")

		// Normalized local name: owner__repo (lowercase, safe for filesystem)
		localName := strings.ToLower(owner + "__" + repo)
		localName = strings.NewReplacer(" ", "-", ".", "-", "_", "-").Replace(localName)
		tag := strings.NewReplacer(" ", "-", ".", "-", "_", "-").Replace(strings.ToLower(fileHint))
		if tag == "" {
			tag = "latest"
		}

		return &ModelRef{
			Type: modelType,
			Name: localName,
			Tag:  tag,
			HFDirect: &HFDirectRef{
				ModelType: modelType,
				Owner:     owner,
				Repo:      repo,
				FileHint:  fileHint,
			},
		}, nil
	}

	// ── Standard format: type/name[:tag] ────────────────────────────────────
	slashIdx := strings.Index(raw, "/")
	if slashIdx < 0 {
		return nil, fmt.Errorf("missing type prefix.\nExample: llm/mistral:7b  or  llm/hf.co/owner/repo:file")
	}
	modelType := raw[:slashIdx]
	rest := raw[slashIdx+1:]
	if !validTypes[modelType] {
		return nil, fmt.Errorf("unknown type %q (valid: llm, image, audio, video, 3d)", modelType)
	}
	// Detect if rest looks like a filename (contains a model extension)
	// e.g. "image/stable-diffusion-v1-5-pruned-emaonly-Q5_0.gguf" — treat name as the model name and extension as tag
	for _, ext := range []string{".gguf", ".safetensors", ".ckpt", ".pt", ".bin"} {
		if strings.HasSuffix(strings.ToLower(rest), ext) {
			return nil, fmt.Errorf("the name %q looks like a local file.\nUse: vortelio pull --file <path> %s\nOr: vortelio pull %s/hf.co/<owner>/<repo>:<file>", rest, modelType, modelType)
		}
	}
	name, tag, _ := strings.Cut(rest, ":")
	if tag == "" {
		tag = "latest"
	}
	if name == "" {
		return nil, fmt.Errorf("missing model name")
	}
	return &ModelRef{Type: modelType, Name: name, Tag: tag}, nil
}

// ─── MODEL METADATA ──────────────────────────────────────────────────────────

type Model struct {
	Type         string    `json:"type"`
	Name         string    `json:"name"`
	Tag          string    `json:"tag"`
	Format       string    `json:"format"`
	SizeBytes    int64     `json:"size_bytes"`
	LocalPath    string    `json:"local_path"`
	Source       string    `json:"source"`
	Parameters   string    `json:"parameters"`
	License      string    `json:"license"`
	Capabilities []string  `json:"capabilities"`
	DownloadedAt time.Time `json:"downloaded_at"`
	DisplayName  string    `json:"display_name,omitempty"`
	// Chat template info (used by LLM runner for correct stop tokens)
	ChatTemplate    string            `json:"chat_template,omitempty"`
	StopTokens      []string          `json:"stop_tokens,omitempty"`
	SystemOverride  string            `json:"system_override,omitempty"`  // custom system prompt from Modelfile
	Modelfile       string            `json:"modelfile,omitempty"`        // raw Modelfile source
	Template        string            `json:"template,omitempty"`         // Modelfile TEMPLATE (Go template)
	MmProjPath      string            `json:"mmproj_path,omitempty"`      // path to multimodal projector (LLaVA)
	NumGPULayers    int               `json:"num_gpu_layers,omitempty"`   // override auto-detected GPU layers
	ModelParameters map[string]string `json:"model_parameters,omitempty"` // PARAMETER values from Modelfile
}

func (m *Model) SizeHuman() string {
	gb := float64(m.SizeBytes) / 1e9
	if gb >= 1 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	mb := float64(m.SizeBytes) / 1e6
	return fmt.Sprintf("%.0f MB", mb)
}

// ─── MODEL STORE ─────────────────────────────────────────────────────────────

type ModelStore struct {
	baseDir string
}

func NewModelStore() *ModelStore {
	return &ModelStore{baseDir: filepath.Join(config.HomeDir(), "models")}
}

func (s *ModelStore) manifestPath(ref *ModelRef) string {
	return filepath.Join(s.baseDir, ref.Type, ref.Name, ref.Tag, "manifest.json")
}

func (s *ModelStore) List() ([]*Model, error) {
	var models []*Model
	for _, t := range []string{"llm", "image", "audio", "video", "3d"} {
		typeDir := filepath.Join(s.baseDir, t)
		entries, err := os.ReadDir(typeDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			tags, _ := os.ReadDir(filepath.Join(typeDir, entry.Name()))
			for _, tag := range tags {
				mPath := filepath.Join(typeDir, entry.Name(), tag.Name(), "manifest.json")
				if m, err := s.loadManifest(mPath); err == nil {
					models = append(models, m)
				}
			}
		}
	}
	return models, nil
}

func (s *ModelStore) Resolve(ref *ModelRef) (*Model, error) {
	m, err := s.loadManifest(s.manifestPath(ref))
	if err != nil {
		return nil, fmt.Errorf("model %q not found locally", ref.String())
	}
	return m, nil
}

func (s *ModelStore) Remove(ref *ModelRef) error {
	return os.RemoveAll(filepath.Dir(s.manifestPath(ref)))
}

func (s *ModelStore) Rename(ref *ModelRef, displayName string) error {
	m, err := s.loadManifest(s.manifestPath(ref))
	if err != nil {
		return fmt.Errorf("model not found: %w", err)
	}
	m.DisplayName = strings.TrimSpace(displayName)
	return s.Save(m)
}

func (s *ModelStore) Save(m *Model) error {
	ref := &ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
	dir := filepath.Dir(s.manifestPath(ref))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.manifestPath(ref), data, 0644)
}

func (s *ModelStore) loadManifest(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Model
	return &m, json.Unmarshal(data, &m)
}

// ─── HF API ──────────────────────────────────────────────────────────────────

type HFFileInfo = hfFileInfo
type hfFileInfo struct {
	Rfilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

type hfModelInfo struct {
	ID    string       `json:"id"`
	Files []hfFileInfo `json:"siblings"`
}

// fetchHFModelInfo calls HuggingFace API to list files in a repo.
func fetchHFModelInfo(owner, repo string) (*hfModelInfo, error) {
	return fetchHFModelInfoCtx(context.Background(), owner, repo)
}

func fetchHFModelInfoCtx(ctx context.Context, owner, repo string) (*hfModelInfo, error) {
	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s/%s", owner, repo)
	ctx30, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx30, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Vortelio/1.0)")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cancelled")
		}
		return nil, fmt.Errorf("HuggingFace API unreachable: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		// ok
	case 401, 403:
		return nil, fmt.Errorf("repository %s/%s requires authentication.\nSign in at https://huggingface.co and accept the model terms", owner, repo)
	case 404:
		return nil, fmt.Errorf("repository %s/%s not found on HuggingFace.\nCheck the URL and try again", owner, repo)
	default:
		return nil, fmt.Errorf("HuggingFace API: %s", resp.Status)
	}
	var info hfModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("invalid HF response: %w", err)
	}
	if len(info.Files) == 0 {
		return nil, fmt.Errorf("repository %s/%s contains no files.\nIt may be empty or require authentication", owner, repo)
	}
	return &info, nil
}

// pickFile selects the best matching file from a HF repo.
// Priority: .gguf > .safetensors > .pt/.bin/.ckpt > any file matching hint.
func pickFile(files []hfFileInfo, fileHint string) (hfFileInfo, error) {
	hint := strings.ToLower(fileHint)

	// If a hint is given, try to match by name first across ALL file types
	if hint != "" {
		for _, f := range files {
			if strings.Contains(strings.ToLower(f.Rfilename), hint) {
				return f, nil
			}
		}
	}

	// Priority order for model weights
	priority := []string{".gguf", ".safetensors", ".pth", ".pt", ".bin", ".ckpt"}
	var chosen *hfFileInfo
	var weightNames []string
	for _, ext := range priority {
		var matched []hfFileInfo
		for _, f := range files {
			name := strings.ToLower(f.Rfilename)
			if strings.HasSuffix(name, ext) {
				// Skip shard files like model-00001-of-00008 if possible
				if strings.Contains(name, "-of-") && len(matched) > 0 {
					continue
				}
				matched = append(matched, f)
			}
		}
		for _, f := range matched {
			weightNames = append(weightNames, "  "+f.Rfilename)
		}
		if chosen == nil && len(matched) > 0 {
			best := matched[0]
			// Pick smallest (most quantized) for gguf, else first
			if ext == ".gguf" {
				for _, f := range matched[1:] {
					if f.Size < best.Size && f.Size > 0 {
						best = f
					}
				}
			}
			chosen = &best
		}
	}

	if chosen == nil {
		return hfFileInfo{}, fmt.Errorf("no model file found in the repository")
	}
	if hint != "" {
		// An explicit hint that matched nothing must not silently download a
		// different file — report what is actually available instead.
		return hfFileInfo{}, fmt.Errorf(
			"no file matching pattern %q found in the repository.\nAvailable files:\n%s",
			fileHint, strings.Join(weightNames, "\n"),
		)
	}
	return *chosen, nil
}

// ─── DOWNLOADER ──────────────────────────────────────────────────────────────

var hfRegistry = map[string]HFEntry{
	// ── LLM — GGUF ─────────────────────────────────────────────────────────
	"llm/mistral:7b": {
		Repo:   "TheBloke/Mistral-7B-Instruct-v0.2-GGUF",
		File:   "mistral-7b-instruct-v0.2.Q4_K_M.gguf",
		Format: "gguf", Params: "7B", License: "Apache-2.0",
		ChatTemplate: "mistral", StopTokens: []string{"[INST]", "[/INST]"},
	},
	"llm/llama3:8b": {
		Repo:   "bartowski/Meta-Llama-3.1-8B-Instruct-GGUF",
		File:   "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "8B", License: "Meta Llama 3",
		ChatTemplate: "llama3", StopTokens: []string{"<|eot_id|>", "<|end_of_text|>"},
	},
	"llm/phi3:mini": {
		Repo:   "bartowski/Phi-3.5-mini-instruct-GGUF",
		File:   "Phi-3.5-mini-instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "3.8B", License: "MIT",
		ChatTemplate: "phi3", StopTokens: []string{"<|end|>", "<|endoftext|>"},
	},
	"llm/qwen:0.5b": {
		Repo:   "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		File:   "qwen2.5-0.5b-instruct-q4_k_m.gguf",
		Format: "gguf", Params: "0.5B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen:1.5b": {
		Repo:   "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
		File:   "qwen2.5-1.5b-instruct-q4_k_m.gguf",
		Format: "gguf", Params: "1.5B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/gemma:2b": {
		Repo:   "bartowski/gemma-2-2b-it-GGUF",
		File:   "gemma-2-2b-it-Q4_K_M.gguf",
		Format: "gguf", Params: "2B", License: "Gemma",
		ChatTemplate: "gemma", StopTokens: []string{"<end_of_turn>"},
	},
	"llm/llama3.3:70b": {
		Repo:   "bartowski/Meta-Llama-3.3-70B-Instruct-GGUF",
		File:   "Meta-Llama-3.3-70B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "70B", License: "Meta",
		ChatTemplate: "llama3", StopTokens: []string{"<|eot_id|>", "<|end_of_text|>"},
	},
	"llm/llama3.2:3b": {
		Repo:   "bartowski/Llama-3.2-3B-Instruct-GGUF",
		File:   "Llama-3.2-3B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "3B", License: "Meta",
		ChatTemplate: "llama3", StopTokens: []string{"<|eot_id|>", "<|end_of_text|>"},
	},
	"llm/llama3.1:8b": {
		Repo:   "bartowski/Meta-Llama-3.1-8B-Instruct-GGUF",
		File:   "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "8B", License: "Meta",
		ChatTemplate: "llama3", StopTokens: []string{"<|eot_id|>", "<|end_of_text|>"},
	},
	"llm/qwen2.5:0.5b": {
		Repo:   "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		File:   "qwen2.5-0.5b-instruct-q4_k_m.gguf",
		Format: "gguf", Params: "0.5B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen2.5:7b": {
		// Official Qwen GGUF repo ships this size as split shards (single-file 404s);
		// bartowski provides a verified single-file Q4_K_M.
		Repo:   "bartowski/Qwen2.5-7B-Instruct-GGUF",
		File:   "Qwen2.5-7B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "7B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen2.5:14b": {
		Repo:   "bartowski/Qwen2.5-14B-Instruct-GGUF",
		File:   "Qwen2.5-14B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "14B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen2.5-coder:7b": {
		Repo:   "bartowski/Qwen2.5-Coder-7B-Instruct-GGUF",
		File:   "Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "7B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/phi4:latest": {
		Repo:   "bartowski/phi-4-GGUF",
		File:   "phi-4-Q4_K_M.gguf",
		Format: "gguf", Params: "14B", License: "MIT",
		ChatTemplate: "phi3", StopTokens: []string{"<|end|>", "<|endoftext|>"},
	},
	"llm/phi3.5:mini": {
		Repo:   "bartowski/Phi-3.5-mini-instruct-GGUF",
		File:   "Phi-3.5-mini-instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "3.8B", License: "MIT",
		ChatTemplate: "phi3", StopTokens: []string{"<|end|>", "<|endoftext|>"},
	},
	"llm/mistral-nemo:12b": {
		Repo:   "bartowski/Mistral-Nemo-Instruct-2407-GGUF",
		File:   "Mistral-Nemo-Instruct-2407-Q4_K_M.gguf",
		Format: "gguf", Params: "12B", License: "Apache-2.0",
		ChatTemplate: "mistral", StopTokens: []string{"[INST]", "[/INST]"},
	},
	"llm/gemma3:4b": {
		// bartowski's Gemma 3 GGUFs are gated (401); unsloth mirrors them ungated.
		Repo:   "unsloth/gemma-3-4b-it-GGUF",
		File:   "gemma-3-4b-it-Q4_K_M.gguf",
		Format: "gguf", Params: "4B", License: "Gemma",
		ChatTemplate: "gemma", StopTokens: []string{"<end_of_turn>"},
	},
	"llm/gemma3:12b": {
		Repo:   "unsloth/gemma-3-12b-it-GGUF",
		File:   "gemma-3-12b-it-Q4_K_M.gguf",
		Format: "gguf", Params: "12B", License: "Gemma",
		ChatTemplate: "gemma", StopTokens: []string{"<end_of_turn>"},
	},
	"llm/gemma3n:e4b": {
		// Gemma 3n in a llama.cpp-compatible GGUF (unsloth, ungated). Runs natively
		// in Vortelio — unlike the Ollama 'gemma4' blob, which is in Ollama's own
		// engine format that llama.cpp can't read.
		Repo:   "unsloth/gemma-3n-E4B-it-GGUF",
		File:   "gemma-3n-E4B-it-Q4_K_M.gguf",
		Format: "gguf", Params: "4B (E4B)", License: "Gemma",
		ChatTemplate: "gemma", StopTokens: []string{"<end_of_turn>"},
	},
	"llm/deepseek-r1:7b": {
		Repo:   "bartowski/DeepSeek-R1-Distill-Qwen-7B-GGUF",
		File:   "DeepSeek-R1-Distill-Qwen-7B-Q4_K_M.gguf",
		Format: "gguf", Params: "7B", License: "MIT",
		ChatTemplate: "deepseek", StopTokens: []string{"<|EOT|>"},
	},
	"llm/deepseek-r1:14b": {
		Repo:   "bartowski/DeepSeek-R1-Distill-Qwen-14B-GGUF",
		File:   "DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf",
		Format: "gguf", Params: "14B", License: "MIT",
		ChatTemplate: "deepseek", StopTokens: []string{"<|EOT|>"},
	},
	"llm/command-r:35b": {
		Repo:   "bartowski/c4ai-command-r-v01-GGUF",
		File:   "c4ai-command-r-v01-Q4_K_M.gguf",
		Format: "gguf", Params: "35B", License: "CC-BY-NC-4.0",
		ChatTemplate: "command-r", StopTokens: []string{"<|END_OF_TURN_TOKEN|>"},
	},
	// ── LLM — current generation (2026). The chat template comes from the GGUF
	// itself (llama-server --jinja), so no hand-written template is needed. ──
	"llm/qwen3.5:0.8b": {
		Repo:   "unsloth/Qwen3.5-0.8B-GGUF",
		File:   "Qwen3.5-0.8B-Q4_K_M.gguf",
		Format: "gguf", Params: "0.8B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen3.5:4b": {
		Repo:   "unsloth/Qwen3.5-4B-GGUF",
		File:   "Qwen3.5-4B-Q4_K_M.gguf",
		Format: "gguf", Params: "4B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen3.5:9b": {
		Repo:   "unsloth/Qwen3.5-9B-GGUF",
		File:   "Qwen3.5-9B-Q4_K_M.gguf",
		Format: "gguf", Params: "9B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen3.8:27b": {
		Repo:   "unsloth/Qwen3.8-27B-GGUF",
		File:   "Qwen3.8-27B-UD-Q4_K_M.gguf",
		Format: "gguf", Params: "27B", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen3.6:35b-a3b": {
		Repo:   "unsloth/Qwen3.6-35B-A3B-GGUF",
		File:   "Qwen3.6-35B-A3B-UD-Q4_K_M.gguf",
		Format: "gguf", Params: "35B (3B active)", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/qwen3-coder:30b": {
		Repo:   "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF",
		File:   "Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf",
		Format: "gguf", Params: "30B (3B active)", License: "Apache-2.0",
		ChatTemplate: "chatml", StopTokens: []string{"<|im_end|>"},
	},
	"llm/gemma4:e2b": {
		Repo:   "unsloth/gemma-4-E2B-it-GGUF",
		File:   "gemma-4-E2B-it-Q4_K_M.gguf",
		Format: "gguf", Params: "E2B", License: "Apache-2.0",
	},
	"llm/gemma4:e4b": {
		Repo:   "unsloth/gemma-4-E4B-it-GGUF",
		File:   "gemma-4-E4B-it-Q4_K_M.gguf",
		Format: "gguf", Params: "E4B", License: "Apache-2.0",
	},
	"llm/gemma4:12b": {
		Repo:   "unsloth/gemma-4-12b-it-GGUF",
		File:   "gemma-4-12b-it-Q4_K_M.gguf",
		Format: "gguf", Params: "12B", License: "Apache-2.0",
	},
	"llm/gemma4:26b": {
		Repo:   "unsloth/gemma-4-26B-A4B-it-GGUF",
		File:   "gemma-4-26B-A4B-it-UD-Q4_K_M.gguf",
		Format: "gguf", Params: "26B (4B active)", License: "Apache-2.0",
	},
	"llm/gpt-oss:20b": {
		Repo:   "ggml-org/gpt-oss-20b-GGUF",
		File:   "gpt-oss-20b-MXFP4.gguf",
		Format: "gguf", Params: "20B", License: "Apache-2.0",
	},
	// ── Image — GGUF (gpustack repos, public, no token) ───────────────────
	"image/openjourney:latest": {
		Repo:   "gpustack/stable-diffusion-v1-5-GGUF",
		File:   "stable-diffusion-v1-5-Q4_1.gguf",
		Format: "gguf", Params: "860M", License: "CreativeML",
	},
	"image/dreamshaper:latest": {
		Repo:   "gpustack/dreamshaper-8-GGUF",
		File:   "dreamshaper-8-Q5_0.gguf",
		Format: "gguf", Params: "860M", License: "CreativeML",
	},
	"image/sdxl:latest": {
		Repo:   "gpustack/stable-diffusion-xl-base-1.0-GGUF",
		File:   "stable-diffusion-xl-base-1.0-Q4_1.gguf",
		Format: "gguf", Params: "3.5B", License: "CreativeML",
	},
	"image/flux:schnell": {
		Repo:   "city96/FLUX.1-schnell-gguf",
		File:   "flux1-schnell-Q4_K_S.gguf",
		Format: "gguf", Params: "12B", License: "Apache-2.0",
	},
	"image/flux:dev": {
		Repo:   "city96/FLUX.1-dev-gguf",
		File:   "flux1-dev-Q4_K_S.gguf",
		Format: "gguf", Params: "12B", License: "NC",
	},
	"image/realvisxl:v4": {
		Repo:   "gpustack/RealVisXL_V4.0-GGUF",
		File:   "RealVisXL_V4.0-Q5_0.gguf",
		Format: "gguf", Params: "3.5B", License: "CreativeML",
	},
	// ── Audio — Whisper GGUF (ggerganov, widely mirrored) ────────────────
	"audio/whisper:large": {
		Repo:   "ggerganov/whisper.cpp",
		File:   "ggml-large-v3.bin",
		Format: "bin", Params: "1.5B", License: "Apache-2.0",
	},
	"audio/whisper:large-v3": {
		Repo:   "ggerganov/whisper.cpp",
		File:   "ggml-large-v3.bin",
		Format: "bin", Params: "1.5B", License: "Apache-2.0",
	},
	"audio/whisper:base": {
		Repo:   "ggerganov/whisper.cpp",
		File:   "ggml-base.bin",
		Format: "bin", Params: "74M", License: "Apache-2.0",
	},
	"audio/whisper:small": {
		Repo:   "ggerganov/whisper.cpp",
		File:   "ggml-small.bin",
		Format: "bin", Params: "244M", License: "Apache-2.0",
	},
	"audio/kokoro:latest": {
		Repo:   "hexgrad/Kokoro-82M",
		File:   "kokoro-v1_0.pth",
		Format: "pt", Params: "82M", License: "Apache-2.0",
	},
	"audio/bark:latest": {
		Repo:   "suno/bark",
		File:   "pytorch_model.bin",
		Format: "pt", Params: "1.2B", License: "MIT",
	},
	// ── Video ───────────────────────────────────────────────────────────────
	"video/wan:1.3b": {
		Repo:   "Wan-AI/Wan2.1-T2V-1.3B-Diffusers",
		File:   "",
		Format: "diffusers", Params: "1.3B", License: "Apache-2.0",
		FullRepo: true,
	},
	"video/wan:14b": {
		Repo:   "Wan-AI/Wan2.1-T2V-14B-Diffusers",
		File:   "",
		Format: "diffusers", Params: "14B", License: "Apache-2.0",
		FullRepo: true,
	},
	"video/cogvideo:5b": {
		Repo:   "THUDM/CogVideoX-5b",
		File:   "transformer/diffusion_pytorch_model.safetensors",
		Format: "safetensors", Params: "5B", License: "CogVideoX",
	},
	"video/animatediff:v3": {
		Repo:   "guoyww/animatediff-motion-adapter-v1-5-3",
		File:   "diffusion_pytorch_model.safetensors",
		Format: "safetensors", Params: "1.5B", License: "Apache-2.0",
		FullRepo: true, // needs config.json alongside the weights
	},
	// ── 3D ──────────────────────────────────────────────────────────────────
	"3d/triposr:latest": {Repo: "stabilityai/TripoSR", File: "model.ckpt", Format: "ckpt", Params: "1B", License: "MIT"},
	"3d/shap-e:latest":  {Repo: "openai/shap-e", File: "transmitter.pt", Format: "pt", Params: "300M", License: "MIT"},
}

type HFEntry struct {
	Repo         string
	File         string // empty = download full repo
	Format       string
	Params       string
	License      string
	ChatTemplate string
	StopTokens   []string
	FullRepo     bool // true = download entire repo (diffusers/adapters)
}

type ProgressCallback func(downloaded, total int64)

type Downloader struct {
	store  *ModelStore
	client *http.Client
	ctx    context.Context
}

func NewDownloaderWithContext(ctx context.Context) *Downloader {
	return &Downloader{store: NewModelStore(), client: &http.Client{Timeout: 0}, ctx: ctx}
}

func NewDownloader() *Downloader {
	return &Downloader{ctx: context.Background(),
		store:  NewModelStore(),
		client: &http.Client{Timeout: 0},
	}
}

func (d *Downloader) Pull(ref *ModelRef, progress ProgressCallback) error {
	// ── Direct HuggingFace pull ──────────────────────────────────────────────
	if ref.HFDirect != nil {
		return d.pullHFDirect(ref, progress)
	}

	// ── Registry-based pull ──────────────────────────────────────────────────
	key := ref.String()
	entry, ok := hfRegistry[key]
	if !ok {
		key2 := fmt.Sprintf("%s/%s:latest", ref.Type, ref.Name)
		entry, ok = hfRegistry[key2]
	}
	if !ok {
		return fmt.Errorf(
			"model %q not in the Vortelio registry.\n\n"+
				"  You can download any model directly from HuggingFace:\n"+
				"    vortelio pull llm/hf.co/<owner>/<repo>:<file>\n\n"+
				"  Examples:\n"+
				"    vortelio pull llm/hf.co/unsloth/Qwen3.5-0.8B-GGUF:UD-IQ2_XXS\n"+
				"    vortelio pull llm/hf.co/bartowski/Mistral-7B-v0.1-GGUF:Q4_K_M\n\n"+
				"  Or use a registry alias:\n"+
				"    vortelio pull llm/mistral:7b\n"+
				"    vortelio pull llm/llama3:8b\n"+
				"    vortelio pull llm/phi3:mini",
			ref.String(),
		)
	}

	// FullRepo: download entire repo (diffusers pipelines, adapters with config.json, etc.)
	if entry.FullRepo {
		parts := strings.SplitN(entry.Repo, "/", 2)
		owner, repo := parts[0], parts[1]
		// Synthesize an HFDirect ref for this registry entry
		fullRef := &ModelRef{
			Type: ref.Type, Name: ref.Name, Tag: ref.Tag,
			HFDirect: &HFDirectRef{
				ModelType: ref.Type,
				Owner:     owner,
				Repo:      repo,
				FileHint:  entry.File,
			},
		}
		return d.pullHFDirect(fullRef, progress)
	}

	destDir, destFile, err := d.prepareDestDir(ref, filepath.Base(entry.File))
	if err != nil {
		return err
	}
	if d.alreadyDownloaded(destFile) {
		fmt.Println("✓  File already present, updating manifest...")
		return d.saveManifest(ref, destDir, destFile, entry.Format, entry.Params, entry.License,
			fmt.Sprintf("https://huggingface.co/%s", entry.Repo), entry.ChatTemplate, entry.StopTokens)
	}

	url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", entry.Repo, entry.File)
	fmt.Printf("⬇️   Download da HuggingFace: %s\n", entry.Repo)
	if err := d.downloadWithProgress(url, destFile, progress); err != nil {
		return err
	}

	return d.saveManifest(ref, destDir, destFile, entry.Format, entry.Params, entry.License,
		fmt.Sprintf("https://huggingface.co/%s", entry.Repo),
		entry.ChatTemplate, entry.StopTokens)
}

// isDiffusersRepo returns true when the HF repo is a diffusers model directory
// (has a model_index.json at the root).
func isDiffusersRepo(files []hfFileInfo) bool {
	for _, f := range files {
		if f.Rfilename == "model_index.json" {
			return true
		}
	}
	return false
}

// isTransformersRepo returns true when the HF repo is a transformers model
// (has config.json but no model_index.json — typical for LLMs like Qwen, Llama).
func isTransformersRepo(files []hfFileInfo) bool {
	hasConfig := false
	for _, f := range files {
		if f.Rfilename == "config.json" {
			hasConfig = true
		}
		if f.Rfilename == "model_index.json" {
			return false // it's diffusers
		}
	}
	return hasConfig
}

// transformersFilesToDownload returns all files needed to load a transformers model.
// Skips docs, images, and FP32 duplicates when FP16 is available.
func transformersFilesToDownload(files []hfFileInfo) []hfFileInfo {
	// Files to always skip (docs/metadata only)
	skipNames := map[string]bool{
		".gitattributes": true, "README.md": true, "readme.md": true,
		"LICENSE": true, "LICENSE.txt": true, "NOTICE.md": true, "CHANGELOG.md": true,
	}
	// Extensions to skip (media, docs) — NOTE: .txt is NOT skipped globally
	// because merges.txt and vocab.txt are essential tokenizer files
	skipExts := []string{".md", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".html", ".pdf", ".svg"}
	// Directories to skip entirely
	skipDirs := []string{".git/", "docs/", "examples/", "tests/", "eval/", "benchmark/", "demo/"}
	// File patterns to skip (framework-specific formats we don't use)
	skipPatterns := []string{"flax_model", ".msgpack", "tf_model", "rust_model", ".ot", "coreml"}
	// Essential files to ALWAYS keep regardless of extension
	keepNames := map[string]bool{
		"config.json": true, "tokenizer.json": true, "tokenizer_config.json": true,
		"special_tokens_map.json": true, "generation_config.json": true,
		"vocab.json": true, "merges.txt": true, "vocab.txt": true,
		"tokenizer.model": true, "sentencepiece.bpe.model": true,
		"model_index.json": true, "preprocessor_config.json": true,
		"added_tokens.json": true,
	}

	// Prefer fp16 over fp32 if both exist
	hasFP16 := false
	for _, f := range files {
		if strings.Contains(strings.ToLower(f.Rfilename), "fp16") {
			hasFP16 = true
			break
		}
	}

	var keep []hfFileInfo
	for _, f := range files {
		name := f.Rfilename
		lower := strings.ToLower(filepath.Base(name)) // base name for checks
		path := strings.ToLower(name)

		// Always keep essential config/tokenizer files
		if keepNames[filepath.Base(name)] || keepNames[lower] {
			keep = append(keep, f)
			continue
		}

		skip := false
		// Skip by exact name
		if skipNames[name] || skipNames[strings.ToLower(name)] {
			skip = true
		}
		// Skip by extension
		for _, ext := range skipExts {
			if strings.HasSuffix(path, ext) {
				skip = true
				break
			}
		}
		// Skip by directory prefix
		for _, dir := range skipDirs {
			if strings.HasPrefix(path, dir) {
				skip = true
				break
			}
		}
		// Skip by content pattern (TF/Flax weights)
		for _, pat := range skipPatterns {
			if strings.Contains(path, pat) {
				skip = true
				break
			}
		}
		// Skip fp32 if fp16 exists
		if hasFP16 && strings.Contains(path, "fp32") {
			skip = true
		}

		if !skip {
			keep = append(keep, f)
		}
	}
	return keep
}

// diffusersFilesToDownload returns all files needed to run a diffusers model locally.
// It skips .git files, READMEs, and other non-essential files.
func diffusersFilesToDownload(files []hfFileInfo) []hfFileInfo {
	var keep []hfFileInfo
	skip := []string{".gitattributes", "README.md", "LICENSE", ".git"}
	skipExt := []string{".md", ".txt", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".html"}
	for _, f := range files {
		name := f.Rfilename
		lower := strings.ToLower(name)
		// Skip git/doc files
		shouldSkip := false
		for _, s := range skip {
			if name == s || strings.HasPrefix(name, s+"/") {
				shouldSkip = true
				break
			}
		}
		for _, ext := range skipExt {
			if strings.HasSuffix(lower, ext) {
				shouldSkip = true
				break
			}
		}
		// Skip FP32 variants when FP16 exists (saves space)
		if strings.Contains(lower, "fp32") && !strings.Contains(lower, "fp16") {
			// Only skip if not the only option
			shouldSkip = true
		}
		if !shouldSkip {
			keep = append(keep, f)
		}
	}
	return keep
}

// pullHFDirect downloads directly from HF using the API to find the file.
func (d *Downloader) pullHFDirect(ref *ModelRef, progress ProgressCallback) error {
	hfd := ref.HFDirect
	fmt.Printf("🔍  Searching model on HuggingFace: %s/%s\n", hfd.Owner, hfd.Repo)

	info, err := fetchHFModelInfo(hfd.Owner, hfd.Repo)
	if err != nil {
		return err
	}

	// ── Diffusers repo (has model_index.json) → download full folder ──────
	if isDiffusersRepo(info.Files) {
		return d.pullHFDiffusers(ref, hfd, info.Files, progress)
	}

	// ── Transformers repo (has config.json) → download full folder ────────
	if isTransformersRepo(info.Files) {
		return d.pullHFTransformers(ref, hfd, info.Files, progress)
	}

	chosen, err := pickFile(info.Files, hfd.FileHint)
	if err != nil {
		return err
	}

	// Get file size: HF API sometimes returns 0, so fall back to HEAD request
	fileSize := chosen.Size
	if fileSize == 0 {
		url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", hfd.Owner, hfd.Repo, chosen.Rfilename)
		if resp, err := http.Head(url); err == nil {
			fileSize = resp.ContentLength
			resp.Body.Close()
		}
	}
	if fileSize > 0 {
		fmt.Printf("📄  Selected file: %s (%.2f GB)\n", chosen.Rfilename, float64(fileSize)/1e9)
	} else {
		fmt.Printf("📄  Selected file: %s\n", chosen.Rfilename)
	}

	destDir, destFile, err := d.prepareDestDir(ref, filepath.Base(chosen.Rfilename))
	if err != nil {
		return err
	}
	// Detect chat template from repo name (needed for both download and manifest)
	chatTemplate, stopTokens := detectChatTemplate(hfd.Repo)
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(chosen.Rfilename)), ".")
	source := fmt.Sprintf("https://huggingface.co/%s/%s", hfd.Owner, hfd.Repo)

	if d.alreadyDownloaded(destFile) {
		fmt.Println("✓  File already present, updating manifest...")
		// Always save manifest — it may be missing even if file exists
		return d.saveManifest(ref, destDir, destFile, ext, "", "vedi HuggingFace", source, chatTemplate, stopTokens)
	}

	url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", hfd.Owner, hfd.Repo, chosen.Rfilename)

	fmt.Printf("⬇️   Download da: %s/%s\n", hfd.Owner, hfd.Repo)
	if err := d.downloadWithProgress(url, destFile, progress); err != nil {
		return err
	}

	return d.saveManifest(ref, destDir, destFile, ext, "", "vedi HuggingFace", source, chatTemplate, stopTokens)
}

// pullHFDiffusers downloads an entire diffusers model repo (multi-file).
func (d *Downloader) pullHFDiffusers(ref *ModelRef, hfd *HFDirectRef, allFiles []hfFileInfo, progress ProgressCallback) error {
	files := diffusersFilesToDownload(allFiles)

	// Calculate total size
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.Size
	}
	if totalBytes > 0 {
		fmt.Printf("📦  Diffusers repo: %d files, %.2f GB total\n", len(files), float64(totalBytes)/1e9)
	} else {
		fmt.Printf("📦  Diffusers repo: %d files\n", len(files))
	}

	destRoot := filepath.Join(config.HomeDir(), "models", ref.Type, ref.Name, ref.Tag)
	if err := os.MkdirAll(destRoot, 0755); err != nil {
		return err
	}

	source := fmt.Sprintf("https://huggingface.co/%s/%s", hfd.Owner, hfd.Repo)
	var downloadedBytes int64

	for i, f := range files {
		destFile := filepath.Join(destRoot, filepath.FromSlash(f.Rfilename))
		destDir := filepath.Dir(destFile)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return err
		}

		// Skip if already downloaded and non-empty
		if fi, err := os.Stat(destFile); err == nil && fi.Size() > 0 {
			fmt.Printf("  ✓  [%d/%d] %s (already present)\n", i+1, len(files), f.Rfilename)
			downloadedBytes += fi.Size()
			continue
		}

		url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", hfd.Owner, hfd.Repo, f.Rfilename)
		fmt.Printf("  ⬇️   [%d/%d] %s", i+1, len(files), f.Rfilename)
		if f.Size > 0 {
			fmt.Printf(" (%.0f MB)", float64(f.Size)/1e6)
		}
		fmt.Println()

		if err := d.downloadWithProgress(url, destFile, func(dl, total int64) {
			if progress != nil && totalBytes > 0 {
				progress(downloadedBytes+dl, totalBytes)
			}
		}); err != nil {
			return fmt.Errorf("download of %s failed: %w", f.Rfilename, err)
		}
		downloadedBytes += f.Size
	}

	// The "local path" for a diffusers model is the root directory
	// We save model_index.json path as LocalPath so the runner can find the dir
	manifestPath := filepath.Join(destRoot, "model_index.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		// If model_index.json wasn't in the list, use the first .safetensors
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Rfilename), ".safetensors") {
				manifestPath = filepath.Join(destRoot, filepath.FromSlash(f.Rfilename))
				break
			}
		}
	}

	fmt.Printf("\n✅  Model downloaded to: %s\n", destRoot)
	return d.saveManifest(ref, destRoot, manifestPath, "diffusers", "", "vedi HuggingFace", source, "", nil)
}

// pullHFTransformers downloads an entire transformers model repo (multi-file).
// This handles models like Qwen, Llama, Mistral in their original safetensors format.
func (d *Downloader) pullHFTransformers(ref *ModelRef, hfd *HFDirectRef, allFiles []hfFileInfo, progress ProgressCallback) error {
	files := transformersFilesToDownload(allFiles)
	if len(files) == 0 {
		return fmt.Errorf("no files to download in repository %s/%s", hfd.Owner, hfd.Repo)
	}

	var totalBytes int64
	for _, f := range files {
		totalBytes += f.Size
	}

	if totalBytes > 0 {
		fmt.Printf("📦  %s/%s — %d files, %.2f GB total\n", hfd.Owner, hfd.Repo, len(files), float64(totalBytes)/1e9)
	} else {
		fmt.Printf("📦  %s/%s — %d files\n", hfd.Owner, hfd.Repo, len(files))
	}

	destRoot := filepath.Join(config.HomeDir(), "models", ref.Type, ref.Name, ref.Tag)
	if err := os.MkdirAll(destRoot, 0755); err != nil {
		return err
	}

	source := fmt.Sprintf("https://huggingface.co/%s/%s", hfd.Owner, hfd.Repo)
	var downloadedBytes int64

	for i, f := range files {
		if d.ctx != nil {
			select {
			case <-d.ctx.Done():
				return fmt.Errorf("download cancelled")
			default:
			}
		}

		destFile := filepath.Join(destRoot, filepath.FromSlash(f.Rfilename))
		if err := os.MkdirAll(filepath.Dir(destFile), 0755); err != nil {
			return err
		}

		if fi, err := os.Stat(destFile); err == nil && fi.Size() > 0 {
			downloadedBytes += fi.Size()
			if progress != nil && totalBytes > 0 {
				progress(downloadedBytes, totalBytes)
			}
			continue
		}

		shortName := filepath.Base(f.Rfilename)
		fileMsg := fmt.Sprintf("[%d/%d] %s", i+1, len(files), shortName)
		if f.Size > 0 {
			fileMsg += fmt.Sprintf(" (%.0f MB)", float64(f.Size)/1e6)
		}
		fmt.Printf("  ⬇️   %s\n", fileMsg)
		if progress != nil {
			progress(downloadedBytes, max64(totalBytes, 1))
		}

		url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", hfd.Owner, hfd.Repo, f.Rfilename)
		if err := d.downloadWithProgress(url, destFile, func(dl, total int64) {
			if progress != nil {
				eff := totalBytes
				if eff == 0 {
					eff = total
				}
				pct := downloadedBytes + dl
				if eff > 0 && pct > eff {
					pct = eff
				}
				progress(pct, max64(eff, 1))
			}
		}); err != nil {
			return fmt.Errorf("%s: %w", shortName, err)
		}
		downloadedBytes += f.Size
	}

	// Manifest: LocalPath = config.json (or model.safetensors if no config)
	manifestPath := filepath.Join(destRoot, "config.json")
	if _, err := os.Stat(manifestPath); err != nil {
		// Use first safetensors file
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Rfilename), ".safetensors") ||
				strings.HasSuffix(strings.ToLower(f.Rfilename), ".bin") {
				manifestPath = filepath.Join(destRoot, filepath.FromSlash(f.Rfilename))
				break
			}
		}
	}
	chatTemplate, stopTokens := detectChatTemplate(hfd.Repo)
	fmt.Printf("\n✅  Downloaded: %s\n", destRoot)
	return d.saveManifest(ref, destRoot, manifestPath, "safetensors", "", "vedi HuggingFace", source, chatTemplate, stopTokens)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// detectChatTemplate guesses the chat template from the repo/model name.
func detectChatTemplate(repoName string) (string, []string) {
	name := strings.ToLower(repoName)
	switch {
	case strings.Contains(name, "qwen"):
		return "chatml", []string{"<|im_end|>"}
	case strings.Contains(name, "llama-3") || strings.Contains(name, "llama3"):
		return "llama3", []string{"<|eot_id|>", "<|end_of_text|>"}
	case strings.Contains(name, "llama-2") || strings.Contains(name, "llama2"):
		return "llama2", []string{"[/INST]"}
	case strings.Contains(name, "mistral") || strings.Contains(name, "mixtral"):
		return "mistral", []string{"[/INST]", "</s>"}
	case strings.Contains(name, "phi-3") || strings.Contains(name, "phi3"):
		return "phi3", []string{"<|end|>", "<|endoftext|>"}
	case strings.Contains(name, "gemma"):
		return "gemma", []string{"<end_of_turn>"}
	case strings.Contains(name, "deepseek"):
		return "deepseek", []string{"<|EOT|>"}
	case strings.Contains(name, "command-r"):
		return "command-r", []string{"<|END_OF_TURN_TOKEN|>"}
	default:
		return "chatml", []string{"<|im_end|>"}
	}
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func (d *Downloader) prepareDestDir(ref *ModelRef, filename string) (dir, file string, err error) {
	dir = filepath.Join(config.HomeDir(), "models", ref.Type, ref.Name, ref.Tag)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return
	}
	file = filepath.Join(dir, filename)
	return
}

func (d *Downloader) alreadyDownloaded(destFile string) bool {
	// Check actual model file exists and is non-empty
	fi, err := os.Stat(destFile)
	if err != nil || fi.Size() == 0 {
		return false
	}
	// Also verify it looks like a complete file (size > 1 MB — sanity check)
	return fi.Size() > 1*1024*1024
}

func (d *Downloader) downloadWithProgress(url, destFile string, progress ProgressCallback) error {
	ctx := context.Background()
	if d.ctx != nil {
		ctx = d.ctx
	}

	// Use a simple client — HuggingFace public files redirect to CDN
	client := &http.Client{
		Timeout: 0, // no timeout for large files
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 15 {
				return fmt.Errorf("troppi redirect (%d)", len(via))
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Vortelio/1.0)")
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Vortelio/1.0)")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity") // disable gzip to get real Content-Length

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("download cancelled")
		}
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// proceed
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("access denied (HTTP %d) — the model may require a HuggingFace token", resp.StatusCode)
	case http.StatusNotFound:
		return fmt.Errorf("file not found (404)\nURL: %s", url)
	default:
		return fmt.Errorf("server: %s\nURL: %s", resp.Status, url)
	}

	if err := os.MkdirAll(filepath.Dir(destFile), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	// Download to a temp file and rename at the end, so an interrupted download
	// never leaves a partial file that alreadyDownloaded() would treat as complete.
	tmpFile := destFile + ".partial"
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmpFile)
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 512*1024) // 512 KB buffer

	for {
		if ctx.Err() != nil {
			cleanup()
			return fmt.Errorf("download cancelled")
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				cleanup()
				return werr
			}
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			cleanup()
			if ctx.Err() != nil {
				return fmt.Errorf("download cancelled")
			}
			return fmt.Errorf("read error: %w", readErr)
		}
	}
	if downloaded == 0 {
		cleanup()
		return fmt.Errorf("empty file received from HuggingFace")
	}
	if total > 0 && downloaded < total {
		cleanup()
		return fmt.Errorf("incomplete download: received %d of %d bytes", downloaded, total)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("write error: %w", err)
	}
	os.Remove(destFile) // Windows: rename fails if the target exists
	if err := os.Rename(tmpFile, destFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("finalize download: %w", err)
	}
	return nil
}

func dirOrFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (d *Downloader) saveManifest(ref *ModelRef, dir, localPath, format, params, license, source, chatTemplate string, stopTokens []string) error {
	// Use directory size for multi-file models (diffusers/transformers)
	size := dirOrFileSize(dir)
	if size == 0 {
		size = dirOrFileSize(localPath)
	}
	m := &Model{
		Type:         ref.Type,
		Name:         ref.Name,
		Tag:          ref.Tag,
		Format:       format,
		SizeBytes:    size,
		LocalPath:    localPath,
		Source:       source,
		Parameters:   params,
		License:      license,
		ChatTemplate: chatTemplate,
		StopTokens:   stopTokens,
		DownloadedAt: time.Now(),
		Capabilities: capabilitiesFor(ref.Type),
	}
	return d.store.Save(m)
}

func capabilitiesFor(modelType string) []string {
	switch modelType {
	case "llm":
		return []string{"text-generation", "chat", "completion"}
	case "image":
		return []string{"text-to-image", "image-to-image"}
	case "audio":
		return []string{"speech-to-text", "text-to-speech"}
	case "video":
		return []string{"text-to-video"}
	}
	return nil
}

// ─── LOCAL IMPORTER ───────────────────────────────────────────────────────────

type LocalImporter struct {
	store *ModelStore
}

func NewLocalImporter() *LocalImporter {
	return &LocalImporter{store: NewModelStore()}
}

func (li *LocalImporter) Import(path, modelType string) error {
	if !validTypes[modelType] {
		return fmt.Errorf("tipo sconosciuto %q", modelType)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}

	base := filepath.Base(path)
	ext := strings.TrimPrefix(filepath.Ext(base), ".")
	name := strings.TrimSuffix(base, filepath.Ext(base))
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))

	destDir := filepath.Join(config.HomeDir(), "models", modelType, name, "local")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	dest := filepath.Join(destDir, base)

	if err := os.Symlink(path, dest); err != nil && !os.IsExist(err) {
		if err2 := copyFile(path, dest); err2 != nil {
			return err2
		}
	}

	chatTemplate, stopTokens := detectChatTemplate(name)
	m := &Model{
		Type:         modelType,
		Name:         name,
		Tag:          "local",
		Format:       ext,
		SizeBytes:    fi.Size(),
		LocalPath:    dest,
		Source:       "local:" + path,
		ChatTemplate: chatTemplate,
		StopTokens:   stopTokens,
		DownloadedAt: time.Now(),
		Capabilities: capabilitiesFor(modelType),
	}
	return li.store.Save(m)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// TestTransformersFiles is exported for testing only
func TestTransformersFiles(files []hfFileInfo) []hfFileInfo {
	return transformersFilesToDownload(files)
}
