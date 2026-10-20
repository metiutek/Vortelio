package hub

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vortelio/vortelio/internal/gguf"
)

// Native audio models. audio.cpp GGUFs embed their own model spec (family,
// tasks, languages, built-in voices, request options), so after download the
// manifest is filled from the file itself — any audio.cpp GGUF works, not only
// the ones listed here. The catalog below is the curated starting set: models
// tested end to end, small enough for a 4 GB GPU unless marked otherwise.

// Audio tasks Vortelio exposes.
const (
	TaskTTS   = "tts"   // text to speech with built-in voices
	TaskClone = "clone" // speak in the voice of a reference recording
	TaskMusic = "music" // music from a description
	TaskSFX   = "sfx"   // sound effects from a description
	TaskEdit  = "edit"  // change words in a recording
	TaskSep   = "sep"   // split a track into stems
	TaskASR   = "asr"   // speech to text
)

// AudioCatalogEntry is one curated, tested audio model.
type AudioCatalogEntry struct {
	Ref         string   `json:"ref"` // model reference to pull
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tasks       []string `json:"tasks"`
	Engine      string   `json:"engine"` // audiocpp | whisper
	Repo        string   `json:"repo"`
	File        string   `json:"file"`
	SizeBytes   int64    `json:"size_bytes"`
	License     string   `json:"license"`
	Languages   string   `json:"languages"`
	Recommended bool     `json:"recommended,omitempty"`
}

const audioCppRepo = "audio-cpp/audio.cpp-gguf"

