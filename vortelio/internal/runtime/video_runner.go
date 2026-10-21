package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
)

// VideoRunner generates video from text prompts.
type VideoRunner struct {
	model *hub.Model
	hw    *Hardware
}

func NewVideoRunner(model *hub.Model, hw *Hardware) *VideoRunner {
	return &VideoRunner{model: model, hw: hw}
}

// native reports whether the model runs on stable-diffusion.cpp.
func (r *VideoRunner) native() bool {
	f := hub.ImageFamilyOf(r.model)
	return f != nil && f.Kind == "video" && strings.HasSuffix(strings.ToLower(r.model.LocalPath), ".gguf")
}

// runNative generates with stable-diffusion.cpp and writes opts.OutputFile
// (its extension follows the result: .mp4, or .gif without ffmpeg).
func (r *VideoRunner) runNative(opts *RunOptions, emit func(int, string)) error {
	res, err := NativeGenerateVideo(context.Background(), r.model, r.hw, VideoRequest{
		Prompt: opts.Prompt, Steps: opts.Steps, ForceCPU: opts.ForceCPU,
	}, func(pct int, msg string) {
		if emit != nil {
			emit(pct, msg)
		} else {
			fmt.Printf("\r    %3d%%  %-40s", pct, msg)
		}
	})
	if emit == nil {
		fmt.Println()
	}
	if err != nil {
		return err
	}
	out := ResolveOutputPath(opts.OutputFile, "output."+res.Ext)
	if ext := filepath.Ext(out); !strings.EqualFold(ext, "."+res.Ext) && emit == nil {
		out = strings.TrimSuffix(out, ext) + "." + res.Ext
	}
	if err := os.WriteFile(out, res.Data, 0o644); err != nil {
		return err
	}
	if emit == nil {
		fmt.Printf("✅  Video saved: %s (%d frames, seed %d)\n", out, res.Frames, res.Seed)
	}
	return nil
}

func (r *VideoRunner) Run(opts *RunOptions) error {
	if r.native() {
		if opts.Prompt == "" {
			return fmt.Errorf("a text prompt is required for video generation\n  Example: vortelio run video/wan2.1:1.3b \"a flying cat\"")
		}
		return r.runNative(opts, nil)
	}
	AcquireGPU("video") // free VRAM held by a chat model
	if opts.Prompt == "" {
		return fmt.Errorf("a text prompt is required for video generation\n  Example: vortelio run video/wan:1.3b \"a flying cat\"")
	}

	output := opts.OutputFile
	if output == "" {
		output = "output.mp4"
	}

	name := strings.ToLower(r.model.Name)
	fmt2 := strings.ToLower(r.model.Format)
	local := strings.ToLower(r.model.LocalPath)

	fmt.Printf("🎬  Generating video (%d steps)\n", opts.Steps)
	fmt.Printf("    Prompt: %q\n", opts.Prompt)
	fmt.Printf("    Output: %s\n\n", output)

	var script string
	switch {
	case strings.Contains(name, "cogvideo") || strings.Contains(name, "cog"):
		script = r.cogVideoScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)

	case strings.Contains(name, "wan") || strings.Contains(local, "wan"):
		// Wan 2.1 — supports both GGUF and safetensors
		if fmt2 == "gguf" || strings.HasSuffix(local, ".gguf") {
			script = r.wanGGUFScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		} else {
			script = r.wanScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		}

	case strings.Contains(name, "hunyuan") || strings.Contains(name, "ltx") ||
		strings.Contains(name, "mochi"):
		script = r.hunyuanScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)

	case strings.Contains(name, "animatediff") || strings.Contains(name, "animate-diff"):
		script = r.animateDiffScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)

	default:
		// Unknown model: try to detect from format
		if fmt2 == "gguf" || strings.HasSuffix(local, ".gguf") {
			script = r.wanGGUFScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		} else {
			// Generic diffusers auto-pipeline — works for any installed
			// text-to-video model with a model_index.json.
			script = r.genericVideoScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		}
	}

	return r.runPythonScriptProg(script, nil)
}

