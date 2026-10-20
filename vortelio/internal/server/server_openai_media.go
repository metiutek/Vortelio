package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// ── POST /v1/audio/transcriptions ────────────────────────────────────────────

func handleOpenAIAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		jsonError(w, 400, "multipart parse error: "+err.Error())
		return
	}

	modelName := r.FormValue("model")
	if modelName == "" {
		modelName = "audio/whisper:large"
	}
	responseFormat := r.FormValue("response_format")
	if responseFormat == "" {
		responseFormat = "json"
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, 400, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	ext := ".wav"
	if idx := strings.LastIndex(header.Filename, "."); idx >= 0 {
		ext = header.Filename[idx:]
	}
	tmp, err := os.CreateTemp("", "vortelio-stt-*"+ext)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		jsonError(w, 500, "failed to save upload: "+err.Error())
		return
	}
	tmp.Close()

	model, err := resolveAudioModel(modelName, hub.TaskASR)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	hw := getHardware()

	// Whisper: use the persistent worker (model stays loaded → fast). Fall back to
	// the one-shot runner if the worker can't start.
	var text string
	if strings.Contains(strings.ToLower(model.Name), "whisper") {
		text, err = runtime.TranscribeViaWorker(model.Tag, tmpPath)
		if err != nil {
			text, err = runtime.NewAudioRunner(model, hw).TranscribeText(tmpPath)
		}
	} else {
		text, err = runtime.NewAudioRunner(model, hw).TranscribeText(tmpPath)
	}
	if err != nil {
		jsonError(w, 500, "transcription failed: "+err.Error())
		return
	}

	switch responseFormat {
	case "text":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, text)
	case "srt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "1\n00:00:00,000 --> 00:00:10,000\n%s\n\n", text)
	case "vtt":
		w.Header().Set("Content-Type", "text/vtt")
		fmt.Fprintf(w, "WEBVTT\n\n00:00:00.000 --> 00:00:10.000\n%s\n\n", text)
	default: // "json", "verbose_json"
		respond(w, 200, map[string]interface{}{
			"text": text,
		})
	}
}

// ── POST /v1/audio/translations ───────────────────────────────────────────────

func handleOpenAIAudioTranslations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		jsonError(w, 400, "multipart parse error: "+err.Error())
		return
	}

	modelName := r.FormValue("model")
	if modelName == "" {
		modelName = "audio/whisper:large"
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, 400, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	ext := ".wav"
	if idx := strings.LastIndex(header.Filename, "."); idx >= 0 {
		ext = header.Filename[idx:]
	}
	tmp, err := os.CreateTemp("", "vortelio-trans-*"+ext)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		jsonError(w, 500, "failed to save upload: "+err.Error())
		return
	}
	tmp.Close()

	model, err := resolveAudioModel(modelName, hub.TaskASR)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	hw := getHardware()
	ar := runtime.NewAudioRunner(model, hw)

	text, err := ar.TranslateText(tmpPath)
	if err != nil {
		jsonError(w, 500, "translation failed: "+err.Error())
		return
	}

	respond(w, 200, map[string]interface{}{"text": text})
}

// ── POST /v1/audio/speech ─────────────────────────────────────────────────────

type openAISpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

func handleOpenAIAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req openAISpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Input == "" {
		jsonError(w, 400, "input is required")
		return
	}
	modelName := req.Model
	if modelName == "" {
		modelName = "audio/kokoro"
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "wav"
	}

	model, err := resolveAudioModel(modelName, hub.TaskTTS)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	hw := getHardware()
	var data []byte
	if model.Engine != "" {
		data, err = runtime.NativeSpeak(r.Context(), model, hw, runtime.SpeechRequest{Text: req.Input, Voice: openAIVoice(model, req.Voice)})
	} else {
		data, err = runtime.NewAudioRunner(model, hw).SynthesizeToBytes(req.Input)
	}
	if err != nil {
		jsonError(w, 500, "synthesis failed: "+err.Error())
		return
	}
	// The engines produce WAV; other formats need ffmpeg.
	if req.ResponseFormat != "wav" && req.ResponseFormat != "pcm" {
		if conv, cerr := transcodeAudio(r.Context(), data, req.ResponseFormat); cerr == nil {
			data = conv
		} else {
			req.ResponseFormat = "wav"
		}
	}

	mimeTypes := map[string]string{
		"wav":  "audio/wav",
		"mp3":  "audio/mpeg",
		"ogg":  "audio/ogg",
		"flac": "audio/flac",
	}
	mime := mimeTypes[req.ResponseFormat]
	if mime == "" {
		mime = "audio/wav"
	}
	w.Header().Set("Content-Type", mime)
	w.WriteHeader(200)
	w.Write(data)
}