// AudioCatalog is the curated audio model list, recommended models first.
var AudioCatalog = []AudioCatalogEntry{
	{Ref: "audio/kokoro:82m", Name: "Kokoro 82M", Tasks: []string{TaskTTS}, Engine: "audiocpp",
		Description: "Natural speech with 54 built-in voices (Italian: Sara, Nicola). Fast on any machine.",
		Repo:        audioCppRepo, File: "Kokoro-82M-GGUF/kokoro-82m-q8_0.gguf", SizeBytes: 189550976,
		License: "Apache-2.0", Languages: "en, it, es, fr, pt, hi, ja, zh", Recommended: true},
	{Ref: "audio/piper:lessac", Name: "Piper Lessac", Tasks: []string{TaskTTS}, Engine: "audiocpp",
		Description: "Very fast lightweight English voice, ideal for read-aloud.",
		Repo:        audioCppRepo, File: "Piper-TTS-GGUF/piper-en-us-lessac-medium-orig.gguf", SizeBytes: 62662016,
		License: "see model card", Languages: "en"},
	{Ref: "audio/chatterbox:turbo", Name: "Chatterbox Turbo", Tasks: []string{TaskTTS}, Engine: "audiocpp",
		Description: "Expressive English voice.",
		Repo:        audioCppRepo, File: "Chatterbox-Turbo-GGUF/chatterbox-turbo-q8_0.gguf", SizeBytes: 699102368,
		License: "MIT", Languages: "en"},
	{Ref: "audio/dots-tts:edit", Name: "DotTTS Edit", Tasks: []string{TaskClone, TaskEdit}, Engine: "audiocpp",
		Description: "Clones a voice from a short recording and edits words in a recording, keeping the voice. Multilingual.",
		Repo:        audioCppRepo, File: "DotTTS-Edit-GGUF/dots-tts-edit-q8_0.gguf", SizeBytes: 2962805408,
		License: "see model card", Languages: "multilingual (it, en, zh, …)", Recommended: true},
	{Ref: "audio/voxcpm:0.5b", Name: "VoxCPM 0.5B", Tasks: []string{TaskClone}, Engine: "audiocpp",
		Description: "Small voice cloning model.",
		Repo:        audioCppRepo, File: "VoxCPM1-GGUF/voxcpm-0.5b-q8_0-audiovae-f16.gguf", SizeBytes: 847888768,
		License: "Apache-2.0", Languages: "en, zh, ja, ko"},
	{Ref: "audio/stable-audio:music", Name: "Stable Audio 3 Small — Music", Tasks: []string{TaskMusic}, Engine: "audiocpp",
		Description: "Instrumental music from a description, up to 2 minutes. 10 s of music in ~6 s on a 4 GB GPU.",
		Repo:        audioCppRepo, File: "Stable-Audio-3-Small-Music-GGUF/stable-audio-3-small-music-q8_0.gguf", SizeBytes: 1683576608,
		License: "Stability AI Community", Languages: "en (prompt)", Recommended: true},
	{Ref: "audio/stable-audio:sfx", Name: "Stable Audio 3 Small — Sound effects", Tasks: []string{TaskSFX}, Engine: "audiocpp",
		Description: "Sound effects and ambiences from a description.",
		Repo:        audioCppRepo, File: "Stable-Audio-3-Small-SFX-GGUF/stable-audio-3-small-sfx-q8_0.gguf", SizeBytes: 1683576608,
		License: "Stability AI Community", Languages: "en (prompt)"},
	{Ref: "audio/htdemucs:4stems", Name: "HT-Demucs", Tasks: []string{TaskSep}, Engine: "audiocpp",
		Description: "Splits a song into vocals, drums, bass and other.",
		Repo:        audioCppRepo, File: "HTDemucs-GGUF/htdemucs-q8_0.gguf", SizeBytes: 61940768,
		License: "MIT", Languages: "-", Recommended: true},
	{Ref: "audio/parakeet:0.6b-v3", Name: "Parakeet TDT 0.6B v3", Tasks: []string{TaskASR}, Engine: "audiocpp",
		Description: "Fast, accurate transcription for 25 European languages (Italian included).",
		Repo:        audioCppRepo, File: "Parakeet-TDT-0.6B-v3-GGUF/parakeet-tdt-0.6b-v3-q8_0.gguf", SizeBytes: 915735760,
		License: "CC-BY-4.0", Languages: "it, en, de, fr, es, pt, … (25)", Recommended: true},
	{Ref: "audio/whisper:base", Name: "Whisper Base", Tasks: []string{TaskASR}, Engine: "whisper",
		Description: "Transcription in 99 languages with automatic language detection.",
		Repo:        "ggerganov/whisper.cpp", File: "ggml-base.bin", SizeBytes: 147951465,
		License: "MIT", Languages: "99 languages"},
	{Ref: "audio/whisper:large-v3", Name: "Whisper Large v3", Tasks: []string{TaskASR}, Engine: "whisper",
		Description: "Most accurate Whisper. Slow without a large GPU.",
		Repo:        "ggerganov/whisper.cpp", File: "ggml-large-v3.bin", SizeBytes: 3095033483,
		License: "MIT", Languages: "99 languages"},
}

func init() {
	for _, e := range AudioCatalog {
		hfRegistry[e.Ref] = HFEntry{Repo: e.Repo, File: e.File, Format: formatOf(e.File), License: e.License}
	}
	// The old names point at the native models now (no Python needed).
	hfRegistry["audio/kokoro:latest"] = hfRegistry["audio/kokoro:82m"]
}

func formatOf(file string) string {
	if isGGUF(file) {
		return "gguf"
	}
	return strings.TrimPrefix(filepath.Ext(file), ".")
}

// AudioCatalogFor returns the catalog entry for a model reference, if any.
func AudioCatalogFor(ref *ModelRef) (AudioCatalogEntry, bool) {
	key := ref.Type + "/" + ref.Name + ":" + ref.Tag
	for _, e := range AudioCatalog {
		if e.Ref == key {
			return e, true
		}
	}
	return AudioCatalogEntry{}, false
}