func (r *VideoRunner) cogVideoScript(prompt, output string, steps int, forceCPU bool) string {
	device := r.deviceString(forceCPU)
	dtype := "torch.float16"
	if device == "cpu" {
		dtype = "torch.float32"
	}
	modelDir := r.modelDir()

	return fmt.Sprintf(`
import sys
try:
    from diffusers import CogVideoXPipeline
    import torch
except ImportError:
    import subprocess, shutil
    pip_cmds = [
        [sys.executable, "-m", "pip", "install", "--quiet", "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"],
        [sys.executable, "-m", "pip", "install", "--quiet", "--break-system-packages", "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"],
    ]
    uv = shutil.which("uv")
    if uv: pip_cmds.append([uv, "pip", "install", "--quiet", "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"])
    ok = any(subprocess.run(cmd, capture_output=True).returncode == 0 for cmd in pip_cmds)
    if not ok:
        print("ERROR: could not install diffusers. Install manually: pip install diffusers torch")
        sys.exit(1)

import imageio, numpy as np, os

device = "%s"
model_path = "%s"
print(f"Loading CogVideoX from {model_path}...")

pipe = CogVideoXPipeline.from_pretrained(
    model_path,
    torch_dtype=%s,
)
pipe = pipe.to(device)
pipe.enable_model_cpu_offload()
pipe.enable_sequential_cpu_offload()
pipe.vae.enable_slicing()
pipe.vae.enable_tiling()

print("Generating video (this may take several minutes)...")
video = pipe(
    prompt="%s",
    num_video_frames=49,
    num_inference_steps=%d,
    guidance_scale=6.0,
).frames[0]

# Export as MP4
frames = [(f * 255).clip(0,255).astype("uint8") for f in video]
imageio.mimsave(r"""%s""", frames, fps=8, codec="libx264")
print("\\n✅  Saved to: " + r"""%s""")
`,
		device, modelDir, dtype,
		escapePy(prompt), steps,
		output, output,
	)
}

