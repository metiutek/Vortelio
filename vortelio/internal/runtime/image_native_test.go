package runtime

import (
	"strings"
	"testing"

	"github.com/vortelio/vortelio/internal/hub"
)

func TestResolveImageRequest(t *testing.T) {
	z := &hub.Model{Type: "image", Family: "z-image", LocalPath: "z_image_turbo-Q3_K.gguf"}
	r := ResolveImageRequest(z, ImageRequest{Prompt: "x", Negative: "blurry", Width: 700, Count: 20})
	if r.Steps != 8 || r.CFG != 1 || r.Sampler != "euler" {
		t.Errorf("defaults not applied: %+v", r)
	}
	if r.Width != 704 || r.Height != 768 {
		t.Errorf("size = %dx%d, want 704x768", r.Width, r.Height)
	}
	if r.Negative != "" {
		t.Error("negative prompt kept for a family without real CFG")
	}
	if r.Count != 8 {
		t.Errorf("count = %d, want 8", r.Count)
	}
	if r.Seed == nil || *r.Seed < 0 {
		t.Error("no random seed")
	}

	sd := &hub.Model{Type: "image", LocalPath: "stable-diffusion-v1-5-Q4_1.gguf"}
	seed := int64(42)
	r = ResolveImageRequest(sd, ImageRequest{Prompt: "x", Negative: "blurry", Steps: 500, Seed: &seed})
	if r.Negative != "blurry" || r.Steps != 150 || r.Width != 512 || *r.Seed != 42 || r.CFG != 7 {
		t.Errorf("sd1: %+v", r)
	}
}

func TestImageServerArgs(t *testing.T) {
	m := &hub.Model{Type: "image", Family: "z-image", LocalPath: "/m/z.gguf",
		Components: map[string]string{"vae": "/s/ae.safetensors", "llm": "/s/qwen.gguf"}}
	args := strings.Join(imageServerArgs(m, hub.ImageFamilies["z-image"], 1234, false), " ")
	for _, want := range []string{"--diffusion-model /m/z.gguf", "--llm /s/qwen.gguf", "--vae /s/ae.safetensors", "--flow-shift 3", "--vae-tiling", "--listen-port 1234"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "--backend") {
		t.Error("GPU run must not force a backend")
	}
	sd := &hub.Model{Type: "image", LocalPath: "/m/sd.gguf"}
	args = strings.Join(imageServerArgs(sd, hub.ImageFamilies["sd1"], 1, true), " ")
	if !strings.HasPrefix(args, "-m /m/sd.gguf") || !strings.Contains(args, "--backend cpu") {
		t.Errorf("sd1 cpu args: %s", args)
	}
}

func TestSDLogListener(t *testing.T) {
	l := &sdLog{tail: tailBuf{max: 1024}}
	var got []string
	l.listen(func(s string) { got = append(got, s) })
	l.Write([]byte("  |=====>   | 1/8 - 3.0s/it\x1b[K\r  |====="))
	l.Write([]byte("=>   | 2/8 - 3.0s/it\r\n[I] decoding 1 latents\n"))
	if len(got) != 3 || !sdSamplingRe.MatchString(got[1]) || got[2] != "[I] decoding 1 latents" {
		t.Errorf("lines = %q", got)
	}
}
