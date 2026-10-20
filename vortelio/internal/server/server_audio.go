package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// Audio studio API (Audio page): speech with built-in or cloned voices,
// music and sound effects, speech editing, stem separation, transcription.
// Requests are JSON; audio travels as base64 (any format the engines or
// ffmpeg can read — the web UI always sends WAV). Every model runs natively
// (audio.cpp / whisper.cpp), one at a time.

const maxAudioBody = 200 << 20

// audioModelInfo is an installed native audio model as the UI needs it.
type audioModelInfo struct {
	Ref          string   `json:"ref"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Tasks        []string `json:"tasks"`
	Engine       string   `json:"engine"`
	Languages    []string `json:"languages,omitempty"`
	Voices       []string `json:"voices,omitempty"`
	DefaultVoice string   `json:"default_voice,omitempty"`
	SizeBytes    int64    `json:"size_bytes"`
}

func installedAudioModels() []audioModelInfo {
	ms, _ := hub.NewModelStore().List()
	var out []audioModelInfo
	for _, m := range ms {
		if m.Type != "audio" || m.Engine == "" || len(m.Tasks) == 0 {
			continue
		}
		name := m.DisplayName
		ref := &hub.ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
		if e, ok := hub.AudioCatalogFor(ref); ok && name == "" {
			name = e.Name
		}
		if name == "" {
			name = m.Name + ":" + m.Tag
		}
		out = append(out, audioModelInfo{
			Ref: ref.String(), Name: name, Description: m.Description, Tasks: m.Tasks, Engine: m.Engine,
			Languages: m.Languages, Voices: m.Voices, DefaultVoice: m.DefaultVoice, SizeBytes: m.SizeBytes,
		})
	}
	return out
}

// GET /api/audio/catalog → curated models (with installed flag) + installed models.
func handleAudioCatalog(w http.ResponseWriter, r *http.Request) {
	installed := installedAudioModels()
	have := map[string]bool{}
	for _, m := range installed {
		have[m.Ref] = true
	}
	type row struct {
		hub.AudioCatalogEntry
		Installed bool `json:"installed"`
	}
	var cat []row
	for _, e := range hub.AudioCatalog {
		ref, _ := hub.ParseModelRef(e.Ref)
		cat = append(cat, row{AudioCatalogEntry: e, Installed: ref != nil && have[ref.String()]})
	}
	respond(w, 200, map[string]any{
		"catalog": cat, "models": installed,
		"ffmpeg": runtime.FindFFmpeg() != "",
	})
}

// pickAudioModel returns the requested model, or the best installed one
// for task when none is named.
func pickAudioModel(name, task string) (*hub.Model, error) {
	store := hub.NewModelStore()
	if name != "" {
		ref, err := hub.ParseModelRef(name)
		if err != nil {
			return nil, err
		}
		m, err := store.Resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("model %s is not installed", name)
		}
		if m.Engine == "" {
			return nil, fmt.Errorf("%s is not a native audio model", name)
		}
		if !m.HasTask(task) {
			return nil, fmt.Errorf("%s cannot do this (%s)", name, task)
		}
		return m, nil
	}
	ms, _ := store.List()
	var best *hub.Model
	bestRank := 1 << 30
	for _, m := range ms {
		if m.Type != "audio" || m.Engine == "" || !m.HasTask(task) {
			continue
		}
		rank := 100
		for i, e := range hub.AudioCatalog {
			if e.Ref == m.Type+"/"+m.Name+":"+m.Tag {
				rank = i
				if !e.Recommended {
					rank += 50
				}
			}
		}
		if rank < bestRank {
			best, bestRank = m, rank
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no installed model can do this yet — install one from the Audio page (%s)", task)
	}
	return best, nil
}

// audioWork is a scratch directory for one request's inputs.
func audioWork() (string, func(), error) {
	dir, err := os.MkdirTemp("", "vortelio-audio-req-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

func decodeAudioB64(s string) ([]byte, error) {
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("audio must be base64-encoded")
	}
	return b, nil
}

func readAudioJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonError(w, 400, "invalid request: "+err.Error())
		return false
	}
	return true
}

func audioCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Minute)
}

func respondWAV(w http.ResponseWriter, model *hub.Model, wav []byte, started time.Time) {
	secs := 0.0
	if p, err := runtime.DecodeWAV(wav); err == nil {
		secs = p.Seconds()
	}
	respond(w, 200, map[string]any{
		"model": model.Type + "/" + model.Name + ":" + model.Tag, "mime": "audio/wav",
		"audio": base64.StdEncoding.EncodeToString(wav), "seconds": secs,
		"elapsed": time.Since(started).Seconds(),
	})
}

// POST /api/audio/speech {model?, text, voice?, language?, ref_audio?, ref_text?, seed?}
// With ref_audio the text is spoken in that voice (cloning models).
func handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Text     string `json:"text"`
		Voice    string `json:"voice"`
		Language string `json:"language"`
		RefAudio string `json:"ref_audio"`
		RefText  string `json:"ref_text"`
		Seed     *int64 `json:"seed"`
	}
	if !readAudioJSON(w, r, &req) {
		return
	}
	task := hub.TaskTTS
	if req.RefAudio != "" {
		task = hub.TaskClone
	}
	m, err := pickAudioModel(req.Model, task)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	started := time.Now()
	ctx, cancel := audioCtx(r)
	defer cancel()
	sr := runtime.SpeechRequest{Text: req.Text, Voice: req.Voice, Language: req.Language, RefText: req.RefText, Seed: req.Seed}
	if req.RefAudio != "" {
		data, err := decodeAudioB64(req.RefAudio)
		if err != nil {
			jsonError(w, 400, err.Error())
			return
		}
		dir, done, err := audioWork()
		if err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		defer done()
		path, pcm, err := runtime.PrepareAudioInput(ctx, data, dir, 0, 1)
		if err != nil {
			jsonError(w, 400, err.Error())
			return
		}
		if pcm.Seconds() > 60 {
			jsonError(w, 400, "use a voice recording of 3–30 seconds (this one is longer than a minute)")
			return
		}
		sr.RefAudio = path
	}
	wav, err := runtime.NativeSpeak(ctx, m, getHardware(), sr)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	respondWAV(w, m, wav, started)
}

// POST /api/audio/generate {model?, kind: "music"|"sfx", prompt, seconds?, seed?}
func handleAudioGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string  `json:"model"`
		Kind    string  `json:"kind"`
		Prompt  string  `json:"prompt"`
		Seconds float64 `json:"seconds"`
		Seed    *int64  `json:"seed"`
	}
	if !readAudioJSON(w, r, &req) {
		return
	}
	task := hub.TaskMusic
	if req.Kind == "sfx" {
		task = hub.TaskSFX
	}
	m, err := pickAudioModel(req.Model, task)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	started := time.Now()
	ctx, cancel := audioCtx(r)
	defer cancel()
	wav, err := runtime.NativeGenerateAudio(ctx, m, getHardware(), req.Prompt, req.Seconds, req.Seed)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	respondWAV(w, m, wav, started)
}

// POST /api/audio/edit {model?, audio, transcript?, target, asr_model?}
// Without a transcript the recording is transcribed first.
func handleAudioEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model      string `json:"model"`
		Audio      string `json:"audio"`
		Transcript string `json:"transcript"`
		Target     string `json:"target"`
		ASRModel   string `json:"asr_model"`
	}
	if !readAudioJSON(w, r, &req) {
		return
	}
	m, err := pickAudioModel(req.Model, hub.TaskEdit)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	data, err := decodeAudioB64(req.Audio)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	started := time.Now()
	ctx, cancel := audioCtx(r)
	defer cancel()
	dir, done, err := audioWork()
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	defer done()
	path, pcm, err := runtime.PrepareAudioInput(ctx, data, dir, 0, 1)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if pcm.Seconds() > 60 {
		jsonError(w, 400, "speech editing works on recordings up to 60 seconds")
		return
	}
	transcript := strings.TrimSpace(req.Transcript)
	if transcript == "" {
		asr, err := pickAudioModel(req.ASRModel, hub.TaskASR)
		if err != nil {
			jsonError(w, 400, "type what the recording says, or install a transcription model: "+err.Error())
			return
		}
		if transcript, err = runtime.NativeTranscribe(ctx, asr, getHardware(), path, ""); err != nil {
			jsonError(w, 500, "transcription failed: "+err.Error())
			return
		}
	}
	wav, err := runtime.NativeEditSpeech(ctx, m, getHardware(), path, transcript, req.Target)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	secs := 0.0
	if p, err := runtime.DecodeWAV(wav); err == nil {
		secs = p.Seconds()
	}
	respond(w, 200, map[string]any{
		"mime": "audio/wav", "audio": base64.StdEncoding.EncodeToString(wav), "seconds": secs,
		"transcript": transcript, "elapsed": time.Since(started).Seconds(),
	})
}

// POST /api/audio/separate {model?, audio} → {stems: [{name, audio}]}
func handleAudioSeparate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Audio string `json:"audio"`
	}
	if !readAudioJSON(w, r, &req) {
		return
	}
	m, err := pickAudioModel(req.Model, hub.TaskSep)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	data, err := decodeAudioB64(req.Audio)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	started := time.Now()
	ctx, cancel := audioCtx(r)
	defer cancel()
	dir, done, err := audioWork()
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	defer done()
	// The separation models are trained on 44.1 kHz stereo.
	path, _, err := runtime.PrepareAudioInput(ctx, data, dir, 44100, 2)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	stems, err := runtime.NativeSeparate(ctx, m, getHardware(), path)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	type stem struct {
		Name  string `json:"name"`
		Audio string `json:"audio"`
	}
	out := make([]stem, 0, len(stems))
	for _, s := range stems {
		out = append(out, stem{Name: s.Name, Audio: base64.StdEncoding.EncodeToString(s.WAV)})
	}
	respond(w, 200, map[string]any{"mime": "audio/wav", "stems": out, "elapsed": time.Since(started).Seconds()})
}

// POST /api/audio/transcribe {model?, audio, language?} → {text}
func handleAudioTranscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Audio    string `json:"audio"`
		Language string `json:"language"`
	}
	if !readAudioJSON(w, r, &req) {
		return
	}
	m, err := pickAudioModel(req.Model, hub.TaskASR)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	data, err := decodeAudioB64(req.Audio)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	started := time.Now()
	ctx, cancel := audioCtx(r)
	defer cancel()
	dir, done, err := audioWork()
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	defer done()
	path, pcm, err := runtime.PrepareAudioInput(ctx, data, dir, 16000, 1)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	text, err := runtime.NativeTranscribe(ctx, m, getHardware(), path, req.Language)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]any{"text": text, "seconds": pcm.Seconds(), "elapsed": time.Since(started).Seconds(),
		"model": m.Type + "/" + m.Name + ":" + m.Tag})
}

// transcodeAudio converts WAV to mp3/ogg/flac/opus/aac with ffmpeg.
func transcodeAudio(ctx context.Context, wav []byte, format string) ([]byte, error) {
	ff := runtime.FindFFmpeg()
	if ff == "" {
		return nil, errors.New("ffmpeg not installed")
	}
	codec := map[string]string{"mp3": "mp3", "ogg": "ogg", "opus": "ogg", "flac": "flac", "aac": "adts"}[format]
	if codec == "" {
		return nil, fmt.Errorf("unsupported format %q", format)
	}
	cmd := exec.CommandContext(ctx, ff, "-loglevel", "error", "-f", "wav", "-i", "pipe:0", "-f", codec, "pipe:1")
	runtime.HideWindow(cmd)
	cmd.Stdin = bytes.NewReader(wav)
	return cmd.Output()
}