// audioSpec is the part of an audio.cpp model spec Vortelio uses.
type audioSpec struct {
	Family      string   `json:"family"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Tasks       []string `json:"tasks"`
	Languages   []string `json:"languages"`
	UI          struct {
		DefaultVoice  string   `json:"default_voice"`
		BuiltinVoices []string `json:"builtin_voices"`
	} `json:"ui"`
}

// describeAudioModel fills the engine, tasks, voices and languages of an
// audio model from its file: the audio.cpp spec embedded in the GGUF, or the
// whisper.cpp ggml format. Unknown files are left as they are.
func describeAudioModel(m *Model, catalog *AudioCatalogEntry) {
	if m.Type != "audio" {
		return
	}
	lower := strings.ToLower(filepath.Base(m.LocalPath))
	if strings.HasSuffix(lower, ".bin") && strings.HasPrefix(lower, "ggml-") {
		m.Engine, m.Tasks = "whisper", []string{TaskASR}
		m.Capabilities = []string{"speech-to-text"}
		return
	}
	if !isGGUF(lower) {
		return
	}
	g, err := gguf.Read(m.LocalPath, 0)
	if err != nil || g.Arch() != "audiocpp" {
		return
	}
	var spec audioSpec
	if s := g.String("audiocpp.model_spec.json"); s != "" {
		_ = json.Unmarshal([]byte(s), &spec)
	}
	if spec.Family == "" {
		spec.Family = g.String("audiocpp.model_spec.family")
	}
	m.Engine = "audiocpp"
	m.Family = spec.Family
	m.Languages = spec.Languages
	m.Voices = spec.UI.BuiltinVoices
	m.DefaultVoice = spec.UI.DefaultVoice
	if len(m.Voices) == 0 {
		if names, ok := g.Metadata["audiocpp.embedded_files.names"].([]any); ok {
			for _, n := range names {
				if s, ok := n.(string); ok && strings.HasPrefix(s, "voices/") && strings.HasSuffix(s, ".bin") {
					m.Voices = append(m.Voices, strings.TrimSuffix(strings.TrimPrefix(s, "voices/"), ".bin"))
				}
			}
		}
	}
	if m.Description == "" {
		m.Description = spec.Description
	}
	if catalog != nil {
		m.Tasks = append([]string{}, catalog.Tasks...)
	} else {
		m.Tasks = tasksFromSpec(spec, lower)
	}
	m.Capabilities = audioCapabilities(m.Tasks)
}

// tasksFromSpec maps the spec's task list to Vortelio tasks.
func tasksFromSpec(spec audioSpec, file string) []string {
	set := map[string]bool{}
	for _, t := range spec.Tasks {
		switch t {
		case "tts":
			set[TaskTTS] = true
		case "clone":
			set[TaskClone] = true
		case "music":
			set[TaskMusic] = true
		case "sfx":
			set[TaskSFX] = true
		case "sep":
			set[TaskSep] = true
		case "asr":
			set[TaskASR] = true
		}
	}
	// DotTTS-Edit is the only DotTTS that edits (MF/SOAR only speak).
	if spec.Family == "dots_tts" && strings.Contains(file, "edit") {
		set[TaskEdit] = true
	}
	// Without built-in voices a speech model only speaks in a cloned voice.
	if set[TaskTTS] && len(spec.UI.BuiltinVoices) == 0 && set[TaskClone] {
		delete(set, TaskTTS)
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func audioCapabilities(tasks []string) []string {
	var caps []string
	for _, t := range tasks {
		switch t {
		case TaskTTS, TaskClone:
			caps = append(caps, "text-to-speech")
		case TaskASR:
			caps = append(caps, "speech-to-text")
		case TaskMusic, TaskSFX:
			caps = append(caps, "text-to-audio")
		case TaskEdit, TaskSep:
			caps = append(caps, "audio-to-audio")
		}
	}
	return caps
}

// HasTask reports whether the model performs task.
func (m *Model) HasTask(task string) bool {
	for _, t := range m.Tasks {
		if t == task {
			return true
		}
	}
	return false
}
