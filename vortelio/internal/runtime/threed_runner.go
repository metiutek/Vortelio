package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
)

// ThreeDRunner generates 3D models using TripoSR or Shap-E.
type ThreeDRunner struct {
	model *hub.Model
	hw    *Hardware
}

func NewThreeDRunner(model *hub.Model, hw *Hardware) *ThreeDRunner {
	return &ThreeDRunner{model: model, hw: hw}
}

func (r *ThreeDRunner) Run(opts *RunOptions) error {
	pythonBin := FindPython()
	if pythonBin == "" {
		return fmt.Errorf("Python 3 not found — needed for 3D models\n  Install Python 3.10+ from https://python.org/downloads")
	}

	name := strings.ToLower(r.model.Name)
	// Also check source/path for model type hints
	path := strings.ToLower(r.model.LocalPath)

	switch {
	case strings.Contains(name, "triposr") || strings.Contains(name, "tripo"):
		return r.runTripoSR(pythonBin, opts)
	case strings.Contains(name, "shap-e") || strings.Contains(name, "shape") || strings.Contains(name, "shap_e"):
		return r.runShapE(pythonBin, opts)
	case strings.Contains(name, "lgm") || strings.Contains(path, "lgm"):
		return r.runLGM(pythonBin, opts)
	case strings.Contains(name, "trellis") || strings.Contains(path, "trellis"):
		return r.runTRELLIS(pythonBin, opts)
	default:
		// Generic: try as a diffusers/transformers 3D model
		return r.runGeneric3D(pythonBin, opts)
	}
}