// ── POST /v1/images/generations ───────────────────────────────────────────────

type openAIImageGenerationRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
}

func handleOpenAIImageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req openAIImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Prompt == "" {
		jsonError(w, 400, "prompt is required")
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "url"
	}
	model, err := resolveImageModel(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	hw := getHardware()
	ir := runtime.NewImageRunner(model, hw)

	type imgResult struct {
		B64JSON string `json:"b64_json,omitempty"`
		URL     string `json:"url,omitempty"`
	}
	var results []imgResult

	var native [][]byte
	if nativeImageModel(model) {
		ireq := runtime.ImageRequest{Prompt: req.Prompt, Count: req.N}
		if wd, ht, ok := strings.Cut(req.Size, "x"); ok {
			ireq.Width, _ = strconv.Atoi(wd)
			ireq.Height, _ = strconv.Atoi(ht)
		}
		res, err := runtime.NativeGenerateImage(r.Context(), model, hw, ireq, nil)
		if err != nil {
			jsonError(w, 500, fmt.Sprintf("generation failed: %v", err))
			return
		}
		native = res.Images
	}
	for i := 0; i < req.N; i++ {
		var data []byte
		if native != nil {
			if i >= len(native) {
				break
			}
			data = native[i]
		} else if data, err = ir.GenerateToBytes(req.Prompt, 0, false); err != nil {
			jsonError(w, 500, fmt.Sprintf("generation failed: %v", err))
			return
		}
		if req.ResponseFormat == "b64_json" {
			results = append(results, imgResult{B64JSON: base64.StdEncoding.EncodeToString(data)})
		} else {
			// Save to output dir and return path as "url"
			outDir := runtime.DefaultOutputDir()
			outPath := fmt.Sprintf("%s/vortelio-img-%d.png", outDir, time.Now().UnixNano())
			os.WriteFile(outPath, data, 0644)
			results = append(results, imgResult{URL: "file://" + outPath})
		}
	}

	respond(w, 200, map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    results,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// resolveAudioModel finds the model an OpenAI-style request names; when it is
// not installed (OpenAI clients send "tts-1", "whisper-1"…) or cannot do the
// task, the best installed model for task is used instead.
func resolveAudioModel(name, task string) (*hub.Model, error) {
	if name != "" && !strings.Contains(name, "/") {
		name = "audio/" + name
	}
	if ref, err := hub.ParseModelRef(name); err == nil {
		if m, err := hub.NewModelStore().Resolve(ref); err == nil && (m.Engine == "" || m.HasTask(task)) {
			return m, nil
		}
	}
	if m, err := pickAudioModel("", task); err == nil {
		return m, nil
	}
	// Legacy (Python) audio models carry no task list.
	if models, err := hub.NewModelStore().List(); err == nil {
		for _, m := range models {
			if m.Type == "audio" && m.Engine == "" {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("no installed audio model can do this — install one from the Audio page")
}

// openAIVoice maps an OpenAI voice name (alloy, nova…) onto a model's voices.
func openAIVoice(m *hub.Model, voice string) string {
	if voice == "" || len(m.Voices) == 0 {
		return ""
	}
	v := strings.ToLower(voice)
	for _, have := range m.Voices {
		if strings.ToLower(have) == v {
			return have
		}
	}
	for _, have := range m.Voices {
		if strings.HasSuffix(strings.ToLower(have), "_"+v) {
			return have
		}
	}
	return ""
}

// resolveImageModel finds the model an OpenAI-style request names; when it
// is not installed (clients send "dall-e-3"…) the best installed one is used.
func resolveImageModel(name string) (*hub.Model, error) {
	if m, err := pickImageModel(name); err == nil {
		return m, nil
	}
	return nil, fmt.Errorf("no image model installed — install one from the Images page")
}
