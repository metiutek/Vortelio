package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/hub"
	rt "github.com/vortelio/vortelio/internal/runtime"
)

// ── Model-as-tool media provider ─────────────────────────────────────────────
//
// Lets an LLM (local or cloud) call Vortelio's other generative models as tools:
// image, video, audio (TTS/STT) and 3D. Each tool resolves a locally installed
// model of the requested type (the caller may name one, otherwise the first
// installed model of that type is used), runs it, saves the artifact to the
// default output directory, and returns the path. A "media_generated" tool event
// carries a base64 data URI so the UI can render the result inline without
// bloating the LLM context with binary data.

type mediaProvider struct {
	emit rt.ToolEventEmitter
}

func newMediaProvider(emit rt.ToolEventEmitter) *mediaProvider {
	return &mediaProvider{emit: emit}
}

func (m *mediaProvider) Tools() []rt.ToolDef {
	return []rt.ToolDef{
		toolDef("generate_image", "Generate an image from a text prompt using a local image model (e.g. Stable Diffusion). Returns the saved file path.",
			`{"type":"object","properties":{"prompt":{"type":"string","description":"What to draw."},"model":{"type":"string","description":"Optional image model name. Defaults to the first installed image model."},"steps":{"type":"integer","description":"Sampling steps. Default 20."}},"required":["prompt"]}`),
		toolDef("generate_video", "Generate a short video clip from a text prompt using a local video model. Returns the saved file path.",
			`{"type":"object","properties":{"prompt":{"type":"string","description":"What the video should show."},"model":{"type":"string","description":"Optional video model name."},"steps":{"type":"integer","description":"Sampling steps. Default 20."}},"required":["prompt"]}`),
		toolDef("text_to_speech", "Synthesize speech audio from text using a local TTS model (e.g. Kokoro/Bark). Returns the saved audio file path.",
			`{"type":"object","properties":{"text":{"type":"string","description":"Text to speak."},"model":{"type":"string","description":"Optional audio model name."}},"required":["text"]}`),
		toolDef("transcribe", "Transcribe an audio file to text using a local speech-to-text model (e.g. Whisper).",
			`{"type":"object","properties":{"input_file":{"type":"string","description":"Path to the audio file to transcribe."},"model":{"type":"string","description":"Optional audio model name."}},"required":["input_file"]}`),
		toolDef("generate_3d", "Generate a 3D model (mesh) from a text prompt or an input image using a local 3D model. Returns the saved file path.",
			`{"type":"object","properties":{"prompt":{"type":"string","description":"Text prompt describing the object."},"input_file":{"type":"string","description":"Optional input image path for image-to-3D."},"model":{"type":"string","description":"Optional 3D model name."}},"required":[]}`),
		toolDef("list_models", "List the user's installed models and a set of models that can be installed on demand. Use this before generating media if you are unsure a suitable model is installed.",
			`{"type":"object","properties":{},"required":[]}`),
		toolDef("install_model", "Download and install a model so it can be used (e.g. an image model for generate_image). Accept a catalog ref like \"image/openjourney\" or a plain name like \"stable diffusion\", \"sdxl\", \"whisper\". The download can take a few minutes; when it finishes the model is ready to use.",
			`{"type":"object","properties":{"model":{"type":"string","description":"Model to install: a catalog ref (image/openjourney, audio/whisper:base, llm/qwen2.5:7b) or a plain name."}},"required":["model"]}`),
		toolDef("rename_file", "Rename or move a file on the user's computer (e.g. rename a generated image). Use this to actually perform the rename yourself instead of telling the user how to do it.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Current full file path."},"new_name":{"type":"string","description":"New name. A bare name keeps the same folder and extension; a full path moves it there."}},"required":["path","new_name"]}`),
	}
}

func (m *mediaProvider) Execute(name, argsJSON string) (string, error) {
	switch name {
	case "generate_image":
		return m.generateImage(argsJSON)
	case "generate_video":
		return m.generateVideo(argsJSON)
	case "text_to_speech":
		return m.textToSpeech(argsJSON)
	case "transcribe":
		return m.transcribe(argsJSON)
	case "generate_3d":
		return m.generate3D(argsJSON)
	case "list_models":
		return m.listModels()
	case "install_model":
		return m.installModel(argsJSON)
	case "rename_file":
		return m.renameFile(argsJSON)
	default:
		return "", fmt.Errorf("unknown media tool: %s", name)
	}
}

// renameFile renames (or moves) a file. A bare new_name keeps the original folder
// and, if no extension is given, the original extension.
func (m *mediaProvider) renameFile(argsJSON string) (string, error) {
	var a struct {
		Path    string `json:"path"`
		NewName string `json:"new_name"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	src := strings.TrimSpace(a.Path)
	nn := strings.TrimSpace(a.NewName)
	if src == "" || nn == "" {
		return "", fmt.Errorf("path and new_name are required")
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("file not found: %s", src)
	}
	var dst string
	if strings.ContainsAny(nn, `/\`) || filepath.IsAbs(nn) {
		dst = nn // full/relative path provided
	} else {
		dst = filepath.Join(filepath.Dir(src), nn) // bare name → same folder
	}
	if filepath.Ext(dst) == "" {
		dst += filepath.Ext(src) // keep original extension
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("rename failed: %v", err)
	}
	b, _ := json.Marshal(map[string]interface{}{"status": "ok", "from": src, "to": dst, "note": "File renamed."})
	return string(b), nil
}

// listModels reports installed models + a curated set installable on demand.
func (m *mediaProvider) listModels() (string, error) {
	models, _ := hub.NewModelStore().List()
	installed := []string{}
	for _, md := range models {
		installed = append(installed, md.Type+"/"+md.Name+":"+md.Tag)
	}
	installable := []map[string]string{
		{"ref": "image/openjourney", "note": "Stable Diffusion 1.5 — light, good for 4GB GPUs"},
		{"ref": "image/sdxl", "note": "Stable Diffusion XL — higher quality, needs more VRAM"},
		{"ref": "image/flux:schnell", "note": "FLUX.1 Schnell — top quality, large"},
		{"ref": "audio/whisper:base", "note": "Whisper — speech-to-text"},
		{"ref": "audio/kokoro", "note": "Kokoro — text-to-speech"},
		{"ref": "llm/qwen3.5:9b", "note": "Qwen 3.5 9B — capable chat/coding"},
		{"ref": "llm/qwen3.5:4b", "note": "Qwen 3.5 4B — light, fast"},
		{"ref": "llm/gemma4:e4b", "note": "Gemma 4 E4B — fast, multilingual"},
	}
	b, _ := json.Marshal(map[string]interface{}{"installed": installed, "installable": installable})
	return string(b), nil
}

// resolveInstallRef maps a catalog ref or a plain model name to a catalog ref.
func resolveInstallRef(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if strings.Contains(q, "/") { // already a ref like image/openjourney[:tag]
		return q
	}
	s := strings.ToLower(q)
	switch {
	case strings.Contains(s, "sdxl"), strings.Contains(s, "xl"):
		return "image/sdxl:latest"
	case strings.Contains(s, "flux"):
		return "image/flux:schnell"
	case strings.Contains(s, "openjourney"), strings.Contains(s, "midjourney"):
		return "image/openjourney:latest"
	case strings.Contains(s, "stable"), strings.Contains(s, "diffusion"), s == "sd", strings.Contains(s, "image"):
		return "image/openjourney:latest" // SD 1.5 — lightest image model
	case strings.Contains(s, "whisper"), strings.Contains(s, "transcri"), strings.Contains(s, "speech-to"):
		return "audio/whisper:base"
	case strings.Contains(s, "kokoro"), strings.Contains(s, "tts"), strings.Contains(s, "voice"), strings.Contains(s, "speech"):
		return "audio/kokoro:latest"
	case strings.Contains(s, "coder"):
		return "llm/qwen3-coder:30b"
	case strings.Contains(s, "qwen"):
		return "llm/qwen3.5:9b"
	case strings.Contains(s, "gemma"):
		return "llm/gemma4:e4b"
	case strings.Contains(s, "llama"):
		return "llm/llama3.2:3b"
	}
	return q
}

// installModel downloads a model on demand so it can be used by the other tools.
func (m *mediaProvider) installModel(argsJSON string) (string, error) {
	var a struct {
		Model string `json:"model"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	refStr := resolveInstallRef(a.Model)
	if refStr == "" {
		return "", fmt.Errorf("specify which model to install (e.g. \"image/openjourney\" or \"stable diffusion\")")
	}
	ref, err := hub.ParseModelRef(refStr)
	if err != nil {
		return "", fmt.Errorf("unknown model %q: %v", a.Model, err)
	}
	// Already installed? Then we're done.
	if mdl, e := hub.NewModelStore().Resolve(ref); e == nil && mdl != nil {
		b, _ := json.Marshal(map[string]interface{}{"status": "ok", "model": refStr, "note": "already installed"})
		return string(b), nil
	}
	if m.emit != nil {
		m.emit("tool_progress", map[string]string{"text": "Downloading " + refStr + " — this can take a few minutes…"})
	}
	d := hub.NewDownloader()
	var lastPct int
	if err := d.Pull(ref, func(done, total int64) {
		if total > 0 && m.emit != nil {
			p := int(done * 100 / total)
			if p >= lastPct+10 {
				lastPct = p
				m.emit("tool_progress", map[string]string{"text": fmt.Sprintf("Downloading %s… %d%%", refStr, p)})
			}
		}
	}); err != nil {
		return "", fmt.Errorf("download failed for %s: %v", refStr, err)
	}
	b, _ := json.Marshal(map[string]interface{}{
		"status": "ok", "model": refStr,
		"note": "Installed and ready. You can now use it (e.g. call generate_image again).",
	})
	return string(b), nil
}

