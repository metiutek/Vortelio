package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
)

// Native images: stable-diffusion.cpp's sd-server loads a model (and its
// family's VAE and text encoders) once and serves every generation over
// HTTP, like llama-server does for chat. One model at a time; stopped after
// ImageIdleTimeout or when another kind of model needs the GPU.
//
// The API is asynchronous: POST /sdcpp/v1/img_gen returns a job id that is
// polled at /sdcpp/v1/jobs/<id>. The job has no step counter, so progress is
// read from the "N/M - x s/it" lines the server prints while sampling.

// ImageIdleTimeout stops an unused image server.
var ImageIdleTimeout = 5 * time.Minute

type imageProc struct {
	key  string
	url  string
	cmd  *exec.Cmd
	done chan struct{}
	log  *sdLog
	used time.Time
	busy int
}

func (p *imageProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *imageProc) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	}
}

// sdLog keeps the tail of the server output and passes every line (split on
// \r too: progress bars redraw in place) to the active listener.
type sdLog struct {
	mu       sync.Mutex
	tail     tailBuf
	partial  []byte
	listener func(string)
}

func (l *sdLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail.Write(p)
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexAny(l.partial, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(stripANSI(string(l.partial[:i])))
		l.partial = l.partial[i+1:]
		if line != "" && l.listener != nil {
			l.listener(line)
		}
	}
	if len(l.partial) > 64*1024 {
		l.partial = nil
	}
	return len(p), nil
}

func (l *sdLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tail.String()
}

func (l *sdLog) listen(fn func(string)) {
	l.mu.Lock()
	l.listener = fn
	l.mu.Unlock()
}

// lastSDLines returns the last n meaningful log lines (progress bars dropped).
func lastSDLines(s string, n int) string {
	var keep []string
	for _, ln := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		ln = strings.TrimSpace(stripANSI(ln))
		if ln == "" || sdSamplingRe.MatchString(ln) || strings.Contains(ln, "MB/s") {
			continue
		}
		keep = append(keep, ln)
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

var (
	imageMu   sync.Mutex
	imageCur  *imageProc
	imageOnce sync.Once
)

// StopImageServer stops the running image server (frees its memory).
func StopImageServer() {
	imageMu.Lock()
	defer imageMu.Unlock()
	if imageCur != nil {
		imageCur.stop()
		imageCur = nil
	}
}

func imageInit() {
	imageOnce.Do(func() {
		RegisterGPUEvictor("image", StopImageServer)
		OnEngineReplace("sdcpp", StopImageServer)
		go func() {
			for range time.Tick(30 * time.Second) {
				imageMu.Lock()
				if imageCur != nil && imageCur.busy == 0 && time.Since(imageCur.used) > ImageIdleTimeout {
					imageCur.stop()
					imageCur = nil
				}
				imageMu.Unlock()
			}
		}()
	})
}

// imageServerArgs builds the sd-server command line for m.
func imageServerArgs(m *hub.Model, fam *hub.ImageFamily, port int, cpu bool) []string {
	main := "-m"
	if fam.MainRole == hub.RoleDiffusion {
		main = "--diffusion-model"
	}
	args := []string{main, m.LocalPath}
	roles := make([]string, 0, len(m.Components))
	for r := range m.Components {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		args = append(args, "--"+r, m.Components[r])
	}
	if fam.FlowShift > 0 {
		args = append(args, "--flow-shift", strconv.FormatFloat(fam.FlowShift, 'f', -1, 64))
	}
	if fam.VAETiling {
		args = append(args, "--vae-tiling")
	}
	if cpu {
		args = append(args, "--backend", "cpu")
	}
	return append(args, "-l", "127.0.0.1", "--listen-port", strconv.Itoa(port))
}

// acquireImage returns a running server for m and marks it busy; call the
// returned release when the request is done.
func acquireImage(ctx context.Context, m *hub.Model, hw *Hardware, forceCPU bool, status func(string)) (*imageProc, func(), error) {
	imageInit()
	fam := hub.ImageFamilyOf(m)
	if fam == nil {
		return nil, nil, fmt.Errorf("Vortelio does not know how to run %s — use a model from the Images catalog", filepath.Base(m.LocalPath))
	}
	if missing := hub.MissingImageComponents(m); len(missing) > 0 {
		status("Downloading the files this model needs…")
		if err := hub.EnsureImageComponents(ctx, m, nil); err != nil {
			return nil, nil, fmt.Errorf("downloading the model's extra files: %w", err)
		}
	} else if len(m.Components) != len(fam.Components) {
		_ = hub.EnsureImageComponents(ctx, m, nil) // fills the manifest of older models
	}
	cpu := forceCPU || (hw != nil && hw.Backend != BackendCUDA && hw.Backend != BackendMetal && hw.Backend != BackendROCm)
	key := fmt.Sprintf("%s|%v|%v", m.LocalPath, m.Components, cpu)
	if !cpu {
		AcquireGPU("image")
	}
	imageMu.Lock()
	defer imageMu.Unlock()
	if imageCur != nil && imageCur.alive() && imageCur.key == key {
		imageCur.busy++
		imageCur.used = time.Now()
		p := imageCur
		return p, releaseImage(p), nil
	}
	if imageCur != nil {
		imageCur.stop()
		imageCur = nil
	}
	bin, err := EnsureEngine(ctx, "sdcpp", hw)
	if err != nil {
		return nil, nil, fmt.Errorf("image engine: %w", err)
	}
	status("Loading the model…")
	p, err := startImageServer(ctx, bin, m, fam, cpu)
	if err != nil && !cpu && isOOM(err) {
		status("Not enough GPU memory — loading on the CPU (slower)…")
		p, err = startImageServer(ctx, bin, m, fam, true)
	}
	if err != nil {
		return nil, nil, err
	}
	p.key = key
	p.busy = 1
	p.used = time.Now()
	imageCur = p
	return p, releaseImage(p), nil
}

func releaseImage(p *imageProc) func() {
	return func() {
		imageMu.Lock()
		p.busy--
		p.used = time.Now()
		imageMu.Unlock()
	}
}

func startImageServer(ctx context.Context, bin string, m *hub.Model, fam *hub.ImageFamily, cpu bool) (*imageProc, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	p := &imageProc{url: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{}),
		log: &sdLog{tail: tailBuf{max: 32768}}}
	cmd := exec.Command(bin, imageServerArgs(m, fam, port, cpu)...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout, cmd.Stderr = p.log, p.log
	HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", filepath.Base(bin), err)
	}
	p.cmd = cmd
	go func() { cmd.Wait(); close(p.done) }()

	// The server binds its port only after the model is loaded, so the
	// first 200 from /v1/models means ready.
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return nil, fmt.Errorf("the image engine stopped while loading %s: %s", m.Name, lastSDLines(p.log.String(), 6))
		case <-ctx.Done():
			p.stop()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := client.Get(p.url + "/v1/models")
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p, nil
		}
	}
	p.stop()
	return nil, fmt.Errorf("the image engine did not become ready: %s", lastSDLines(p.log.String(), 6))
}

