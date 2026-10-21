package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vortelio/vortelio/internal/hub"
)

// Native video: Wan models on the same stable-diffusion.cpp server as the
// images (POST /sdcpp/v1/vid_gen). The engine writes MJPEG AVI, which
// browsers cannot play: it is converted to H.264 MP4 when ffmpeg is
// installed, otherwise to an animated GIF in pure Go.

// VideoRequest is one text- or image-to-video generation. Zero fields take
// the family's defaults.
type VideoRequest struct {
	Prompt    string
	Negative  string
	Width     int
	Height    int
	Frames    int // rounded to 4n+1
	FPS       int
	Steps     int
	CFG       float64
	Seed      *int64
	InitImage []byte // PNG/JPEG: animate this picture (families with ImageInput)
	ForceCPU  bool
}

// VideoResult is a generated clip.
type VideoResult struct {
	Data   []byte // MP4 or GIF
	Mime   string
	Ext    string
	AVI    []byte // the engine's original file
	Seed   int64
	Width  int
	Height int
	Frames int
	FPS    int
	Steps  int
}

// ResolveVideoRequest fills the zero fields of r with the family defaults
// and clamps the values to what the engine accepts.
func ResolveVideoRequest(m *hub.Model, r VideoRequest) VideoRequest {
	fam := hub.ImageFamilyOf(m)
	if fam == nil || fam.Kind != "video" {
		fam = hub.ImageFamilies["wan2.1-t2v"]
	}
	if r.Steps <= 0 {
		r.Steps = fam.Steps
	}
	r.Steps = min(r.Steps, 100)
	if r.CFG <= 0 {
		r.CFG = fam.CFG
	}
	if r.Width <= 0 {
		r.Width = fam.Width
	}
	if r.Height <= 0 {
		r.Height = fam.Height
	}
	// Multiples of 32 between 256 and 1280.
	snap := func(v int) int { return max(256, min(1280, (v+16)/32*32)) }
	r.Width, r.Height = snap(r.Width), snap(r.Height)
	if r.Frames <= 0 {
		r.Frames = fam.Frames
	}
	r.Frames = max(5, min(121, (r.Frames-1+2)/4*4+1))
	if r.FPS <= 0 {
		r.FPS = fam.FPS
	}
	r.FPS = max(4, min(30, r.FPS))
	if strings.TrimSpace(r.Negative) == "" {
		r.Negative = fam.DefaultNegative
	}
	if !fam.ImageInput {
		r.InitImage = nil
	}
	if r.Seed == nil || *r.Seed < 0 {
		s := rand.Int63n(1 << 31)
		r.Seed = &s
	}
	return r
}

