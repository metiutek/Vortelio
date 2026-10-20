package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
)

// Native audio: audio.cpp (speech, voice cloning, music, sound effects,
// speech editing, stem separation, transcription) and whisper.cpp
// (transcription) run as local HTTP servers, one model at a time, started on
// first use and stopped after AudioIdleTimeout or when another kind of model
// needs the GPU. Audio inputs are passed as server-local WAV paths.

// AudioIdleTimeout stops an unused audio server.
var AudioIdleTimeout = 5 * time.Minute

type audioProc struct {
	engine string // audiocpp | whisper
	key    string // model path + backend
	url    string
	cmd    *exec.Cmd
	done   chan struct{}
	log    *tailBuf
	dir    string
	used   time.Time
	busy   int
}

func (p *audioProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *audioProc) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	}
	if p.dir != "" {
		os.RemoveAll(p.dir)
	}
}

var (
	audioMu   sync.Mutex
	audioCur  *audioProc
	audioOnce sync.Once
)

// StopAudioServers stops the running audio server (frees its memory).
func StopAudioServers() {
	audioMu.Lock()
	defer audioMu.Unlock()
	if audioCur != nil {
		audioCur.stop()
		audioCur = nil
	}
}

func audioInit() {
	audioOnce.Do(func() {
		RegisterGPUEvictor("audio", StopAudioServers)
		OnEngineReplace("audiocpp", StopAudioServers)
		OnEngineReplace("whisper", StopAudioServers)
		go func() {
			for range time.Tick(30 * time.Second) {
				audioMu.Lock()
				if audioCur != nil && audioCur.busy == 0 && time.Since(audioCur.used) > AudioIdleTimeout {
					audioCur.stop()
					audioCur = nil
				}
				audioMu.Unlock()
			}
		}()
	})
}

// audioServerTask is the audiocpp_server session task for a model: one
// session serves every task of its family (DotTTS speaks, clones and edits
// from a "tts" session; Stable Audio makes music and effects from "gen").
func audioServerTask(m *hub.Model) string {
	switch {
	case m.HasTask(hub.TaskASR):
		return "asr"
	case m.HasTask(hub.TaskSep):
		return "sep"
	case m.HasTask(hub.TaskMusic), m.HasTask(hub.TaskSFX):
		return "gen"
	}
	return "tts"
}

// acquireAudio returns a running server for m and marks it busy; call the
// returned release when the request is done.
func acquireAudio(ctx context.Context, m *hub.Model, hw *Hardware) (*audioProc, func(), error) {
	audioInit()
	if m.Engine != "audiocpp" && m.Engine != "whisper" {
		return nil, nil, fmt.Errorf("%s is not a native audio model", m.Name)
	}
	AcquireGPU("audio")
	audioMu.Lock()
	defer audioMu.Unlock()
	if audioCur != nil && audioCur.alive() && audioCur.key == m.LocalPath {
		audioCur.busy++
		audioCur.used = time.Now()
		p := audioCur
		return p, releaseAudio(p), nil
	}
	if audioCur != nil {
		audioCur.stop()
		audioCur = nil
	}
	bin, err := EnsureEngine(ctx, m.Engine, hw)
	if err != nil {
		return nil, nil, fmt.Errorf("audio engine: %w", err)
	}
	accel := "cpu"
	if st := GetEngineStatus(m.Engine); st.Accel != "" {
		accel = st.Accel
	}
	p, err := startAudioServer(ctx, bin, m, accel)
	if err != nil && accel != "cpu" && isOOM(err) {
		// The GPU is too small (or busy): the CPU is slower but works.
		p, err = startAudioServer(ctx, bin, m, "cpu")
	}
	if err != nil {
		return nil, nil, err
	}
	p.busy = 1
	p.used = time.Now()
	audioCur = p
	return p, releaseAudio(p), nil
}

func releaseAudio(p *audioProc) func() {
	return func() {
		audioMu.Lock()
		p.busy--
		p.used = time.Now()
		audioMu.Unlock()
	}
}

func isOOM(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "out of memory") || strings.Contains(m, "cudamalloc") || strings.Contains(m, "failed to allocate")
}