// ── requests ────────────────────────────────────────────────────────────────

// ImageRequest is one text-to-image generation. Zero fields take the
// family's defaults.
type ImageRequest struct {
	Prompt    string
	Negative  string
	Width     int
	Height    int
	Steps     int
	CFG       float64
	Guidance  float64
	Sampler   string
	Scheduler string
	Seed      *int64 // nil or < 0 = random
	Count     int    // images, 1–8
	ForceCPU  bool
}

// ImageResult is the outcome of a generation.
type ImageResult struct {
	Images [][]byte // PNG
	Seed   int64    // seed of the first image; the others use Seed+1, Seed+2…
	Width  int
	Height int
	Steps  int
}

// ImageProgress reports generation progress (0–100) with a short message.
type ImageProgress func(pct int, msg string)

// ResolveImageRequest fills the zero fields of r with the family defaults
// and clamps the values to what the engine accepts.
func ResolveImageRequest(m *hub.Model, r ImageRequest) ImageRequest {
	fam := hub.ImageFamilyOf(m)
	if fam == nil {
		fam = hub.ImageFamilies["sd1"]
	}
	if r.Steps <= 0 {
		r.Steps = fam.Steps
	}
	if r.Steps > 150 {
		r.Steps = 150
	}
	if r.CFG <= 0 {
		r.CFG = fam.CFG
	}
	if r.Guidance <= 0 {
		r.Guidance = fam.Guidance
	}
	if r.Sampler == "" {
		r.Sampler = fam.Sampler
	}
	if r.Scheduler == "" {
		r.Scheduler = fam.Scheduler
	}
	if r.Width <= 0 {
		r.Width = fam.Width
	}
	if r.Height <= 0 {
		r.Height = fam.Height
	}
	// Multiples of 64 between 256 and 2048.
	snap := func(v int) int {
		v = (v + 32) / 64 * 64
		return max(256, min(2048, v))
	}
	r.Width, r.Height = snap(r.Width), snap(r.Height)
	if !fam.Negative {
		r.Negative = "" // no effect without real CFG
	}
	r.Count = max(1, min(8, r.Count))
	if r.Seed == nil || *r.Seed < 0 {
		s := rand.Int63n(1 << 31)
		r.Seed = &s
	}
	return r
}