// NativeGenerateVideo generates a clip with stable-diffusion.cpp.
func NativeGenerateVideo(ctx context.Context, m *hub.Model, hw *Hardware, r VideoRequest, progress ImageProgress) (*VideoResult, error) {
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, errors.New("describe the video you want")
	}
	if fam := hub.ImageFamilyOf(m); fam == nil || fam.Kind != "video" {
		return nil, fmt.Errorf("%s is not a native video model — install one from the Video page", m.Name)
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	r = ResolveVideoRequest(m, r)
	p, release, err := acquireImage(ctx, m, hw, r.ForceCPU, func(msg string) { progress(2, msg) })
	if err != nil {
		return nil, err
	}
	defer release()

	// Sampling (Steps steps) takes most of the time, then the VAE decodes the
	// frames in tiles (another N/M bar).
	sampled := false
	p.log.listen(func(line string) {
		mm := sdSamplingRe.FindStringSubmatch(line)
		if mm == nil {
			return
		}
		cur, _ := strconv.Atoi(mm[1])
		tot, _ := strconv.Atoi(mm[2])
		if tot <= 0 {
			return
		}
		if tot == r.Steps && !sampled {
			progress(5+cur*70/tot, fmt.Sprintf("Step %d/%d", cur, tot))
			if cur == tot {
				sampled = true
			}
			return
		}
		if sampled {
			progress(75+cur*20/tot, "Decoding the frames…")
		}
	})
	defer p.log.listen(nil)
	progress(4, "Reading the prompt…")

	body := map[string]any{
		"prompt": r.Prompt, "negative_prompt": r.Negative,
		"width": r.Width, "height": r.Height, "video_frames": r.Frames, "fps": r.FPS,
		"seed": *r.Seed, "output_format": "avi", "output_compression": 95,
		"sample_params": map[string]any{
			"sample_steps": r.Steps, "sample_method": "euler",
			"guidance": map[string]any{"txt_cfg": r.CFG},
		},
	}
	if len(r.InitImage) > 0 {
		img, err := fitInitImage(r.InitImage, r.Width, r.Height)
		if err != nil {
			return nil, err
		}
		body["init_image"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	}
	raw, err := p.runJob(ctx, "/sdcpp/v1/vid_gen", body)
	if err != nil {
		return nil, err
	}
	var res struct {
		B64    string `json:"b64_json"`
		Frames int    `json:"frame_count"`
	}
	json.Unmarshal(raw, &res)
	avi, err := base64.StdEncoding.DecodeString(res.B64)
	if err != nil || len(avi) == 0 {
		return nil, errors.New("the video engine returned no video")
	}
	progress(97, "Encoding the video…")
	out := &VideoResult{AVI: avi, Seed: *r.Seed, Width: r.Width, Height: r.Height, Frames: r.Frames, FPS: r.FPS, Steps: r.Steps}
	if res.Frames > 0 {
		out.Frames = res.Frames
	}
	if mp4, err := aviToMP4(ctx, avi); err == nil {
		out.Data, out.Mime, out.Ext = mp4, "video/mp4", "mp4"
		return out, nil
	}
	g, err := aviToGIF(avi, r.FPS)
	if err != nil {
		return nil, fmt.Errorf("cannot convert the video: %w", err)
	}
	out.Data, out.Mime, out.Ext = g, "image/gif", "gif"
	return out, nil
}

// fitInitImage scales and crops a picture to w×h (cover) and returns a PNG.
func fitInitImage(data []byte, w, h int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the start image must be a PNG or JPEG picture")
	}
	sb := src.Bounds()
	scale := max(float64(w)/float64(sb.Dx()), float64(h)/float64(sb.Dy()))
	cw, ch := int(float64(w)/scale), int(float64(h)/scale)
	x0, y0 := sb.Min.X+(sb.Dx()-cw)/2, sb.Min.Y+(sb.Dy()-ch)/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ { // nearest-neighbour sampling: fine for a start frame
		sy := y0 + y*ch/h
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(x0+x*cw/w, sy))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// aviToMP4 re-encodes the engine's AVI as H.264 MP4 with ffmpeg.
func aviToMP4(ctx context.Context, avi []byte) ([]byte, error) {
	ff := FindFFmpeg()
	if ff == "" {
		return nil, errors.New("ffmpeg not installed")
	}
	dir, err := os.MkdirTemp("", "vortelio-video-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.avi"), filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(in, avi, 0o644); err != nil {
		return nil, err
	}
	cmd := HideWindow(exec.CommandContext(ctx, ff, "-v", "error", "-y", "-i", in,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "18", "-movflags", "+faststart", out))
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(string(msg)))
	}
	return os.ReadFile(out)
}

// aviFrames returns the JPEG frames of an MJPEG AVI.
func aviFrames(avi []byte) ([][]byte, error) {
	i := bytes.Index(avi, []byte("movi"))
	if len(avi) < 12 || string(avi[:4]) != "RIFF" || i < 0 {
		return nil, errors.New("not an AVI file")
	}
	var frames [][]byte
	for pos := i + 4; pos+8 <= len(avi); {
		id := string(avi[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(avi[pos+4 : pos+8]))
		if id == "idx1" || pos+8+size > len(avi) {
			break
		}
		if id == "LIST" { // nested list (rec ): step inside it
			pos += 12
			continue
		}
		if strings.HasSuffix(id, "dc") || strings.HasSuffix(id, "db") {
			frames = append(frames, avi[pos+8:pos+8+size])
		}
		pos += 8 + size + size%2
	}
	if len(frames) == 0 {
		return nil, errors.New("the AVI has no video frames")
	}
	return frames, nil
}

// aviToGIF turns an MJPEG AVI into a looping animated GIF.
func aviToGIF(avi []byte, fps int) ([]byte, error) {
	frames, err := aviFrames(avi)
	if err != nil {
		return nil, err
	}
	delay := max(2, 100/max(1, fps))
	g := &gif.GIF{LoopCount: 0}
	for _, f := range frames {
		img, err := jpeg.Decode(bytes.NewReader(f))
		if err != nil {
			continue
		}
		pal := image.NewPaletted(img.Bounds(), palette.Plan9)
		draw.FloydSteinberg.Draw(pal, img.Bounds(), img, image.Point{})
		g.Image = append(g.Image, pal)
		g.Delay = append(g.Delay, delay)
	}
	if len(g.Image) == 0 {
		return nil, errors.New("no readable frames")
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
