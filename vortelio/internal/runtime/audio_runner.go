package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
)

type AudioRunner struct {
	model *hub.Model
	hw    *Hardware
}

func NewAudioRunner(model *hub.Model, hw *Hardware) *AudioRunner {
	return &AudioRunner{model: model, hw: hw}
}

// hasCap reports whether the model declares the given capability.
func (r *AudioRunner) hasCap(c string) bool {
	for _, cap := range r.model.Capabilities {
		if strings.EqualFold(cap, c) {
			return true
		}
	}
	return false
}

// engine picks the inference backend from the model's name, source and declared
// capabilities — not a hardcoded model list. A known engine keyword wins; any
// other model routes by capability. Unknown TTS models fall back to a generic
// HuggingFace transformers pipeline ("generic-tts") instead of silently using
// Kokoro, so newly installed models actually run themselves.
func (r *AudioRunner) engine() string {
	hay := strings.ToLower(r.model.Name + " " + r.model.Source + " " + r.model.Tag)
	switch {
	case strings.Contains(hay, "whisper"):
		return "whisper"
	case strings.Contains(hay, "pocket"):
		return "pocket"
	case strings.Contains(hay, "bark"):
		return "bark"
	case strings.Contains(hay, "kokoro"):
		return "kokoro"
	}
	// No known keyword: a transcription-only model is treated as ASR (whisper),
	// everything else as a generic text-to-speech model.
	if r.hasCap("speech-to-text") && !r.hasCap("text-to-speech") {
		return "whisper"
	}
	return "generic-tts"
}

// ttsScript returns the synthesis script for the model's engine. Unknown
// engines use the generic transformers pipeline.
func (r *AudioRunner) ttsScript(opts *RunOptions) string {
	switch r.engine() {
	case "pocket":
		return r.buildPocketTTSScript(opts)
	case "bark":
		return r.buildBarkScript(opts)
	case "kokoro":
		return r.buildKokoroScript(opts)
	default:
		return r.buildGenericTTSScript(opts)
	}
}

// hfRepoOrPath resolves a transformers-loadable reference for the model: the
// HuggingFace "owner/name" repo when the source points at huggingface.co,
// otherwise the local model directory.
func (r *AudioRunner) hfRepoOrPath() string {
	if src := r.model.Source; strings.Contains(src, "huggingface.co/") {
		repo := src[strings.Index(src, "huggingface.co/")+len("huggingface.co/"):]
		repo = strings.TrimSuffix(repo, "/")
		parts := strings.Split(repo, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return repo
	}
	if p := r.model.LocalPath; p != "" {
		// A file (e.g. *.gguf) → use its directory; transformers loads a dir.
		if filepath.Ext(p) != "" {
			return filepath.Dir(p)
		}
		return p
	}
	return r.model.Name
}

// ensureDeps installs required Python packages for the given model if missing.
func (r *AudioRunner) ensureDeps(pythonBin string) {
	switch r.engine() {
	case "whisper":
		if !CheckPythonPackage(pythonBin, "faster_whisper") {
			fmt.Println("📦  Installing faster-whisper...")
			_ = InstallPythonPackage(pythonBin, "faster-whisper")
			if !CheckPythonPackage(pythonBin, "faster_whisper") {
				// fallback: openai-whisper
				fmt.Println("📦  Installing openai-whisper (fallback)...")
				_ = InstallPythonPackage(pythonBin, "openai-whisper")
			}
		}
	case "kokoro":
		if !CheckPythonPackage(pythonBin, "kokoro_onnx") && !CheckPythonPackage(pythonBin, "kokoro") {
			fmt.Println("📦  Installing kokoro-onnx soundfile...")
			_ = InstallPythonPackage(pythonBin, "kokoro-onnx", "soundfile")
		}
		if !CheckPythonPackage(pythonBin, "soundfile") {
			_ = InstallPythonPackage(pythonBin, "soundfile")
		}
	case "bark":
		if !CheckPythonPackage(pythonBin, "bark") {
			fmt.Println("📦  Installing bark (this may take a few minutes)...")
			_ = InstallPythonPackage(pythonBin, "git+https://github.com/suno-ai/bark.git", "scipy")
		}
		if !CheckPythonPackage(pythonBin, "torch") {
			fmt.Println("📦  Installing torch...")
			_ = InstallPythonPackage(pythonBin, "torch")
		}
	case "pocket":
		if !CheckPythonPackage(pythonBin, "pocket_tts") {
			fmt.Println("📦  Installing pocket-tts scipy...")
			_ = InstallPythonPackage(pythonBin, "pocket-tts", "scipy")
		}
		if !CheckPythonPackage(pythonBin, "scipy") {
			_ = InstallPythonPackage(pythonBin, "scipy")
		}
	case "generic-tts":
		// Best-effort runtime for any HuggingFace text-to-speech model.
		if !CheckPythonPackage(pythonBin, "transformers") {
			fmt.Println("📦  Installing transformers (generic TTS)...")
			_ = InstallPythonPackage(pythonBin, "transformers", "sentencepiece")
		}
		if !CheckPythonPackage(pythonBin, "torch") {
			fmt.Println("📦  Installing torch...")
			_ = InstallPythonPackage(pythonBin, "torch")
		}
		if !CheckPythonPackage(pythonBin, "soundfile") {
			_ = InstallPythonPackage(pythonBin, "soundfile")
		}
	}
}

func (r *AudioRunner) Run(opts *RunOptions) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("Python 3 not found — needed for audio models\n  Install Python 3.10+ from https://python.org/downloads")
	}
	r.ensureDeps(pythonBin)
	// An attached audio file always means speech-to-text.
	if opts.InputFile != "" {
		return r.runPython(pythonBin, r.buildWhisperScript(opts))
	}
	if r.engine() == "whisper" {
		return fmt.Errorf("Whisper transcribes speech → text, so it needs an audio file\n  Example: vortelio run audio/whisper:large --input ./audio.mp3")
	}
	return r.runPython(pythonBin, r.ttsScript(opts))
}

