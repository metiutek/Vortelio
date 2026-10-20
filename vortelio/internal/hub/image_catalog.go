package hub

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/vortelio/vortelio/internal/config"
)

// Native image models (stable-diffusion.cpp). A model belongs to a family;
// the family says which extra files the engine needs next to the main one
// (VAE, text encoders) and the settings it was trained for. Older models
// (SD 1.5, SDXL) are a single file; newer ones (FLUX, Z-Image) ship the
// diffusion weights alone and share the VAE and encoders, which are
// downloaded once into models/image/_shared.

// Image component roles. Each is the sd-server flag that loads the file.
const (
	RoleModel     = "model"           // -m: full checkpoint (UNet + VAE + CLIP)
	RoleDiffusion = "diffusion-model" // --diffusion-model: standalone diffusion weights
	RoleVAE       = "vae"
	RoleClipL     = "clip_l"
	RoleT5XXL     = "t5xxl"
	RoleLLM       = "llm" // LLM text encoder (Qwen3 for Z-Image)
)

// ImageComponent is an extra file a family needs.
type ImageComponent struct {
	Role string `json:"role"`
	Repo string `json:"repo"`
	File string `json:"file"`
	Size int64  `json:"size"`
}

// ImageFamily describes how a family of image models is run.
type ImageFamily struct {
	ID         string           `json:"id"`
	Label      string           `json:"label"`
	MainRole   string           `json:"main_role"`
	Components []ImageComponent `json:"components,omitempty"`
	// Defaults the model was trained for.
	Steps     int     `json:"steps"`
	CFG       float64 `json:"cfg"`
	Guidance  float64 `json:"guidance,omitempty"` // distilled guidance (FLUX dev)
	Sampler   string  `json:"sampler"`
	Scheduler string  `json:"scheduler,omitempty"`
	FlowShift float64 `json:"flow_shift,omitempty"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Negative  bool    `json:"negative"` // the negative prompt has an effect (real CFG)
	// VAETiling decodes in tiles: needed at high resolution on small GPUs.
	VAETiling bool `json:"vae_tiling,omitempty"`
}

var (
	compFluxVAE  = ImageComponent{Role: RoleVAE, Repo: "second-state/FLUX.1-schnell-GGUF", File: "ae.safetensors", Size: 335304388}
	compFluxClip = ImageComponent{Role: RoleClipL, Repo: "second-state/FLUX.1-schnell-GGUF", File: "clip_l-Q8_0.gguf", Size: 131000000}
	compFluxT5   = ImageComponent{Role: RoleT5XXL, Repo: "second-state/FLUX.1-schnell-GGUF", File: "t5xxl-Q4_K.gguf", Size: 2753000000}
	compQwen3TE  = ImageComponent{Role: RoleLLM, Repo: "unsloth/Qwen3-4B-GGUF", File: "Qwen3-4B-Q4_K_M.gguf", Size: 2497000000}
	compSDXLVAE  = ImageComponent{Role: RoleVAE, Repo: "madebyollin/sdxl-vae-fp16-fix", File: "sdxl_vae.safetensors", Size: 334643268}
)

// ImageFamilies are the families Vortelio knows how to run.
var ImageFamilies = map[string]*ImageFamily{
	"sd1": {ID: "sd1", Label: "Stable Diffusion 1.5", MainRole: RoleModel,
		Steps: 20, CFG: 7, Sampler: "euler_a", Width: 512, Height: 512, Negative: true},
	"sdxl": {ID: "sdxl", Label: "Stable Diffusion XL", MainRole: RoleModel,
		Components: []ImageComponent{compSDXLVAE},
		Steps:      25, CFG: 6, Sampler: "dpm++2m", Scheduler: "karras", Width: 1024, Height: 1024, Negative: true, VAETiling: true},
	"flux-schnell": {ID: "flux-schnell", Label: "FLUX.1 Schnell", MainRole: RoleDiffusion,
		Components: []ImageComponent{compFluxVAE, compFluxClip, compFluxT5},
		Steps:      4, CFG: 1, Sampler: "euler", Width: 768, Height: 768, VAETiling: true},
	"flux-dev": {ID: "flux-dev", Label: "FLUX.1 Dev", MainRole: RoleDiffusion,
		Components: []ImageComponent{compFluxVAE, compFluxClip, compFluxT5},
		Steps:      20, CFG: 1, Guidance: 3.5, Sampler: "euler", Width: 768, Height: 768, VAETiling: true},
	"z-image": {ID: "z-image", Label: "Z-Image Turbo", MainRole: RoleDiffusion,
		Components: []ImageComponent{compFluxVAE, compQwen3TE},
		Steps:      8, CFG: 1, Sampler: "euler", FlowShift: 3, Width: 768, Height: 768, VAETiling: true},
}

// ImageCatalogEntry is one curated, tested image model.
type ImageCatalogEntry struct {
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Family      string `json:"family"`
	Repo        string `json:"repo"`
	File        string `json:"file"`
	SizeBytes   int64  `json:"size_bytes"` // main file + components
	License     string `json:"license"`
	Recommended bool   `json:"recommended,omitempty"`
}

// ImageCatalog is the curated image model list, recommended models first.
var ImageCatalog = []ImageCatalogEntry{
	{Ref: "image/z-image:turbo", Name: "Z-Image Turbo", Family: "z-image",
		Description: "Best quality: photos, illustrations and readable text in 8 steps. Runs on a 4 GB GPU (about 2–4 minutes per image).",
		Repo:        "leejet/Z-Image-Turbo-GGUF", File: "z_image_turbo-Q3_K.gguf", SizeBytes: 3143559104 + compFluxVAE.Size + compQwen3TE.Size,
		License: "Apache-2.0", Recommended: true},
	{Ref: "image/sd1.5:latest", Name: "Stable Diffusion 1.5", Family: "sd1",
		Description: "Light and fast, runs anywhere. 512×512.",
		Repo:        "gpustack/stable-diffusion-v1-5-GGUF", File: "stable-diffusion-v1-5-Q4_1.gguf", SizeBytes: 1763946592,
		License: "CreativeML OpenRAIL-M", Recommended: true},
	{Ref: "image/dreamshaper:latest", Name: "DreamShaper 8", Family: "sd1",
		Description: "SD 1.5 fine-tune for artistic and fantasy images.",
		Repo:        "gpustack/dreamshaper-8-GGUF", File: "dreamshaper-8-Q5_0.gguf", SizeBytes: 1840000000,
		License: "CreativeML OpenRAIL-M"},
	{Ref: "image/sdxl:latest", Name: "Stable Diffusion XL", Family: "sdxl",
		Description: "1024×1024, more detail than SD 1.5. Slow on a 4 GB GPU.",
		Repo:        "gpustack/stable-diffusion-xl-base-1.0-GGUF", File: "stable-diffusion-xl-base-1.0-Q4_1.gguf", SizeBytes: 4078000000 + compSDXLVAE.Size,
		License: "CreativeML OpenRAIL++-M"},
	{Ref: "image/realvisxl:v4", Name: "RealVisXL 4", Family: "sdxl",
		Description: "SDXL fine-tune for photorealism.",
		Repo:        "gpustack/RealVisXL_V4.0-GGUF", File: "RealVisXL_V4.0-Q5_0.gguf", SizeBytes: 4300000000 + compSDXLVAE.Size,
		License: "CreativeML OpenRAIL++-M"},
	{Ref: "image/flux:schnell", Name: "FLUX.1 Schnell", Family: "flux-schnell",
		Description: "Excellent prompt following in 4 steps. Large: 8 GB+ GPU recommended.",
		Repo:        "city96/FLUX.1-schnell-gguf", File: "flux1-schnell-Q4_K_S.gguf", SizeBytes: 6784000000 + compFluxVAE.Size + compFluxClip.Size + compFluxT5.Size,
		License: "Apache-2.0"},
	{Ref: "image/flux:dev", Name: "FLUX.1 Dev", Family: "flux-dev",
		Description: "Higher quality FLUX, 20 steps. Non-commercial license, 8 GB+ GPU recommended.",
		Repo:        "city96/FLUX.1-dev-gguf", File: "flux1-dev-Q4_K_S.gguf", SizeBytes: 6810000000 + compFluxVAE.Size + compFluxClip.Size + compFluxT5.Size,
		License: "FLUX.1-dev non-commercial"},
}

func init() {
	for _, e := range ImageCatalog {
		hfRegistry[e.Ref] = HFEntry{Repo: e.Repo, File: e.File, Format: "gguf", License: e.License}
	}
	// openjourney was always plain SD 1.5.
	hfRegistry["image/openjourney:latest"] = hfRegistry["image/sd1.5:latest"]
}

// ImageCatalogFor returns the catalog entry for a model reference, if any.
func ImageCatalogFor(ref *ModelRef) (ImageCatalogEntry, bool) {
	key := ref.Type + "/" + ref.Name + ":" + ref.Tag
	for _, e := range ImageCatalog {
		if e.Ref == key {
			return e, true
		}
	}
	return ImageCatalogEntry{}, false
}

// DetectImageFamily guesses the family of an image GGUF from its file name
// ("" when it is not a known one).
func DetectImageFamily(file string) string {
	n := strings.ToLower(filepath.Base(file))
	switch {
	case strings.Contains(n, "z_image") || strings.Contains(n, "z-image") || strings.Contains(n, "zimage"):
		return "z-image"
	case strings.Contains(n, "flux") && strings.Contains(n, "schnell"):
		return "flux-schnell"
	case strings.Contains(n, "flux") && (strings.Contains(n, "dev") || strings.Contains(n, "krea")):
		return "flux-dev"
	case strings.Contains(n, "flux"), strings.Contains(n, "sd3"), strings.Contains(n, "qwen"), strings.Contains(n, "wan"):
		return "" // needs files Vortelio cannot pick for it
	case strings.Contains(n, "xl"):
		return "sdxl"
	}
	return "sd1"
}

// ImageFamilyOf returns the family of an installed image model, or nil.
func ImageFamilyOf(m *Model) *ImageFamily {
	if m.Family != "" {
		if f := ImageFamilies[m.Family]; f != nil {
			return f
		}
	}
	return ImageFamilies[DetectImageFamily(m.LocalPath)]
}

// sharedComponentPath is where a component lives on disk, shared by every
// model of the families that use it.
func sharedComponentPath(c ImageComponent) string {
	owner, repo, _ := strings.Cut(c.Repo, "/")
	norm := strings.NewReplacer(".", "-", "_", "-")
	dir := norm.Replace(strings.ToLower(owner)) + "__" + norm.Replace(strings.ToLower(repo))
	return filepath.Join(config.HomeDir(), "models", "image", "_shared", dir, path.Base(c.File))
}

// describeImageModel fills the engine, family and component paths of an
// image GGUF. Non-GGUF image models (diffusers folders) are left alone.
func describeImageModel(m *Model, catalog *ImageCatalogEntry) {
	if m.Type != "image" || !isGGUF(m.LocalPath) {
		return
	}
	fam := ""
	if catalog != nil {
		fam = catalog.Family
		if m.Description == "" {
			m.Description = catalog.Description
		}
	} else {
		fam = DetectImageFamily(m.LocalPath)
	}
	m.Engine = "sdcpp"
	m.Family = fam
	m.Capabilities = []string{"text-to-image"}
	if f := ImageFamilies[fam]; f != nil {
		m.Components = map[string]string{}
		for _, c := range f.Components {
			m.Components[c.Role] = sharedComponentPath(c)
		}
	}
}

// MissingImageComponents lists the components of m not yet on disk.
func MissingImageComponents(m *Model) []ImageComponent {
	f := ImageFamilyOf(m)
	if f == nil {
		return nil
	}
	var out []ImageComponent
	for _, c := range f.Components {
		p := sharedComponentPath(c)
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			out = append(out, c)
		}
	}
	return out
}

// EnsureImageComponents downloads the components m still needs and records
// their paths in its manifest. progress receives (bytes done, total) over all
// missing components.
func EnsureImageComponents(ctx context.Context, m *Model, progress ProgressCallback) error {
	f := ImageFamilyOf(m)
	if f == nil {
		if m.Type == "image" && isGGUF(m.LocalPath) {
			return fmt.Errorf("Vortelio does not know which extra files %s needs — use a model from the Images catalog", filepath.Base(m.LocalPath))
		}
		return nil
	}
	d := NewDownloaderWithContext(ctx)
	if err := d.fetchImageComponents(MissingImageComponents(m), 0, progress); err != nil {
		return err
	}
	changed := m.Engine != "sdcpp" || m.Family != f.ID || len(m.Components) != len(f.Components)
	if changed {
		m.Engine, m.Family = "sdcpp", f.ID
		m.Components = map[string]string{}
		for _, c := range f.Components {
			m.Components[c.Role] = sharedComponentPath(c)
		}
		return d.store.Save(m)
	}
	return nil
}

// fetchImageComponents downloads comps; progress totals start at offset (the
// bytes of the main file already counted).
func (d *Downloader) fetchImageComponents(comps []ImageComponent, offset int64, progress ProgressCallback) error {
	if len(comps) == 0 {
		return nil
	}
	// Exact sizes and checksums from the repo listings.
	sizes := map[string]hfFileInfo{}
	for _, c := range comps {
		if _, ok := sizes[c.Repo+"/"+c.File]; ok {
			continue
		}
		owner, repo, _ := strings.Cut(c.Repo, "/")
		if info, err := fetchHFModelInfoCtx(d.context(), owner, repo); err == nil {
			for _, f := range info.Files {
				sizes[c.Repo+"/"+f.Rfilename] = f
			}
		}
	}
	var total int64 = offset
	for i, c := range comps {
		if f, ok := sizes[c.Repo+"/"+c.File]; ok && f.Size > 0 {
			comps[i].Size = f.Size
		}
		total += comps[i].Size
	}
	done := offset
	for _, c := range comps {
		dest := sharedComponentPath(c)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		fmt.Printf("  ⬇️   %s (%s, %.0f MB)\n", path.Base(c.File), c.Role, float64(c.Size)/1e6)
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", c.Repo, c.File)
		base := done
		err := d.downloadWithProgress(url, dest, c.Size, sizes[c.Repo+"/"+c.File].sha(), func(have, _ int64) {
			if progress != nil {
				progress(base+have, total)
			}
		})
		if err != nil {
			return fmt.Errorf("%s: %w", path.Base(c.File), err)
		}
		done += c.Size
	}
	return nil
}