// findMediaModel returns an installed model of the given type. If name is set it
// matches by Name/DisplayName (substring, case-insensitive); otherwise the first
// installed model of that type is returned.
func findMediaModel(typ, name string) (*hub.Model, error) {
	models, err := hub.NewModelStore().List()
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	var first *hub.Model
	for _, mdl := range models {
		if mdl.Type != typ {
			continue
		}
		if first == nil {
			first = mdl
		}
		if name == "" {
			return mdl, nil
		}
		if strings.Contains(strings.ToLower(mdl.Name), name) ||
			strings.Contains(strings.ToLower(mdl.DisplayName), name) {
			return mdl, nil
		}
	}
	if name != "" && first != nil {
		return first, nil // requested name not found; fall back to first available
	}
	if first != nil {
		return first, nil
	}
	return nil, fmt.Errorf("no installed %s model found — call the install_model tool to download one (for images use model \"stable diffusion\"), then retry", typ)
}

// saveArtifact writes data to the default output dir with a timestamped name and
// emits a media_generated event carrying a data URI for inline UI rendering.
func (m *mediaProvider) saveArtifact(kind, ext, mime string, data []byte) (string, error) {
	dir := rt.DefaultOutputDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	fname := fmt.Sprintf("vortelio-%s-%d.%s", kind, time.Now().Unix(), ext)
	path := filepath.Join(dir, fname)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	if m.emit != nil {
		dataURI := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		m.emit("media_generated", map[string]interface{}{
			"kind": kind, "path": path, "mime": mime, "data_uri": dataURI,
		})
	}
	b, _ := json.Marshal(map[string]interface{}{
		"status": "ok", "kind": kind, "path": path,
		"note": "Artifact saved. The user can view it inline; reference it by path if needed.",
	})
	return string(b), nil
}