func (r *VideoRunner) animateDiffScript(prompt, output string, steps int, forceCPU bool) string {
	device := r.deviceString(forceCPU)
	dtype := "torch.float16"
	if device == "cpu" {
		dtype = "torch.float32"
	}
	// Use local model directory
	modelDir := strings.ReplaceAll(filepath.Dir(r.model.LocalPath), `\`, `/`)

	return fmt.Sprintf(`
import sys, os
try:
    from diffusers import AnimateDiffPipeline, MotionAdapter, EulerDiscreteScheduler
    from diffusers.utils import export_to_video
    import torch
except ImportError:
    print("VORTELIO_PROGRESS:0:Installing diffusers...")
    import subprocess
    # Try normal pip first, then --break-system-packages, then uv
    pip_ok = subprocess.run([sys.executable, "-m", "pip", "install", "--quiet",
        "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"],
        capture_output=True).returncode == 0
    if not pip_ok:
        pip_ok = subprocess.run([sys.executable, "-m", "pip", "install", "--quiet",
            "--break-system-packages",
            "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"],
            capture_output=True).returncode == 0
    if not pip_ok:
        import shutil
        uv = shutil.which("uv")
        if uv:
            subprocess.run([uv, "pip", "install", "--quiet",
                "diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg"])
    from diffusers import AnimateDiffPipeline, MotionAdapter, EulerDiscreteScheduler
    from diffusers.utils import export_to_video
    import torch

print("VORTELIO_PROGRESS:5:Controllo file modello...")
model_path = r"""%s"""

# Auto-download config.json if missing (happens with models installed in old pullai versions)
config_path = os.path.join(model_path, "config.json")
if not os.path.exists(config_path):
    print("VORTELIO_PROGRESS:8:Download config.json mancante...")
    try:
        from huggingface_hub import hf_hub_download
        for fname in ["config.json", "diffusion_pytorch_model.safetensors.index.json"]:
            try:
                local = hf_hub_download(
                    repo_id="guoyww/animatediff-motion-adapter-v1-5-3",
                    filename=fname, local_dir=model_path
                )
                print(f"  downloaded: {fname}")
            except Exception:
                pass
    except ImportError:
        pass

print("VORTELIO_PROGRESS:10:Loading adapter...")
adapter = MotionAdapter.from_pretrained(model_path, torch_dtype=%s)
print("VORTELIO_PROGRESS:30:Creazione pipeline...")
pipe = AnimateDiffPipeline.from_pretrained(
    "emilianJR/epiCRealism",
    motion_adapter=adapter,
    torch_dtype=%s,
)
pipe = pipe.to("%s")
print("VORTELIO_PROGRESS:50:Generating video...")
frames = pipe(
    prompt=r"""%s""",
    num_frames=16,
    num_inference_steps=%d,
    guidance_scale=7.5,
).frames[0]
print("VORTELIO_PROGRESS:90:Saving...")
export_to_video(frames, r"""%s""", fps=8)
print("VORTELIO_DONE:" + r"""%s""")
print("VORTELIO_PROGRESS:100:Done!")
`,
		modelDir, dtype, dtype, device,
		escapePy(prompt), steps,
		output, output,
	)
}

// genericVideoScript is the fallback for any installed text-to-video model that
// has a diffusers layout (model_index.json). DiffusionPipeline.from_pretrained
// auto-resolves the concrete pipeline class (CogVideoX, LTX, Mochi, Wan,
// Hunyuan, AnimateDiff, …) so a newly installed model runs itself instead of
// being forced through a hardcoded AnimateDiff base model.
func (r *VideoRunner) genericVideoScript(prompt, output string, steps int, forceCPU bool) string {
	device := r.deviceString(forceCPU)
	dtype := "torch.float16"
	if device == "cpu" {
		dtype = "torch.float32"
	}
	modelDir := r.modelDir()
	outputFwd := strings.ReplaceAll(output, `\`, `/`)

	return fmt.Sprintf(`import sys, os, subprocess, shutil
os.environ["PYTHONIOENCODING"] = "utf-8"
model_dir = r"""%s"""
prompt = r"""%s"""
output_path = r"""%s"""
device = "%s"
steps = %d

print("VORTELIO_PROGRESS:5:Checking dependencies...")
def pip_run(*args):
    strategies = [
        [sys.executable, "-m", "pip", "install", "-q"] + list(args),
        [sys.executable, "-m", "pip", "install", "-q", "--break-system-packages"] + list(args),
    ]
    uv = shutil.which("uv")
    if uv:
        strategies.append([uv, "pip", "install", "-q"] + list(args))
    for cmd in strategies:
        if subprocess.run(cmd, capture_output=True).returncode == 0:
            return True
    return False

try:
    import torch
    from diffusers import DiffusionPipeline
    from diffusers.utils import export_to_video
except ImportError:
    print("VORTELIO_PROGRESS:8:Installing diffusers...")
    pip_run("diffusers[torch]", "transformers", "accelerate", "imageio", "imageio-ffmpeg", "sentencepiece")
    import torch
    from diffusers import DiffusionPipeline
    from diffusers.utils import export_to_video

print("VORTELIO_PROGRESS:20:Loading model (auto-pipeline)...")
try:
    pipe = DiffusionPipeline.from_pretrained(model_dir, torch_dtype=%s)
except Exception as e:
    print("ERROR: this video model could not be loaded by diffusers: " + str(e))
    print("       (a model_index.json describing the pipeline is required)")
    sys.exit(1)
pipe = pipe.to(device)
try:
    pipe.enable_attention_slicing()
except Exception:
    pass
try:
    pipe.enable_model_cpu_offload()
except Exception:
    pass

print("VORTELIO_PROGRESS:50:Generating video...")
out = pipe(prompt=prompt, num_inference_steps=steps)
frames = getattr(out, "frames", None)
if frames is None:
    frames = getattr(out, "images", None)
if frames is None:
    print("ERROR: model produced no frames")
    sys.exit(1)
# Diffusers video pipelines usually return a batch: frames[0] is the clip.
if isinstance(frames, (list, tuple)) and len(frames) and isinstance(frames[0], (list, tuple)):
    frames = frames[0]
print("VORTELIO_PROGRESS:90:Saving...")
export_to_video(frames, output_path, fps=8)
print("VORTELIO_DONE:" + output_path)
print("VORTELIO_PROGRESS:100:Done!")
`,
		modelDir, escapePy(prompt), outputFwd, device, steps, dtype,
	)
}

func (r *VideoRunner) modelDir() string {
	localPath := r.model.LocalPath
	// If LocalPath is a file (config.json, .safetensors, etc.), start from its directory
	if fi, err := os.Stat(localPath); err == nil && !fi.IsDir() {
		localPath = filepath.Dir(localPath)
	}
	dir := strings.ReplaceAll(localPath, `\`, `/`)
	// Walk up to find model_index.json (diffusers) or config.json (transformers)
	for {
		if _, err := os.Stat(filepath.Join(dir, "model_index.json")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return strings.ReplaceAll(filepath.Dir(r.model.LocalPath), `\`, `/`)
}

func (r *VideoRunner) deviceString(forceCPU bool) string {
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

func (r *VideoRunner) runPythonScriptProg(script string, onProgress func(ProgressEvent)) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("python3 not found.\n\nInstall Python 3.10+ from https://python.org/downloads\n" +
			"Make sure to check \"Add Python to PATH\" during installation")
	}

	tmp, err := os.CreateTemp("", "vortelio-video-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()

	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	if onProgress != nil {
		ch := make(chan ProgressEvent, 32)
		go func() {
			for ev := range ch {
				onProgress(ev)
			}
		}()
		return RunWithProgress(cmd, ch)
	}
	return RunWithOutput(cmd, os.Stdout, os.Stderr)
}

// RunWithProgress for video — streams VORTELIO_PROGRESS lines as channel events
func (r *VideoRunner) RunWithProgress(opts *RunOptions, progress chan<- ProgressEvent) error {
	if r.native() && progress != nil {
		defer close(progress)
		if opts.Prompt == "" {
			return fmt.Errorf("a text prompt is required for video generation")
		}
		return r.runNative(opts, func(pct int, msg string) {
			select {
			case progress <- ProgressEvent{Percent: pct, Message: msg}:
			default:
			}
		})
	}
	AcquireGPU("video") // free VRAM held by a chat model
	if opts.Prompt == "" {
		if progress != nil {
			close(progress)
		}
		return fmt.Errorf("a text prompt is required for video generation")
	}
	output := opts.OutputFile
	if output == "" {
		output = "output.mp4"
	}
	name := strings.ToLower(r.model.Name)
	local := strings.ToLower(r.model.LocalPath)
	fmt2 := strings.ToLower(r.model.Format)
	var script string
	switch {
	case strings.Contains(name, "cogvideo") || strings.Contains(name, "cog"):
		script = r.cogVideoScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
	case strings.Contains(name, "wan") || strings.Contains(local, "wan"):
		if fmt2 == "gguf" || strings.HasSuffix(local, ".gguf") {
			script = r.wanGGUFScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		} else {
			script = r.wanScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
		}
	case strings.Contains(name, "hunyuan") || strings.Contains(name, "ltx") || strings.Contains(name, "mochi"):
		script = r.hunyuanScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
	case strings.Contains(name, "animatediff") || strings.Contains(name, "animate-diff"):
		script = r.animateDiffScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
	case fmt2 == "gguf" || strings.HasSuffix(local, ".gguf"):
		script = r.wanGGUFScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
	default:
		script = r.genericVideoScript(opts.Prompt, output, opts.Steps, opts.ForceCPU)
	}
	pythonBin := FindPython()
	if pythonBin == "" {
		if progress != nil {
			close(progress)
		}
		return fmt.Errorf("python3 not found")
	}
	tmp, err := os.CreateTemp("", "vortelio-video-*.py")
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

// wanGGUFScript runs Wan 2.1 T2V from a GGUF file using the "wan" Python package
func (r *VideoRunner) wanGGUFScript(prompt, output string, steps int, forceCPU bool) string {
	// Wan 2.1 GGUF requires diffusers WanPipeline (diffusers >= 0.33.0)
	// The model directory should contain the pipeline files
	modelDir := r.modelDir()
	device := r.deviceString(forceCPU)
	outputFwd := strings.ReplaceAll(output, `\`, `/`)

	return fmt.Sprintf(`import sys, os, subprocess, shutil
os.environ["PYTHONIOENCODING"] = "utf-8"
model_dir = r"""%s"""
prompt = r"""%s"""
output_path = r"""%s"""
device = "%s"
steps = %d

print("VORTELIO_PROGRESS:5:Checking dependencies...")

def pip_run(*args):
    """Try multiple pip invocation strategies."""
    strategies = [
        [sys.executable, "-m", "pip", "install", "-q"] + list(args),
        [sys.executable, "-m", "pip", "install", "-q", "--break-system-packages"] + list(args),
    ]
    uv = shutil.which("uv")
    if uv:
        strategies.append([uv, "pip", "install", "-q"] + list(args))
    for cmd in strategies:
        if subprocess.run(cmd, capture_output=True).returncode == 0:
            return True
    return False

# Ensure diffusers >= 0.33.0 for WanPipeline
try:
    import diffusers
    from packaging.version import Version
    if Version(diffusers.__version__) < Version("0.33.0"):
        print("VORTELIO_PROGRESS:8:Aggiornamento diffusers...")
        pip_run("diffusers[torch]>=0.33.0", "transformers", "accelerate")
        import importlib; importlib.reload(diffusers)
except ImportError:
    print("VORTELIO_PROGRESS:8:Installing diffusers...")
    pip_run("diffusers[torch]>=0.33.0", "transformers", "accelerate")

# Check if model_dir has the full diffusers structure
# If it only contains a .gguf file, auto-download the full model
model_index = os.path.join(model_dir, "model_index.json")
if not os.path.exists(model_index):
    print("VORTELIO_PROGRESS:12:Downloading full Wan 2.1 model (diffusers files missing)...")
    # Find if there's a .gguf file to determine the size (1.3B vs 14B)
    gguf_files = [f for f in os.listdir(model_dir) if f.endswith(".gguf")]
    repo_id = "Wan-AI/Wan2.1-T2V-14B-Diffusers"
    if any("1.3b" in f.lower() or "1_3b" in f.lower() for f in gguf_files):
        repo_id = "Wan-AI/Wan2.1-T2V-1.3B-Diffusers"
    print(f"VORTELIO_PROGRESS:13:Download da {repo_id}...")
    try:
        import os as _os
        # Prevent HuggingFace from caching in ~/.cache/huggingface (saves disk space)
        _os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS_WARNING", "1")
        from huggingface_hub import snapshot_download
        model_dir = snapshot_download(
            repo_id=repo_id,
            local_dir=model_dir,
            local_dir_use_symlinks=False,  # copy files directly, no symlinks
            ignore_patterns=["*.gguf", "*.bin", "flax_*", "tf_*", "*.msgpack"],
        )
        print(f"VORTELIO_PROGRESS:45:Download complete!")
    except Exception as e:
        print(f"VORTELIO_ERROR:Automatic download failed: {e}\nRe-download the model with: vortelio pull video/wan:1.3b")
        sys.exit(1)

print("VORTELIO_PROGRESS:50:Loading WanPipeline...")
try:
    from diffusers import AutoencoderKLWan, WanPipeline
    from diffusers.schedulers.scheduling_unipc_multistep import UniPCMultistepScheduler
    import torch
    dtype = torch.bfloat16 if device == "cuda" else torch.float32
    pipe = WanPipeline.from_pretrained(model_dir, torch_dtype=dtype)
    if device == "cuda":
        pipe.enable_model_cpu_offload()
        pipe.vae.enable_slicing()
    print("VORTELIO_PROGRESS:50:Generating video...")
    output_frames = pipe(
        prompt=prompt,
        negative_prompt="blurry, low quality, watermark",
        height=480, width=832,
        num_frames=49, num_inference_steps=steps,
        guidance_scale=5.0,
    ).frames[0]
    print("VORTELIO_PROGRESS:90:Saving video...")
    from diffusers.utils import export_to_video
    export_to_video(output_frames, output_path, fps=16)
    print("VORTELIO_DONE:" + output_path)
    print("VORTELIO_PROGRESS:100:Done!")
except AttributeError:
    print("VORTELIO_ERROR:WanPipeline not available in this diffusers version.\nRun: pip install diffusers>=0.33.0")
    sys.exit(1)
except Exception as e:
    print(f"VORTELIO_ERROR:{e}")
    sys.exit(1)
`, modelDir, escapePy(prompt), outputFwd, device, steps)
}

// wanScript runs Wan 2.1 from safetensors via diffusers WanPipeline
func (r *VideoRunner) wanScript(prompt, output string, steps int, forceCPU bool) string {
	modelDir := strings.ReplaceAll(r.modelDir(), `\`, `/`)
	device := r.deviceString(forceCPU)
	outputFwd := strings.ReplaceAll(output, `\`, `/`)

	return fmt.Sprintf(`import sys, os, subprocess, shutil
os.environ["PYTHONIOENCODING"] = "utf-8"
model_dir = r"""%s"""
prompt = r"""%s"""
output_path = r"""%s"""
device = "%s"
steps = %d

def pip_install(*pkgs):
    for args in [
        [sys.executable, "-m", "pip", "install", "-q"] + list(pkgs),
        [sys.executable, "-m", "pip", "install", "-q", "--break-system-packages"] + list(pkgs),
    ]:
        if subprocess.run(args, capture_output=True).returncode == 0:
            return True
    return False

print("VORTELIO_PROGRESS:10:Installing diffusers...")
pip_install("diffusers[torch]", "transformers", "accelerate")

print("VORTELIO_PROGRESS:25:Loading WanPipeline...")
try:
    from diffusers import WanPipeline
    import torch
    dtype = torch.float16 if device == "cuda" else torch.float32
    pipe = WanPipeline.from_pretrained(model_dir, torch_dtype=dtype)
    if device == "cuda":
        pipe.enable_model_cpu_offload()
    print("VORTELIO_PROGRESS:55:Generating video...")
    output = pipe(prompt=prompt, num_inference_steps=steps, num_frames=49).frames[0]
    print("VORTELIO_PROGRESS:90:Saving...")
    from diffusers.utils import export_to_video
    export_to_video(output, output_path, fps=16)
    print("VORTELIO_DONE:" + output_path)
    print("VORTELIO_PROGRESS:100:Done!")
except Exception as e:
    print(f"VORTELIO_ERROR:{e}")
    sys.exit(1)
`, modelDir, escapePy(prompt), outputFwd, device, steps)
}

// hunyuanScript — HunyuanVideo / LTX-Video / Mochi via diffusers
func (r *VideoRunner) hunyuanScript(prompt, output string, steps int, forceCPU bool) string {
	modelDir := strings.ReplaceAll(r.modelDir(), `\`, `/`)
	device := r.deviceString(forceCPU)
	name := strings.ToLower(r.model.Name)
	outputFwd := strings.ReplaceAll(output, `\`, `/`)

	pipelineClass := "HunyuanVideoPipeline"
	if strings.Contains(name, "ltx") {
		pipelineClass = "LTXPipeline"
	}
	if strings.Contains(name, "mochi") {
		pipelineClass = "MochiPipeline"
	}

	return fmt.Sprintf(`import sys, os, subprocess
os.environ["PYTHONIOENCODING"] = "utf-8"
model_dir = r"""%s"""
prompt = r"""%s"""
output_path = r"""%s"""
device = "%s"
steps = %d
pipeline_class = "%s"

print("VORTELIO_PROGRESS:10:Installing diffusers...")
subprocess.run([sys.executable, "-m", "pip", "install", "-q", "diffusers[torch]", "transformers", "accelerate"], capture_output=True)

print(f"VORTELIO_PROGRESS:25:Loading {pipeline_class}...")
try:
    import diffusers, torch
    PipeClass = getattr(diffusers, pipeline_class)
    dtype = torch.bfloat16 if device == "cuda" else torch.float32
    pipe = PipeClass.from_pretrained(model_dir, torch_dtype=dtype)
    if device == "cuda":
        pipe.enable_model_cpu_offload()
    print("VORTELIO_PROGRESS:55:Generating video...")
    frames = pipe(prompt=prompt, num_inference_steps=steps).frames[0]
    print("VORTELIO_PROGRESS:90:Saving...")
    from diffusers.utils import export_to_video
    export_to_video(frames, output_path, fps=24)
    print("VORTELIO_DONE:" + output_path)
    print("VORTELIO_PROGRESS:100:Done!")
except Exception as e:
    print(f"VORTELIO_ERROR:{e}")
    sys.exit(1)
`, modelDir, escapePy(prompt), outputFwd, device, steps, pipelineClass)
}