func startAudioServer(ctx context.Context, bin string, m *hub.Model, accel string) (*audioProc, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "vortelio-audio-")
	if err != nil {
		return nil, err
	}
	p := &audioProc{engine: m.Engine, key: m.LocalPath, url: fmt.Sprintf("http://127.0.0.1:%d", port),
		done: make(chan struct{}), log: &tailBuf{max: 32768}, dir: dir}

	var args []string
	switch m.Engine {
	case "audiocpp":
		backend := accel
		if backend == "" {
			backend = "cpu"
		}
		cfg := map[string]any{
			"host": "127.0.0.1", "port": port, "backend": backend,
			"lazy_load": false, "max_loaded_models": 1,
			// Music can take minutes; Vortelio bounds its own wait.
			"busy_timeout_ms": 0,
			"models": []map[string]any{{
				"id": "model", "family": m.Family, "path": m.LocalPath,
				"task": audioServerTask(m), "mode": "offline",
			}},
		}
		data, _ := json.Marshal(cfg)
		cfgPath := filepath.Join(dir, "server.json")
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			return nil, err
		}
		args = []string{"--config", cfgPath, "--no-ui"}
	case "whisper":
		args = []string{"-m", m.LocalPath, "--host", "127.0.0.1", "--port", fmt.Sprint(port)}
		if accel == "cpu" {
			args = append(args, "-ng")
		}
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout, cmd.Stderr = p.log, p.log
	HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cannot start %s: %w", filepath.Base(bin), err)
	}
	p.cmd = cmd
	go func() { cmd.Wait(); close(p.done) }()

	health := p.url + "/health"
	if m.Engine == "whisper" {
		health = p.url + "/"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			os.RemoveAll(dir)
			return nil, fmt.Errorf("the audio engine stopped while loading %s: %s", m.Name, lastLines(p.log.String(), 6))
		case <-ctx.Done():
			p.stop()
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		resp, err := client.Get(health)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p, nil
		}
	}
	p.stop()
	return nil, fmt.Errorf("the audio engine did not become ready: %s", lastLines(p.log.String(), 6))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// ── requests ────────────────────────────────────────────────────────────────

// AudioResult is one generated audio file.
type AudioResult struct {
	Name string `json:"name"` // "audio", or a stem name (vocals, drums…)
	WAV  []byte `json:"-"`
}

var audioHTTP = &http.Client{Timeout: 0}

func (p *audioProc) postJSON(ctx context.Context, path string, body map[string]any) (string, []byte, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+path, bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req)
}

func (p *audioProc) do(req *http.Request) (string, []byte, error) {
	resp, err := audioHTTP.Do(req)
	if err != nil {
		if !p.alive() {
			return "", nil, fmt.Errorf("the audio engine stopped: %s", lastLines(p.log.String(), 4))
		}
		return "", nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			return "", nil, errors.New(e.Error.Message)
		}
		return "", nil, fmt.Errorf("audio engine HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out[:min(len(out), 300)])))
	}
	return resp.Header.Get("Content-Type"), out, nil
}