func (m *mediaProvider) generateImage(argsJSON string) (string, error) {
	var a struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Steps  int    `json:"steps"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if a.Steps <= 0 {
		a.Steps = 20
	}
	// Prefer a configured cloud image provider (BYOK: OpenAI/Stability/fal.ai).
	var cloudErr error
	if p, key, ok := cloud.ConfiguredMediaProvider("image"); ok {
		data, ext, err := cloud.GenerateMedia(p.ID, key, a.Model, a.Prompt)
		if err == nil {
			return m.saveArtifact("image", ext, "image/"+ext, data)
		}
		cloudErr = err
	}
	mdl, err := findMediaModel("image", a.Model)
	if err != nil {
		if cloudErr != nil {
			return "", fmt.Errorf("cloud: %v", cloudErr)
		}
		return "", err
	}
	runner := rt.NewImageRunner(mdl, getHardware())
	data, err := runner.GenerateToBytes(a.Prompt, a.Steps, false)
	if err != nil {
		return "", err
	}
	return m.saveArtifact("image", "png", "image/png", data)
}

func (m *mediaProvider) textToSpeech(argsJSON string) (string, error) {
	var a struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Text) == "" {
		return "", fmt.Errorf("text is required")
	}
	// Prefer a configured cloud TTS provider (BYOK: OpenAI TTS / ElevenLabs).
	var cloudErr error
	if p, key, ok := cloud.ConfiguredMediaProvider("audio"); ok {
		data, ext, err := cloud.GenerateMedia(p.ID, key, a.Model, a.Text)
		if err == nil {
			return m.saveArtifact("audio", ext, "audio/"+ext, data)
		}
		cloudErr = err
	}
	mdl, err := findMediaModel("audio", a.Model)
	if err != nil {
		if cloudErr != nil {
			return "", fmt.Errorf("cloud: %v", cloudErr)
		}
		return "", err
	}
	runner := rt.NewAudioRunner(mdl, getHardware())
	data, err := runner.SynthesizeToBytes(a.Text)
	if err != nil {
		return "", err
	}
	return m.saveArtifact("audio", "wav", "audio/wav", data)
}

func (m *mediaProvider) transcribe(argsJSON string) (string, error) {
	var a struct {
		InputFile string `json:"input_file"`
		Model     string `json:"model"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.InputFile) == "" {
		return "", fmt.Errorf("input_file is required")
	}
	if _, err := os.Stat(a.InputFile); err != nil {
		return "", fmt.Errorf("input file not found: %s", a.InputFile)
	}
	mdl, err := findMediaModel("audio", a.Model)
	if err != nil {
		return "", err
	}
	runner := rt.NewAudioRunner(mdl, getHardware())
	text, err := runner.TranscribeText(a.InputFile)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]interface{}{"status": "ok", "text": text})
	return string(b), nil
}

