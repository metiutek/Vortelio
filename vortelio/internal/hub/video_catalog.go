package hub

import (
	"path/filepath"
	"strings"
)

// Native video models (stable-diffusion.cpp, Wan). They are diffusion
// families like the image ones (ImageFamilies), with Kind "video": the
// diffusion weights plus a shared VAE and the UMT5 text encoder, stored under
// models/video/_shared.

// wanNegative is Wan's recommended negative prompt (it was trained with it).
const wanNegative = "色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走"

var (
	compWan21VAE = ImageComponent{Role: RoleVAE, Kind: "video", Repo: "Comfy-Org/Wan_2.1_ComfyUI_repackaged", File: "split_files/vae/wan_2.1_vae.safetensors", Size: 253815318}
	compWan22VAE = ImageComponent{Role: RoleVAE, Kind: "video", Repo: "Comfy-Org/Wan_2.2_ComfyUI_Repackaged", File: "split_files/vae/wan2.2_vae.safetensors", Size: 1409400960}
	compUMT5     = ImageComponent{Role: RoleT5XXL, Kind: "video", Repo: "city96/umt5-xxl-encoder-gguf", File: "umt5-xxl-encoder-Q4_K_M.gguf", Size: 3655000000}
)

func init() {
	ImageFamilies["wan2.1-t2v"] = &ImageFamily{ID: "wan2.1-t2v", Label: "Wan 2.1 text-to-video", Kind: "video", MainRole: RoleDiffusion,
		Components: []ImageComponent{compWan21VAE, compUMT5},
		Steps:      20, CFG: 6, Sampler: "euler", FlowShift: 3, Width: 480, Height: 288, Negative: true, VAETiling: true,
		Frames: 17, FPS: 16, DefaultNegative: wanNegative}
	ImageFamilies["wan2.2-ti2v"] = &ImageFamily{ID: "wan2.2-ti2v", Label: "Wan 2.2 text/image-to-video", Kind: "video", MainRole: RoleDiffusion,
		Components: []ImageComponent{compWan22VAE, compUMT5},
		Steps:      20, CFG: 5, Sampler: "euler", FlowShift: 5, Width: 480, Height: 288, Negative: true, VAETiling: true,
		// No default negative: with the long Chinese one (and our UMT5 quant)
		// Wan 2.2 5B turns into colour noise; without it the clip is clean.
		Frames: 17, FPS: 24, ImageInput: true}

	for _, e := range VideoCatalog {
		hfRegistry[e.Ref] = HFEntry{Repo: e.Repo, File: e.File, Format: "gguf", License: e.License}
	}
}

// VideoCatalog is the curated video model list, recommended first.
var VideoCatalog = []ImageCatalogEntry{
	{Ref: "video/wan2.1:1.3b", Name: "Wan 2.1 1.3B", Family: "wan2.1-t2v",
		Description: "Short clips from a description. Small and fast: runs on a 4 GB GPU.",
		Repo:        "samuelchristlie/Wan2.1-T2V-1.3B-GGUF", File: "Wan2.1-T2V-1.3B-Q8_0.gguf", SizeBytes: 1536000000 + compWan21VAE.Size + compUMT5.Size,
		License: "Apache-2.0", Recommended: true},
	{Ref: "video/wan2.2:5b", Name: "Wan 2.2 5B", Family: "wan2.2-ti2v",
		Description: "Can also animate a picture. Experimental on a 4 GB GPU: very slow, and small clips look rough.",
		Repo:        "QuantStack/Wan2.2-TI2V-5B-GGUF", File: "Wan2.2-TI2V-5B-Q4_K_S.gguf", SizeBytes: 3116000000 + compWan22VAE.Size + compUMT5.Size,
		License: "Apache-2.0"},
}

// VideoCatalogFor returns the catalog entry for a model reference, if any.
func VideoCatalogFor(ref *ModelRef) (ImageCatalogEntry, bool) {
	key := ref.Type + "/" + ref.Name + ":" + ref.Tag
	for _, e := range VideoCatalog {
		if e.Ref == key {
			return e, true
		}
	}
	return ImageCatalogEntry{}, false
}

// DetectVideoFamily guesses the family of a video GGUF from its file name.
func DetectVideoFamily(file string) string {
	n := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(filepath.Base(file)))
	switch {
	case strings.Contains(n, "wan22") && strings.Contains(n, "ti2v"):
		return "wan2.2-ti2v"
	case strings.Contains(n, "wan21") && strings.Contains(n, "t2v") && strings.Contains(n, "13b"):
		return "wan2.1-t2v"
	}
	return "" // 14B and image-to-video Wan need files Vortelio does not pick yet
}

// diffusionCatalogFor returns the family and description of a catalog
// image or video model ("" when ref is not in a catalog).
func diffusionCatalogFor(ref *ModelRef) (string, string) {
	if e, ok := ImageCatalogFor(ref); ok {
		return e.Family, e.Description
	}
	if e, ok := VideoCatalogFor(ref); ok {
		return e.Family, e.Description
	}
	return "", ""
}