// runTripoSR: image → 3D mesh (best quality, fast)
func (r *ThreeDRunner) runTripoSR(pythonBin string, opts *RunOptions) error {
	if opts.InputFile == "" && opts.Prompt == "" {
		fmt.Println("ℹ️   TripoSR: generate 3D models from an image")
		fmt.Println("    vortelio run 3d/triposr --input ./foto_oggetto.png")
		fmt.Println("    vortelio run 3d/triposr --input ./foto.jpg --output modello.obj")
		return nil
	}

	outputPath := ResolveOutputPath(opts.OutputFile, "output.obj")
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)

	if opts.InputFile != "" {
		// Image to 3D
		inputPath := strings.ReplaceAll(opts.InputFile, `\`, `/`)
		fmt.Printf("🧊  3D generation from image\n    Input: %s\n    Output: %s\n\n", opts.InputFile, outputPath)

		// Install TripoSR dependencies
		// Note: TripoSR is not on PyPI — must install from GitHub
		// Requires: git installed and accessible in PATH
		if !CheckPythonPackage(pythonBin, "tsr") {
			fmt.Println("📦  Installing TripoSR e dipendenze...")
			// Install deps first (these are on PyPI)
			_ = InstallPythonPackage(pythonBin, "trimesh", "Pillow", "einops", "omegaconf", "huggingface_hub", "torch", "torchvision")
			// Try GitHub install
			_ = InstallPythonPackage(pythonBin, "git+https://github.com/VAST-AI-Research/TripoSR.git")
			if !CheckPythonPackage(pythonBin, "tsr") {
				fmt.Println()
				fmt.Println("❌  TripoSR not installed automatically.")
				fmt.Println("    Requires git installed. Install manually with:")
				fmt.Println()
				fmt.Println("    1. Installa git: https://git-scm.com/download/win")
				fmt.Println("    2. pip install git+https://github.com/VAST-AI-Research/TripoSR.git")
				fmt.Println()
				fmt.Println("    Or use Shap-E (does not require git):")
				fmt.Println(`    vortelio pull 3d/shap-e && vortelio run 3d/shap-e "un gatto"`)
				return nil
			}
		}

		script := fmt.Sprintf(`import sys, os
output_path = r'''%s'''
input_path = r'''%s'''
try:
    import torch
    from PIL import Image
    import trimesh
except ImportError as e:
    print('Dipendenza mancante:', e)
    print('pip install torch trimesh Pillow einops omegaconf')
    sys.exit(1)

print('Loading TripoSR...')
try:
    from tsr.system import TSR
    model_path = r'''%s'''
    model = TSR.from_pretrained(model_path if os.path.isdir(model_path) else 'stabilityai/TripoSR')
except Exception:
    from tsr.system import TSR
    model = TSR.from_pretrained('stabilityai/TripoSR')

device = 'cuda' if torch.cuda.is_available() else 'cpu'
model = model.to(device)
model.renderer.set_chunk_size(8192)

img = Image.open(input_path).convert('RGBA')
print('Generating 3D mesh...')
with torch.no_grad():
    scene_codes = model([img], device=device)
    meshes = model.extract_mesh(scene_codes, resolution=256)

mesh = meshes[0]
mesh.export(output_path)
print(f'\n✅  3D model saved to: ' + output_path)
`, outputPath, inputPath, r.model.LocalPath)

		return r.runPython(pythonBin, script)
	}

	// Text → 3D: use Shap-E (no git required, works on Python 3.14)
	// TripoSR text→3D requires git+GitHub which often fails on Windows
	// Shap-E is installed directly from PyPI and works reliably
	fmt.Printf("🧊  3D generation from text → Shap-E\n    Prompt: %q\n    Output: %s\n", opts.Prompt, outputPath)
	fmt.Println("ℹ️   Using Shap-E (recommended method for text→3D on Windows)")
	fmt.Println()
	// Delegate to Shap-E
	return r.runShapE(pythonBin, opts)
}

// runShapE: text/image → 3D (Shap-E by OpenAI)
func (r *ThreeDRunner) runShapE(pythonBin string, opts *RunOptions) error {
	text := opts.Prompt
	if text == "" && opts.InputFile == "" {
		fmt.Println("ℹ️   Shap-E: Text → 3D Model")
		fmt.Println("    vortelio run 3d/shap-e \"una sedia di legno\"")
		return nil
	}

	outputPath := ResolveOutputPath(opts.OutputFile, "output.ply")
	outputPath = strings.ReplaceAll(outputPath, `\`, `/`)
	inputPath := strings.ReplaceAll(opts.InputFile, `\`, `/`)

	fmt.Printf("🧊  3D generation (Shap-E)\n    Output: %s\n\n", outputPath)

	script := fmt.Sprintf(`import sys, torch
output_path = r'''%s'''
try:
    from shap_e.diffusion.sample import sample_latents
    from shap_e.diffusion.gaussian_diffusion import diffusion_from_config
    from shap_e.models.download import load_model, load_config
    from shap_e.util.notebooks import create_pan_cameras, decode_latent_mesh
except ImportError:
    print('Shap-E not installed.')
    print('pip install git+https://github.com/openai/shap-e.git')
    sys.exit(1)

device = torch.device('cuda' if torch.cuda.is_available() else 'cpu')
print('Loading Shap-E...')
xm = load_model('transmitter', device=device)
diffusion = diffusion_from_config(load_config('diffusion'))

if %v:
    # Image to 3D
    model = load_model('image300M', device=device)
    from PIL import Image
    img = Image.open(r'''%s''')
    batch_size = 1
    guidance_scale = 3.0
    latents = sample_latents(batch_size=batch_size, model=model, diffusion=diffusion,
        guidance_scale=guidance_scale, model_kwargs=dict(images=[img]*batch_size),
        progress=True, clip_denoised=True, use_karras=True, karras_steps=64,
        sigma_min=1e-3, sigma_max=160, s_churn=0)
else:
    # Text to 3D
    model = load_model('text300M', device=device)
    batch_size = 1
    guidance_scale = 15.0
    latents = sample_latents(batch_size=batch_size, model=model, diffusion=diffusion,
        guidance_scale=guidance_scale, model_kwargs=dict(texts=[%q]),
        progress=True, clip_denoised=True, use_karras=True, karras_steps=64,
        sigma_min=1e-3, sigma_max=160, s_churn=0)

print('Esportazione mesh...')
t = decode_latent_mesh(xm, latents[0]).tri_mesh()
with open(output_path, 'wb') as f:
    t.write_ply(f)
print('\n✅  3D model saved to: ' + output_path)
`, outputPath, opts.InputFile != "", inputPath, text)

	return r.runPython(pythonBin, script)
}

// runLGM: Large Multi-View Gaussian (image → 3D gaussian splat)
func (r *ThreeDRunner) runLGM(pythonBin string, opts *RunOptions) error {
	if opts.InputFile == "" && opts.Prompt == "" {
		fmt.Printf("ℹ️   LGM (%s): generate 3D models from image or text\n", r.model.Name)
		fmt.Println("    vortelio run 3d/jctn--lgm --input ./foto.png")
		fmt.Printf("    vortelio run 3d/jctn--lgm \"un gatto\"\n")
		return nil
	}
	// Text prompt: route to TripoSR's text→image→3D pipeline
	// (LGM itself needs an image, so we generate one first with SD then pass to TripoSR)
	if opts.InputFile == "" && opts.Prompt != "" {
		fmt.Printf("ℹ️   LGM requires an image as input.\n")
		fmt.Printf("    Auto-generating image from text with TripoSR text→3D:\n")
		fmt.Printf("    vortelio run 3d/triposr \"%s\"\n", opts.Prompt)
		fmt.Println()
		// Automatically delegate to triposr text→3D pipeline
		return r.runTripoSR(pythonBin, opts)
	}
	outputPath := strings.ReplaceAll(ResolveOutputPath(opts.OutputFile, "output.ply"), `\`, `/`)
	inputPath := strings.ReplaceAll(opts.InputFile, `\`, `/`)
	fmt.Printf("🧊  3D generation (LGM)\n    Input: %s\n    Output: %s\n\n", opts.InputFile, outputPath)

	script := fmt.Sprintf(`import sys, torch
output_path = r'''%s'''
input_path = r'''%s'''
model_path = r'''%s'''

try:
    from PIL import Image
    import numpy as np
except ImportError:
    print("pip install Pillow numpy")
    sys.exit(1)

print("Loading LGM...")
try:
    from huggingface_hub import hf_hub_download
    from lgm.models.lgm import LGM
    print("LGM loaded successfully")
except ImportError:
    print("LGM not installed. Install with:")
    print("  pip install git+https://github.com/3DTopia/LGM.git")
    sys.exit(1)

device = "cuda" if torch.cuda.is_available() else "cpu"
model = LGM()
ckpt = torch.load(model_path, map_location=device)
model.load_state_dict(ckpt, strict=False)
model = model.to(device).eval()

img = Image.open(input_path).resize((256, 256)).convert("RGBA")
import torchvision.transforms.functional as TF
inp = TF.to_tensor(img).unsqueeze(0).to(device)

with torch.no_grad():
    out = model(inp)

# Save as PLY
import struct
pts = out["gaussians"][0].cpu().numpy()
with open(output_path, "wb") as f:
    f.write(b"ply\nformat binary_little_endian 1.0\n")
    f.write(f"element vertex {len(pts)}\n".encode())
    f.write(b"property float x\nproperty float y\nproperty float z\nend_header\n")
    for p in pts:
        f.write(struct.pack("<fff", *p[:3]))
print(f"\n\u2705  3D model saved to: " + output_path)
`, outputPath, inputPath, strings.ReplaceAll(r.model.LocalPath, `\`, `/`))
	return r.runPython(pythonBin, script)
}

// runTRELLIS: Microsoft TRELLIS text/image → 3D
func (r *ThreeDRunner) runTRELLIS(pythonBin string, opts *RunOptions) error {
	if opts.Prompt == "" && opts.InputFile == "" {
		fmt.Printf("ℹ️   TRELLIS (%s): generate 3D models from text or image\n", r.model.Name)
		fmt.Println(`    vortelio run 3d/jctn--lgm "una sedia di legno"`)
		fmt.Println("    vortelio run 3d/jctn--lgm --input ./foto.jpg")
		return nil
	}
	outputPath := strings.ReplaceAll(ResolveOutputPath(opts.OutputFile, "output.glb"), `\`, `/`)
	fmt.Printf("🧊  3D generation (TRELLIS)\n    Output: %s\n\n", outputPath)
	return fmt.Errorf("TRELLIS requires a separate install\n  pip install git+https://github.com/microsoft/TRELLIS.git")
}

// runGeneric3D: fallback per modelli 3D non riconosciuti
func (r *ThreeDRunner) runGeneric3D(pythonBin string, opts *RunOptions) error {
	modelName := r.model.Name
	modelPath := r.model.LocalPath
	fmt.Printf("🧊  3D model: %s\n", modelName)
	fmt.Printf("    Path: %s\n\n", modelPath)

	if opts.Prompt == "" && opts.InputFile == "" {
		fmt.Println("ℹ️   Specifica un input per questo modello:")
		fmt.Printf("    vortelio run 3d/%s --input ./immagine.png\n", modelName)
		fmt.Printf("    vortelio run 3d/%s \"descrizione oggetto\"\n", modelName)
		return nil
	}

	// Try to run as a transformers model via pipeline
	outputPath := strings.ReplaceAll(ResolveOutputPath(opts.OutputFile, "output.obj"), `\`, `/`)
	inputPath := strings.ReplaceAll(opts.InputFile, `\`, `/`)
	modelDir := strings.ReplaceAll(strings.TrimSuffix(modelPath, "/model.safetensors"), `\`, `/`)

	script := fmt.Sprintf(`import sys, os
output_path = r'''%s'''
input_path = r'''%s'''
model_path = r'''%s'''

print("Trying to load 3D model: " + model_path)
print()

# Try transformers pipeline first
try:
    from transformers import pipeline
    import torch
    device = 0 if torch.cuda.is_available() else -1
    pipe = pipeline("image-to-3d", model=model_path, device=device)
    if input_path:
        from PIL import Image
        img = Image.open(input_path)
        result = pipe(img)
    else:
        result = pipe('''%s''')
    print("\n\u2705  Done")
except Exception as e:
    print(f"Error: {e}")
    print()
    print("The model is not supported automatically.")
    print("See the model documentation on HuggingFace")
    print("per istruzioni di utilizzo specifiche.")
    sys.exit(1)
`, outputPath, inputPath, modelDir, escapePy(opts.Prompt))

	return r.runPython(pythonBin, script)
}

func (r *ThreeDRunner) runPython(pythonBin, script string) error {
	return r.runPythonProg(pythonBin, script, nil)
}

func (r *ThreeDRunner) runPythonProg(pythonBin, script string, onProgress func(ProgressEvent)) error {
	tmp, err := os.CreateTemp("", "vortelio-3d-*.py")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(script)
	tmp.Close()
	return RunWithOutput(
		HideWindow(exec.Command(pythonBin, tmp.Name())),
		os.Stdout, os.Stderr,
	)
}

// RunWithProgress for 3D
func (r *ThreeDRunner) RunWithProgress(opts *RunOptions, progress chan<- ProgressEvent) error {
	if progress != nil {
		progress <- ProgressEvent{Percent: 5, Message: "Starting 3D generation..."}
	}
	err := r.Run(opts)
	if progress != nil {
		if err == nil {
			progress <- ProgressEvent{Percent: 100, Message: "Done!"}
		}
		close(progress)
	}
	return err
}
