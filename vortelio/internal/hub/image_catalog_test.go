package hub

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectImageFamily(t *testing.T) {
	cases := map[string]string{
		"stable-diffusion-v1-5-Q4_1.gguf":        "sd1",
		"dreamshaper-8-Q5_0.gguf":                "sd1",
		"stable-diffusion-xl-base-1.0-Q4_1.gguf": "sdxl",
		"RealVisXL_V4.0-Q5_0.gguf":               "sdxl",
		"flux1-schnell-Q4_K_S.gguf":              "flux-schnell",
		"flux1-dev-Q4_K_S.gguf":                  "flux-dev",
		"z_image_turbo-Q3_K.gguf":                "z-image",
		"z-image-turbo-Q4_K_M.gguf":              "z-image",
		"qwen-image-Q4_K.gguf":                   "",
		"sd3.5_medium-Q4_0.gguf":                 "",
	}
	for file, want := range cases {
		if got := DetectImageFamily(file); got != want {
			t.Errorf("DetectImageFamily(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestImageCatalogFamilies(t *testing.T) {
	for _, e := range ImageCatalog {
		if ImageFamilies[e.Family] == nil {
			t.Errorf("%s: unknown family %q", e.Ref, e.Family)
		}
		if got := DetectImageFamily(e.File); got != e.Family {
			t.Errorf("%s: file %s detected as %q, catalog says %q", e.Ref, e.File, got, e.Family)
		}
		if _, ok := hfRegistry[e.Ref]; !ok {
			t.Errorf("%s not registered", e.Ref)
		}
	}
}

func TestSharedComponentPath(t *testing.T) {
	p := sharedComponentPath(ImageComponent{Repo: "second-state/FLUX.1-schnell-GGUF", File: "ae.safetensors"})
	want := filepath.Join("image", "_shared", "second-state__flux-1-schnell-gguf", "ae.safetensors")
	if !strings.HasSuffix(p, want) {
		t.Errorf("got %s, want suffix %s", p, want)
	}
}

func TestDescribeImageModel(t *testing.T) {
	e, _ := ImageCatalogFor(&ModelRef{Type: "image", Name: "z-image", Tag: "turbo"})
	m := &Model{Type: "image", LocalPath: "/x/z_image_turbo-Q3_K.gguf"}
	describeImageModel(m, e.Family, e.Description)
	if m.Engine != "sdcpp" || m.Family != "z-image" {
		t.Fatalf("engine/family = %s/%s", m.Engine, m.Family)
	}
	if m.Components[RoleVAE] == "" || m.Components[RoleLLM] == "" || len(m.Components) != 2 {
		t.Errorf("components = %v", m.Components)
	}
	sd := &Model{Type: "image", LocalPath: "/x/stable-diffusion-v1-5-Q4_1.gguf"}
	describeImageModel(sd, "", "")
	if sd.Family != "sd1" || len(sd.Components) != 0 {
		t.Errorf("sd1: family %s components %v", sd.Family, sd.Components)
	}
}