// audioOutputs extracts the WAV(s) from an audio.cpp answer: raw audio, or
// JSON with base64 "audio" / "named_audio_outputs".
func audioOutputs(ctype string, data []byte) ([]AudioResult, error) {
	if strings.HasPrefix(ctype, "audio/") || bytes.HasPrefix(data, []byte("RIFF")) {
		return []AudioResult{{Name: "audio", WAV: data}}, nil
	}
	var payload struct {
		Audio string `json:"audio"`
		Named []struct {
			ID    string `json:"id"`
			Audio string `json:"audio"`
		} `json:"named_audio_outputs"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, errors.New("the audio engine returned an unreadable answer")
	}
	var out []AudioResult
	for i, n := range payload.Named {
		if b, err := base64.StdEncoding.DecodeString(n.Audio); err == nil && len(b) > 0 {
			name := n.ID
			if name == "" {
				name = fmt.Sprintf("audio_%d", i)
			}
			out = append(out, AudioResult{Name: name, WAV: b})
		}
	}
	if len(out) == 0 && payload.Audio != "" {
		if b, err := base64.StdEncoding.DecodeString(payload.Audio); err == nil {
			out = append(out, AudioResult{Name: "audio", WAV: b})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the audio engine returned no audio")
	}
	return out, nil
}

// SpeechRequest is text to speech, optionally in a cloned voice.
type SpeechRequest struct {
	Text      string
	Voice     string // built-in voice (TTS models)
	RefAudio  string // WAV path: speak in this voice (cloning models)
	RefText   string // what is said in RefAudio (improves cloning)
	Language  string
	Seed      *int64
	ExtraOpts map[string]string
}

// NativeSpeak synthesizes speech with an audio.cpp model.
func NativeSpeak(ctx context.Context, m *hub.Model, hw *Hardware, r SpeechRequest) ([]byte, error) {
	if strings.TrimSpace(r.Text) == "" {
		return nil, errors.New("there is no text to speak")
	}
	if r.RefAudio == "" && !m.HasTask(hub.TaskTTS) {
		return nil, fmt.Errorf("%s speaks only in a cloned voice: add a voice recording", m.Name)
	}
	if r.RefAudio != "" && !m.HasTask(hub.TaskClone) {
		return nil, fmt.Errorf("%s cannot clone voices — pick a voice-cloning model", m.Name)
	}
	p, release, err := acquireAudio(ctx, m, hw)
	if err != nil {
		return nil, err
	}
	defer release()
	body := map[string]any{"model": "model", "input": r.Text}
	if r.RefAudio != "" {
		body["voice_ref"] = filepath.ToSlash(r.RefAudio)
		if strings.TrimSpace(r.RefText) != "" {
			body["reference_text"] = strings.TrimSpace(r.RefText)
		}
	} else if v := r.Voice; v != "" {
		body["voice"] = v
	} else if m.DefaultVoice != "" {
		body["voice"] = m.DefaultVoice
	}
	if r.Language != "" {
		body["language"] = r.Language
	}
	if r.Seed != nil {
		body["seed"] = fmt.Sprint(*r.Seed)
	}
	if len(r.ExtraOpts) > 0 {
		body["options"] = r.ExtraOpts
	}
	ctype, data, err := p.postJSON(ctx, "/v1/audio/speech", body)
	if err != nil {
		return nil, err
	}
	outs, err := audioOutputs(ctype, data)
	if err != nil {
		return nil, err
	}
	return outs[0].WAV, nil
}

// NativeGenerateAudio makes music or sound effects from a description.
func NativeGenerateAudio(ctx context.Context, m *hub.Model, hw *Hardware, prompt string, seconds float64, seed *int64) ([]byte, error) {
	if !m.HasTask(hub.TaskMusic) && !m.HasTask(hub.TaskSFX) {
		return nil, fmt.Errorf("%s does not generate music or sound effects", m.Name)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("describe the sound you want")
	}
	if seconds <= 0 {
		seconds = 10
	}
	if seconds > 120 {
		seconds = 120
	}
	p, release, err := acquireAudio(ctx, m, hw)
	if err != nil {
		return nil, err
	}
	defer release()
	body := map[string]any{"model": "model", "text": prompt, "duration_seconds": seconds}
	if seed != nil {
		body["seed"] = fmt.Sprint(*seed)
	}
	ctype, data, err := p.postJSON(ctx, "/v1/tasks/run", body)
	if err != nil {
		return nil, err
	}
	outs, err := audioOutputs(ctype, data)
	if err != nil {
		return nil, err
	}
	return outs[0].WAV, nil
}

// NativeEditSpeech changes the words of a recording, keeping the voice:
// transcript is what the recording says, target what it should say.
func NativeEditSpeech(ctx context.Context, m *hub.Model, hw *Hardware, wavPath, transcript, target string) ([]byte, error) {
	if !m.HasTask(hub.TaskEdit) {
		return nil, fmt.Errorf("%s cannot edit speech", m.Name)
	}
	markup, err := EditMarkup(transcript, target)
	if err != nil {
		return nil, err
	}
	p, release, err := acquireAudio(ctx, m, hw)
	if err != nil {
		return nil, err
	}
	defer release()
	ctype, data, err := p.postJSON(ctx, "/v1/tasks/run", map[string]any{
		"model": "model", "text": markup,
		"options":      map[string]string{"template_name": "edit"},
		"source_audio": filepath.ToSlash(wavPath),
	})
	if err != nil {
		return nil, err
	}
	outs, err := audioOutputs(ctype, data)
	if err != nil {
		return nil, err
	}
	return outs[0].WAV, nil
}

// NativeSeparate splits a 44.1 kHz WAV into stems.
func NativeSeparate(ctx context.Context, m *hub.Model, hw *Hardware, wavPath string) ([]AudioResult, error) {
	if !m.HasTask(hub.TaskSep) {
		return nil, fmt.Errorf("%s does not separate tracks", m.Name)
	}
	p, release, err := acquireAudio(ctx, m, hw)
	if err != nil {
		return nil, err
	}
	defer release()
	ctype, data, err := p.postJSON(ctx, "/v1/tasks/run", map[string]any{"model": "model", "audio": filepath.ToSlash(wavPath)})
	if err != nil {
		return nil, err
	}
	return audioOutputs(ctype, data)
}

// NativeTranscribe turns speech into text (audio.cpp ASR or whisper.cpp).
// language "" or "auto" detects it.
func NativeTranscribe(ctx context.Context, m *hub.Model, hw *Hardware, wavPath, language string) (string, error) {
	if !m.HasTask(hub.TaskASR) {
		return "", fmt.Errorf("%s does not transcribe speech", m.Name)
	}
	p, release, err := acquireAudio(ctx, m, hw)
	if err != nil {
		return "", err
	}
	defer release()
	f, err := os.Open(wavPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filepath.Base(wavPath))
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	path := "/v1/audio/transcriptions"
	if m.Engine == "whisper" {
		path = "/inference"
		mw.WriteField("response_format", "json")
		if language == "" {
			language = "auto"
		}
	} else {
		mw.WriteField("model", "model")
	}
	if language != "" && !(m.Engine != "whisper" && language == "auto") {
		mw.WriteField("language", language)
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+path, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	_, data, err := p.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", errors.New("the transcription engine returned an unreadable answer")
	}
	return strings.TrimSpace(out.Text), nil
}