func (m *mediaProvider) generateVideo(argsJSON string) (string, error) {
	var a struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Steps  int    `json:"steps"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if a.Steps <= 0 {
		a.Steps = 20
	}
	// Prefer a configured cloud video provider (BYOK: fal.ai).
	var cloudErr error
	if p, key, ok := cloud.ConfiguredMediaProvider("video"); ok {
		data, ext, err := cloud.GenerateMedia(p.ID, key, a.Model, a.Prompt)
		if err == nil {
			return m.saveArtifact("video", ext, "video/"+ext, data)
		}
		cloudErr = err
	}
	mdl, err := findMediaModel("video", a.Model)
	if err != nil {
		if cloudErr != nil {
			return "", fmt.Errorf("cloud: %v", cloudErr)
		}
		return "", err
	}
	runner := rt.NewVideoRunner(mdl, getHardware())
	data, err := m.runToFile(runner, &rt.RunOptions{Prompt: a.Prompt, Steps: a.Steps}, "mp4")
	if err != nil {
		return "", err
	}
	return m.saveArtifact("video", "mp4", "video/mp4", data)
}

func (m *mediaProvider) generate3D(argsJSON string) (string, error) {
	var a struct {
		Prompt    string `json:"prompt"`
		InputFile string `json:"input_file"`
		Model     string `json:"model"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Prompt) == "" && strings.TrimSpace(a.InputFile) == "" {
		return "", fmt.Errorf("provide a prompt or an input_file")
	}
	// Prefer a configured cloud 3D provider (BYOK: fal.ai).
	var cloudErr error
	if p, key, ok := cloud.ConfiguredMediaProvider("3d"); ok && strings.TrimSpace(a.Prompt) != "" {
		data, ext, err := cloud.GenerateMedia(p.ID, key, a.Model, a.Prompt)
		if err == nil {
			return m.saveArtifact("3d", ext, "model/"+ext, data)
		}
		cloudErr = err
	}
	mdl, err := findMediaModel("3d", a.Model)
	if err != nil {
		if cloudErr != nil {
			return "", fmt.Errorf("cloud: %v", cloudErr)
		}
		return "", err
	}
	runner := rt.NewThreeDRunner(mdl, getHardware())
	data, err := m.runToFile(runner, &rt.RunOptions{Prompt: a.Prompt, InputFile: a.InputFile}, "obj")
	if err != nil {
		return "", err
	}
	return m.saveArtifact("3d", "obj", "model/obj", data)
}

// runToFile runs a runner with a temp output file and returns the bytes produced.
func (m *mediaProvider) runToFile(runner rt.Runner, opts *rt.RunOptions, ext string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "vortelio-tool-*."+ext)
	if err != nil {
		return nil, err
	}
	tmp.Close()
	opts.OutputFile = tmp.Name()
	defer os.Remove(tmp.Name())
	if err := runner.Run(opts); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp.Name())
}