func (r *AudioRunner) RunCapture(opts *RunOptions) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("python3 not found — install Python 3.10+")
	}
	r.ensureDeps(pythonBin)
	var script string
	if r.engine() == "whisper" {
		script = r.buildWhisperScript(opts)
	} else {
		script = r.ttsScript(opts)
	}
	return r.runPythonWith(pythonBin, script, true)
}

func (r *AudioRunner) RunWithProgress(opts *RunOptions, progress chan<- ProgressEvent) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		if progress != nil {
			close(progress)
		}
		return fmt.Errorf("python3 not found")
	}
	r.ensureDeps(pythonBin)
	var script string
	if r.engine() == "whisper" {
		script = r.buildWhisperScript(opts)
	} else {
		script = r.ttsScript(opts)
	}
	tmp, err := os.CreateTemp("", "vortelio-audio-*.py")
	if err != nil {
		if progress != nil {
			close(progress)
		}
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	return RunWithProgress(cmd, progress)
}

func (r *AudioRunner) runPython(pythonBin, script string) error {
	return r.runPythonWith(pythonBin, script, false)
}

func (r *AudioRunner) runPythonWith(pythonBin, script string, capture bool) error {
	tmp, err := os.CreateTemp("", "vortelio-audio-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	if capture {
		return RunWithCapture(cmd)
	}
	return RunWithOutput(cmd, os.Stdout, os.Stderr)
}

func (r *AudioRunner) deviceString(forceCPU bool) string {
	if forceCPU {
		return "cpu"
	}
	switch r.hw.Backend {
	case BackendCUDA:
		return "cuda"
	case BackendMetal:
		return "mps"
	default:
		return "cpu"
	}
}

