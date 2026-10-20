package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
)

type ImageRunner struct {
	model *hub.Model
	hw    *Hardware
}

func NewImageRunner(model *hub.Model, hw *Hardware) *ImageRunner {
	return &ImageRunner{model: model, hw: hw}
}

func (r *ImageRunner) Run(opts *RunOptions) error {
	return r.run(opts, nil)
}

// run performs the generation. emit, when non-nil, receives progress updates.
func (r *ImageRunner) run(opts *RunOptions, emit func(pct int, msg string)) error {
	if opts.Prompt == "" {
		return fmt.Errorf("image generation requires a text prompt\n  Example: vortelio run image/z-image:turbo \"a cat eating pasta\"")
	}

	output := ResolveOutputPath(opts.OutputFile, "output.png")

	if strings.EqualFold(filepath.Ext(r.model.LocalPath), ".gguf") {
		res, err := NativeGenerateImage(context.Background(), r.model, r.hw, ImageRequest{
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
		paths, err := writeImages(res, output)
		if err != nil {
			return err
		}
		for _, p := range paths {
			fmt.Printf("✅  Image saved: %s (seed %d)\n", p, res.Seed)
		}
		return nil
	}

	device := r.deviceString(opts.ForceCPU)
	fmt.Printf("🎨  Generating image (%d step, device=%s)\n", opts.Steps, device)
	fmt.Printf("    Prompt: %q\n", opts.Prompt)
	fmt.Printf("    Output: %s\n\n", output)
	AcquireGPU("image") // free VRAM held by a chat model
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("Python 3 not found — needed to run a %s model.\n  Install Python 3.10+ from https://python.org/downloads, or use a GGUF image model", filepath.Ext(r.model.LocalPath))
	}
	if opts.Steps <= 0 {
		opts.Steps = 25
	}
	return r.runDiffusers(pythonBin, opts.Prompt, output, device, opts.Steps)
}

// sdSamplingRe matches the stable-diffusion.cpp sampling progress line, e.g.
//
//	|============>        | 2/4 - 5.59s/it
//
// The unit suffix distinguishes it from the tensor-loading bars (… - 801MB/s).
var sdSamplingRe = regexp.MustCompile(`\|\s*(\d+)/(\d+)\s*-\s*[\d.]+\s*(?:s/it|it/s)`)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\[K`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// runDiffusers handles safetensors/diffusers format models
func (r *ImageRunner) runDiffusers(pythonBin, prompt, output, device string, steps int) error {
	if !CheckPythonPackage(pythonBin, "diffusers") {
		fmt.Println("📦  diffusers not installed.")
		if r.hw.Backend == BackendCUDA {
			fmt.Println("    pip install diffusers transformers accelerate torch --index-url https://download.pytorch.org/whl/cu121")
		} else {
			fmt.Println("    pip install diffusers transformers accelerate torch")
		}
		return nil
	}

	modelPath := r.model.LocalPath
	modelDir := filepath.Dir(modelPath)
	for {
		if _, err := os.Stat(filepath.Join(modelDir, "model_index.json")); err == nil {
			break
		}
		p := filepath.Dir(modelDir)
		if p == modelDir {
			break
		}
		modelDir = p
	}

	modelPath = strings.ReplaceAll(modelPath, `\`, `/`)
	modelDir = strings.ReplaceAll(modelDir, `\`, `/`)
	output = strings.ReplaceAll(output, `\`, `/`)

	script := fmt.Sprintf(`import sys, os, warnings
warnings.filterwarnings("ignore")
try:
    import torch
    from diffusers import DiffusionPipeline, StableDiffusionPipeline, StableDiffusionXLPipeline
except ImportError as e:
    print(f"Dipendenza mancante: {e}")
    print("pip install diffusers transformers accelerate torch")
    sys.exit(1)

device     = %q
model_path = r'''%s'''
model_dir  = r'''%s'''
dtype = torch.float16 if device != "cpu" else torch.float32

print("Loading model...")
pipe = None
has_index = os.path.exists(os.path.join(model_dir, "model_index.json"))

if has_index:
    try:
        pipe = DiffusionPipeline.from_pretrained(model_dir, torch_dtype=dtype, safety_checker=None)
    except Exception as e:
        print(f"  from_pretrained: {e}")

if pipe is None and os.path.isfile(model_path):
    for cls in [StableDiffusionXLPipeline, StableDiffusionPipeline, DiffusionPipeline]:
        try:
            pipe = cls.from_single_file(model_path, torch_dtype=dtype, safety_checker=None)
            break
        except Exception:
            continue

if pipe is None:
    print("Error: could not load the model.")
    sys.exit(1)

pipe = pipe.to(device)
if device != "cpu":
    pipe.enable_attention_slicing()
    try: pipe.enable_xformers_memory_efficient_attention()
    except: pass

print("Generating...")
result = pipe(prompt='''%s''', num_inference_steps=%d, guidance_scale=7.5)
result.images[0].save(r'''%s''')
print(f"\n✅  Image saved: %s")
`, device, modelPath, modelDir, escapePy(prompt), steps, output, output)

	return r.runPython(pythonBin, script)
}

func (r *ImageRunner) runPython(pythonBin, script string) error {
	tmp, err := os.CreateTemp("", "vortelio-img-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	cmd := HideWindow(exec.Command(pythonBin, tmp.Name()))
	cmd.Env = append(os.Environ(),
		"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1",
		"USE_TF=0", "USE_JAX=0",
		"TRANSFORMERS_VERBOSITY=error",
		"TF_CPP_MIN_LOG_LEVEL=3",
	)
	if err := RunWithOutput(cmd, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("image generation failed: %w", err)
	}
	return nil
}

// RunWithProgress runs image generation, streaming real progress events (parsed
// from the stable-diffusion.cpp sampling bar) on the given channel. The channel is always
// closed before returning.
func (r *ImageRunner) RunWithProgress(opts *RunOptions, progress chan<- ProgressEvent) error {
	if progress == nil {
		return r.Run(opts)
	}
	defer close(progress)

	if opts.Prompt == "" {
		return fmt.Errorf("image generation requires a text prompt")
	}

	// stdout and stderr are read concurrently, so keep the percentage monotonic:
	// a progress bar that jumps backwards looks like a bug to the user.
	var maxPct int
	send := func(pct int, msg string) {
		if pct < maxPct {
			pct = maxPct
		}
		maxPct = pct
		// Non-blocking: never stall generation if the client stopped reading.
		select {
		case progress <- ProgressEvent{Percent: pct, Message: msg}:
		default:
		}
	}
	send(5, "Starting generation…")

	err := r.run(opts, send)
	if err == nil {
		send(100, "Done!")
	}
	return err
}

// GenerateToBytes runs image generation and returns the PNG bytes.
func (r *ImageRunner) GenerateToBytes(prompt string, steps int, forceCPU bool) ([]byte, error) {
	if strings.EqualFold(filepath.Ext(r.model.LocalPath), ".gguf") {
		res, err := NativeGenerateImage(context.Background(), r.model, r.hw, ImageRequest{Prompt: prompt, Steps: steps, ForceCPU: forceCPU}, nil)
		if err != nil {
			return nil, err
		}
		return res.Images[0], nil
	}
	AcquireGPU("image") // free VRAM held by a chat model
	tmp, err := os.CreateTemp("", "vortelio-img-*.png")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	opts := &RunOptions{Prompt: prompt, OutputFile: tmpPath, Steps: steps, ForceCPU: forceCPU}
	if err := r.Run(opts); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpPath)
}

func (r *ImageRunner) deviceString(forceCPU bool) string {
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

func boolPy(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