// NativeGenerateImage generates images with stable-diffusion.cpp.
func NativeGenerateImage(ctx context.Context, m *hub.Model, hw *Hardware, r ImageRequest, progress ImageProgress) (*ImageResult, error) {
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, errors.New("describe the image you want")
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	r = ResolveImageRequest(m, r)
	p, release, err := acquireImage(ctx, m, hw, r.ForceCPU, func(msg string) { progress(3, msg) })
	if err != nil {
		return nil, err
	}
	defer release()

	// Progress: each image samples Steps steps, then decodes.
	img := 0
	p.log.listen(func(line string) {
		if mm := sdSamplingRe.FindStringSubmatch(line); mm != nil {
			cur, _ := strconv.Atoi(mm[1])
			tot, _ := strconv.Atoi(mm[2])
			if tot > 0 && tot == r.Steps {
				if cur == 1 && img < r.Count {
					img++
				}
				done := float64(img-1) + float64(cur)/float64(tot)*0.9
				msg := fmt.Sprintf("Step %d/%d", cur, tot)
				if r.Count > 1 {
					msg = fmt.Sprintf("Image %d/%d · step %d/%d", img, r.Count, cur, tot)
				}
				progress(5+int(done/float64(r.Count)*92), msg)
			}
			return
		}
		if strings.Contains(line, "decoding") && strings.Contains(line, "latent") {
			progress(5+int((float64(img)-0.05)/float64(r.Count)*92), "Decoding…")
		}
	})
	defer p.log.listen(nil)
	progress(5, "Reading the prompt…")

	guidance := map[string]any{"txt_cfg": r.CFG}
	if r.Guidance > 0 {
		guidance["distilled_guidance"] = r.Guidance
	}
	sample := map[string]any{"sample_steps": r.Steps, "guidance": guidance}
	if r.Sampler != "" {
		sample["sample_method"] = r.Sampler
	}
	if r.Scheduler != "" {
		sample["scheduler"] = r.Scheduler
	}
	body := map[string]any{
		"prompt": r.Prompt, "negative_prompt": r.Negative,
		"width": r.Width, "height": r.Height, "seed": *r.Seed,
		"batch_count": r.Count, "output_format": "png", "sample_params": sample,
	}
	images, err := p.runJob(ctx, body)
	if err != nil {
		return nil, err
	}
	return &ImageResult{Images: images, Seed: *r.Seed, Width: r.Width, Height: r.Height, Steps: r.Steps}, nil
}

var imageHTTP = &http.Client{Timeout: 30 * time.Second}

// runJob submits an img_gen job and waits for its images.
func (p *imageProc) runJob(ctx context.Context, body map[string]any) ([][]byte, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/sdcpp/v1/img_gen", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := imageHTTP.Do(req)
	if err != nil {
		return nil, p.failure(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var job struct {
		ID    string `json:"id"`
		Error any    `json:"error"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(out, &job) != nil || job.ID == "" {
		return nil, fmt.Errorf("the image engine refused the request: %s", strings.TrimSpace(string(out[:min(len(out), 300)])))
	}

	for {
		select {
		case <-ctx.Done():
			// A running job cannot be interrupted: stop the server so the GPU
			// is free (it reloads on the next request).
			cr, _ := http.NewRequest(http.MethodPost, p.url+"/sdcpp/v1/jobs/"+job.ID+"/cancel", nil)
			if r, err := imageHTTP.Do(cr); err == nil {
				r.Body.Close()
			}
			StopImageServer()
			return nil, ctx.Err()
		case <-p.done:
			return nil, p.failure(nil)
		case <-time.After(700 * time.Millisecond):
		}
		jr, err := imageHTTP.Get(p.url + "/sdcpp/v1/jobs/" + job.ID)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(jr.Body)
		jr.Body.Close()
		if jr.StatusCode == http.StatusNotFound || jr.StatusCode == http.StatusGone {
			return nil, errors.New("the image engine lost the job")
		}
		if jr.StatusCode != http.StatusOK {
			continue
		}
		var st struct {
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result struct {
				Images []struct {
					Index int    `json:"index"`
					B64   string `json:"b64_json"`
				} `json:"images"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		switch st.Status {
		case "completed":
			sort.Slice(st.Result.Images, func(i, j int) bool { return st.Result.Images[i].Index < st.Result.Images[j].Index })
			var imgs [][]byte
			for _, im := range st.Result.Images {
				if b, err := base64.StdEncoding.DecodeString(im.B64); err == nil && len(b) > 0 {
					imgs = append(imgs, b)
				}
			}
			if len(imgs) == 0 {
				return nil, errors.New("the image engine returned no image")
			}
			return imgs, nil
		case "failed":
			msg := "generation failed"
			if st.Error != nil && st.Error.Message != "" {
				msg = st.Error.Message
			}
			if tail := lastSDLines(p.log.String(), 3); tail != "" {
				msg += " — " + tail
			}
			return nil, errors.New(msg)
		case "cancelled":
			return nil, context.Canceled
		}
	}
}

func (p *imageProc) failure(err error) error {
	if !p.alive() {
		tail := lastSDLines(p.log.String(), 4)
		if isOOM(errors.New(tail)) {
			return fmt.Errorf("not enough memory for this image size — try a smaller size: %s", tail)
		}
		return fmt.Errorf("the image engine stopped: %s", tail)
	}
	return err
}

// writeImages writes res to output (the first image) and output_2.png… for
// the others; it returns the written paths.
func writeImages(res *ImageResult, output string) ([]string, error) {
	var paths []string
	ext := filepath.Ext(output)
	stem := strings.TrimSuffix(output, ext)
	for i, b := range res.Images {
		p := output
		if i > 0 {
			p = fmt.Sprintf("%s_%d%s", stem, i+1, ext)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}