func (r *AudioRunner) buildKokoroScript(opts *RunOptions) string {
	text := opts.Prompt
	if text == "" {
		text = "Hi, I'm Vortelio."
	}
	outputPath := opts.OutputFile
	if outputPath == "" {
		outputPath = ResolveOutputPath("", "output.wav")
	}
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)
	device := r.deviceString(opts.ForceCPU)

	lines := []string{
		`import sys, os, pathlib, urllib.request`,
		`os.environ["PYTHONIOENCODING"] = "utf-8"`,
		``,
		`output_path = """` + outputPath + `"""`,
		`device = "` + device + `"`,
		`text = """` + escapePy(text) + `"""`,
		``,
		`# Try kokoro-onnx first (Python 3.14 compatible, no espeak-ng needed)`,
		`_lib = None`,
		`try:`,
		`    import kokoro_onnx`,
		`    _lib = "kokoro-onnx"`,
		`except ImportError:`,
		`    pass`,
		``,
		`# Fallback: kokoro (requires Python <= 3.12 and espeak-ng)`,
		`if _lib is None:`,
		`    try:`,
		`        from kokoro import KPipeline`,
		`        _lib = "kokoro"`,
		`    except ImportError:`,
		`        pass`,
		``,
		`if _lib is None:`,
		`    print("ERROR: install kokoro with:")`,
		`    print("  pip install kokoro-onnx soundfile  (recommended, Python 3.14)")`,
		`    print("  pip install kokoro soundfile       (requires Python <= 3.12)")`,
		`    sys.exit(1)`,
		``,
		`import soundfile as sf`,
		`import numpy as np`,
		``,
		`def _download_if_missing(url, dest):`,
		`    dest = pathlib.Path(dest)`,
		`    if not dest.exists():`,
		`        dest.parent.mkdir(parents=True, exist_ok=True)`,
		`        print("Download: " + dest.name + " ...")`,
		`        urllib.request.urlretrieve(url, dest)`,
		`    return str(dest)`,
		``,
		`_KOKORO_BASE = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0"`,
		`_cache = pathlib.Path.home() / ".vortelio" / "models" / "audio" / "kokoro"`,
		``,
		`if _lib == "kokoro-onnx":`,
		`    print("Loading Kokoro ONNX...")`,
		`    model_path  = _download_if_missing(_KOKORO_BASE + "/kokoro-v1.0.onnx",  _cache / "kokoro-v1.0.onnx")`,
		`    voices_path = _download_if_missing(_KOKORO_BASE + "/voices-v1.0.bin",   _cache / "voices-v1.0.bin")`,
		`    tts = kokoro_onnx.Kokoro(model_path, voices_path)`,
		`    samples, sr = tts.create(text, voice="af_heart", speed=1.0, lang="en-us")`,
		`    sf.write(output_path, samples, sr)`,
		`else:`,
		`    print("Loading Kokoro on " + device + "...")`,
		`    pipe = KPipeline(lang_code="a", device=device)`,
		`    samples = []`,
		`    for i, (gs, ps, audio) in enumerate(pipe(text, voice="af_heart", speed=1.0)):`,
		`        samples.append(audio)`,
		`    audio = np.concatenate(samples)`,
		`    sf.write(output_path, audio, 24000)`,
		``,
		`print("Audio saved to: " + output_path)`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func (r *AudioRunner) buildWhisperScript(opts *RunOptions) string {
	inputPath := strings.ReplaceAll(opts.InputFile, `\`, `/`)
	device := r.deviceString(opts.ForceCPU)
	tag := r.model.Tag

	lines := []string{
		`import sys, os`,
		`os.environ["PYTHONIOENCODING"] = "utf-8"`,
		``,
		`_lib = None`,
		`try:`,
		`    from faster_whisper import WhisperModel as _FW`,
		`    _lib = "faster-whisper"`,
		`except ImportError:`,
		`    pass`,
		``,
		`if _lib is None:`,
		`    try:`,
		`        import whisper as _W`,
		`        _lib = "openai-whisper"`,
		`    except Exception:`,
		`        pass`,
		``,
		`if _lib is None:`,
		`    print("ERROR: pip install faster-whisper")`,
		`    sys.exit(1)`,
		``,
		`try:`,
		`    import torch as _t`,
		`    _cuda_ok = _t.cuda.is_available()`,
		`    del _t`,
		`except ImportError:`,
		`    _cuda_ok = False`,
		``,
		`_dev = "cuda" if _cuda_ok and "` + device + `" != "cpu" else "cpu"`,
		``,
		`_tag_map = {`,
		`    "large": "large-v3", "large-v3": "large-v3", "large-v2": "large-v2",`,
		`    "large-v1": "large-v1", "medium": "medium", "small": "small",`,
		`    "base": "base", "tiny": "tiny", "turbo": "large-v3-turbo",`,
		`    "large-v3-turbo": "large-v3-turbo", "distil-large-v3": "distil-large-v3",`,
		`}`,
		`import re as _re`,
		`_base = _re.split(r"[/\\]", "` + tag + `")[-1].lower().strip()`,
		`_fw_model = _tag_map.get(_base, _tag_map.get("` + tag + `".lower(), "large-v3"))`,
		``,
		`if _lib == "faster-whisper":`,
		`    _ct = "float16" if _dev == "cuda" else "int8"`,
		`    print("Loading: " + _fw_model + " (" + _ct + ")", file=sys.stderr)`,
		`    _m = _FW(_fw_model, device=_dev, compute_type=_ct)`,
		`    _segs, _ = _m.transcribe("""` + inputPath + `""", beam_size=5)`,
		`    _text = "".join(seg.text for seg in _segs)`,
		`else:`,
		`    _m = _W.load_model(_fw_model, device=_dev)`,
		`    _res = _W.transcribe(_m, """` + inputPath + `""", fp16=(_dev != "cpu"))`,
		`    _text = _res["text"]`,
		``,
		`print(_text)`,
	}
	script := strings.Join(lines, "\n") + "\n"
	if opts.OutputFile != "" {
		outPath := strings.ReplaceAll(ResolveOutputPath(opts.OutputFile, ""), `\`, `/`)
		script += "\nwith open(\"\"\"" + outPath + "\"\"\", \"w\", encoding=\"utf-8\") as f:\n"
		script += "    f.write(_text)\n"
		script += "print(\"Saved to: " + outPath + "\")\n"
	}
	return script
}

// TranscribeText runs Whisper on inputFile and returns the transcribed text.
func (r *AudioRunner) TranscribeText(inputFile string) (string, error) {
	pythonBin := FindPython()
	if pythonBin == "" {
		return "", fmt.Errorf("python3 not found")
	}
	r.ensureDeps(pythonBin)
	opts := &RunOptions{InputFile: inputFile}
	script := r.buildWhisperScript(opts)
	tmp, err := os.CreateTemp("", "vortelio-whisper-*.py")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	out, err := cmd.Output()
	if err != nil {
		// Surface the Python stderr so the real cause is visible (missing ffmpeg,
		// package not installed, model download failure, …) instead of "exit 1".
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if detail == "" {
			detail = err.Error()
		}
		if len(detail) > 500 {
			detail = "…" + detail[len(detail)-500:]
		}
		return "", fmt.Errorf("transcription failed: %s", detail)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// The last non-empty line is the transcribed text
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "Loading") && !strings.HasPrefix(l, "Saved") {
			return l, nil
		}
	}
	return strings.TrimSpace(string(out)), nil
}

// TranslateText runs Whisper translate task on inputFile and returns English text.
func (r *AudioRunner) TranslateText(inputFile string) (string, error) {
	pythonBin := FindPython()
	if pythonBin == "" {
		return "", fmt.Errorf("python3 not found")
	}
	r.ensureDeps(pythonBin)
	inputPath := strings.ReplaceAll(inputFile, `\`, `/`)
	tag := r.model.Tag
	script := fmt.Sprintf(`import sys, os
os.environ["PYTHONIOENCODING"] = "utf-8"
try:
    from faster_whisper import WhisperModel as _FW
    _lib = "faster-whisper"
except ImportError:
    try:
        import whisper as _W
        _lib = "openai-whisper"
    except Exception:
        print("ERROR: pip install faster-whisper")
        sys.exit(1)
_tag_map = {"large":"large-v3","medium":"medium","small":"small","base":"base","tiny":"tiny","turbo":"large-v3-turbo"}
_fw_model = _tag_map.get("%s", "large-v3")
if _lib == "faster-whisper":
    _m = _FW(_fw_model, device="cpu", compute_type="int8")
    _segs, _ = _m.transcribe("""%s""", task="translate", beam_size=5)
    print("".join(seg.text for seg in _segs))
else:
    _m = _W.load_model(_fw_model, device="cpu")
    print(_W.transcribe(_m, """%s""", task="translate")["text"])
`, tag, inputPath, inputPath)
	tmp, err := os.CreateTemp("", "vortelio-whisper-tr-*.py")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("translation failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SynthesizeToBytes runs TTS on text and returns the WAV bytes.
func (r *AudioRunner) SynthesizeToBytes(text string) ([]byte, error) {
	pythonBin := FindPython()
	if pythonBin == "" {
		return nil, fmt.Errorf("python3 not found")
	}
	r.ensureDeps(pythonBin)
	tmp, err := os.CreateTemp("", "vortelio-tts-*.wav")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	opts := &RunOptions{Prompt: text, OutputFile: tmpPath}
	script := r.ttsScript(opts)
	if err := r.runPython(pythonBin, script); err != nil {
		return nil, fmt.Errorf("synthesis failed: %w", err)
	}
	return os.ReadFile(tmpPath)
}

func (r *AudioRunner) buildBarkScript(opts *RunOptions) string {
	text := opts.Prompt
	if text == "" {
		text = "Hello."
	}
	outputPath := opts.OutputFile
	if outputPath == "" {
		outputPath = ResolveOutputPath("", "output.wav")
	}
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)
	device := r.deviceString(opts.ForceCPU)
	return fmt.Sprintf(`import sys, os
os.environ["PYTHONIOENCODING"] = "utf-8"
os.environ['SUNO_OFFLOAD_CPU'] = '1' if '%s' == 'cpu' else '0'
try:
    import torch as _torch
    # PyTorch 2.6+ compat: Bark uses weights_only=False semantics
    _orig_torch_load = _torch.load
    def _bark_torch_load(*a, **kw):
        kw.setdefault('weights_only', False)
        return _orig_torch_load(*a, **kw)
    _torch.load = _bark_torch_load
    from bark import SAMPLE_RATE, generate_audio, preload_models
    import scipy.io.wavfile as wav
    import numpy as np
except ImportError:
    print('Bark not installed. pip install git+https://github.com/suno-ai/bark.git scipy')
    sys.exit(1)
preload_models()
audio = generate_audio('''%s''')
wav.write(r'''%s''', SAMPLE_RATE, audio.astype(np.float32))
print('Audio saved to: ' + r'''%s''')
`, device, escapePy(text), outputPath, outputPath)
}

// buildPocketTTSScript renders the Python that drives kyutai's pocket-tts.
// The GGUF/CoreML weights pulled into ~/.vortelio are WASM/CoreML-only; the
// pip `pocket-tts` package downloads its own PyTorch weights on first run.
func (r *AudioRunner) buildPocketTTSScript(opts *RunOptions) string {
	text := opts.Prompt
	if text == "" {
		text = "Hi, I'm Vortelio."
	}
	outputPath := opts.OutputFile
	if outputPath == "" {
		outputPath = ResolveOutputPath("", "output.wav")
	}
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)

	// pocket-tts voices: en=alba, it=...; default to alba (English).
	voice := "alba"

	lines := []string{
		`import sys, os`,
		`os.environ["PYTHONIOENCODING"] = "utf-8"`,
		``,
		`output_path = """` + outputPath + `"""`,
		`text = """` + escapePy(text) + `"""`,
		`voice = "` + voice + `"`,
		``,
		`try:`,
		`    from pocket_tts import TTSModel`,
		`    import scipy.io.wavfile as wav`,
		`except ImportError:`,
		`    print("ERROR: pocket-tts not installed. pip install pocket-tts scipy")`,
		`    sys.exit(1)`,
		``,
		`print("Loading pocket-tts...")`,
		`model = TTSModel.load_model()`,
		`state = model.get_state_for_audio_prompt(voice)`,
		`print("Generating audio...")`,
		`audio = model.generate_audio(state, text)`,
		`try:`,
		`    audio = audio.detach().cpu().numpy()`,
		`except AttributeError:`,
		`    pass`,
		`wav.write(output_path, int(model.sample_rate), audio)`,
		`print("Audio saved to: " + output_path)`,
	}
	return strings.Join(lines, "\n") + "\n"
}

// buildGenericTTSScript is the fallback for any installed text-to-speech model
// without a dedicated engine: it drives a HuggingFace transformers
// `pipeline("text-to-speech", ...)`, which auto-resolves the right architecture
// (VITS/MMS, SpeechT5, Bark, Parler, etc.) for most HF TTS models.
func (r *AudioRunner) buildGenericTTSScript(opts *RunOptions) string {
	text := opts.Prompt
	if text == "" {
		text = "Hi, I'm Vortelio."
	}
	outputPath := opts.OutputFile
	if outputPath == "" {
		outputPath = ResolveOutputPath("", "output.wav")
	}
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)
	modelRef := strings.ReplaceAll(r.hfRepoOrPath(), `\`, `/`)
	device := r.deviceString(opts.ForceCPU)

	lines := []string{
		`import sys, os`,
		`os.environ["PYTHONIOENCODING"] = "utf-8"`,
		``,
		`output_path = """` + outputPath + `"""`,
		`text = """` + escapePy(text) + `"""`,
		`model_ref = r"""` + modelRef + `"""`,
		`device = "` + device + `"`,
		``,
		`try:`,
		`    import torch`,
		`    from transformers import pipeline`,
		`    import soundfile as sf`,
		`    import numpy as np`,
		`except ImportError:`,
		`    print("ERROR: generic TTS needs: pip install transformers torch soundfile sentencepiece")`,
		`    sys.exit(1)`,
		``,
		`dev = 0 if (device == "cuda" and torch.cuda.is_available()) else -1`,
		`print("Loading TTS model: " + model_ref)`,
		`try:`,
		`    synth = pipeline("text-to-speech", model=model_ref, device=dev)`,
		`except Exception as e:`,
		`    print("ERROR: this model is not supported by the transformers TTS pipeline: " + str(e))`,
		`    print("       (formats like GGUF/CoreML need a dedicated engine)")`,
		`    sys.exit(1)`,
		`print("Generating audio...")`,
		`out = synth(text)`,
		`audio = np.asarray(out["audio"]).squeeze()`,
		`sr = int(out.get("sampling_rate", 16000))`,
		`sf.write(output_path, audio, sr)`,
		`print("Audio saved to: " + output_path)`,
	}
	return strings.Join(lines, "\n") + "\n"
}
