package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// Images page API: native text-to-image with stable-diffusion.cpp. The
// generate endpoint streams progress as server-sent events and ends with the
// images (base64 PNG), which are also saved to the output folder.

// imageModelInfo is an installed image model as the UI needs it.
type imageModelInfo struct {
	Ref         string           `json:"ref"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Family      string           `json:"family,omitempty"`
	Native      bool             `json:"native"`
	Defaults    *hub.ImageFamily `json:"defaults,omitempty"`
	SizeBytes   int64            `json:"size_bytes"`
}

func installedImageModels() []imageModelInfo {
	ms, _ := hub.NewModelStore().List()
	var out []imageModelInfo
	for _, m := range ms {
		if m.Type != "image" {
			continue
		}
		ref := &hub.ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
		name := m.DisplayName
		e, inCatalog := hub.ImageCatalogFor(ref)
		if name == "" && inCatalog {
			name = e.Name
		}
		if name == "" {
			name = m.Name + ":" + m.Tag
		}
		info := imageModelInfo{Ref: ref.String(), Name: name, Description: m.Description, SizeBytes: m.SizeBytes}
		if fam := hub.ImageFamilyOf(m); fam != nil && nativeImageModel(m) {
			info.Native, info.Family, info.Defaults = true, fam.ID, fam
			if info.Description == "" {
				info.Description = fam.Label
			}
		}
		out = append(out, info)
	}
	return out
}

// nativeImageModel reports whether m runs on stable-diffusion.cpp.
func nativeImageModel(m *hub.Model) bool {
	return hub.ImageFamilyOf(m) != nil && strings.EqualFold(filepath.Ext(m.LocalPath), ".gguf")
}

// GET /api/image/catalog → curated models (with installed flag) + installed models.
func handleImageCatalog(w http.ResponseWriter, r *http.Request) {
	installed := installedImageModels()
	have := map[string]bool{}
	for _, m := range installed {
		have[m.Ref] = true
	}
	type row struct {
		hub.ImageCatalogEntry
		Installed bool `json:"installed"`
	}
	var cat []row
	for _, e := range hub.ImageCatalog {
		ref, _ := hub.ParseModelRef(e.Ref)
		cat = append(cat, row{ImageCatalogEntry: e, Installed: ref != nil && have[ref.String()]})
	}
	respond(w, 200, map[string]any{"catalog": cat, "models": installed, "families": hub.ImageFamilies})
}

// pickImageModel returns the named image model, or the best installed one:
// native catalog models first (recommended ones before the others), then any
// native model, then the rest.
func pickImageModel(name string) (*hub.Model, error) {
	store := hub.NewModelStore()
	if name = strings.TrimSpace(name); name != "" {
		refName := name
		if !strings.Contains(refName, "/") {
			refName = "image/" + refName
		}
		if ref, err := hub.ParseModelRef(refName); err == nil {
			if m, err := store.Resolve(ref); err == nil && m.Type == "image" {
				return m, nil
			}
		}
	}
	ms, _ := store.List()
	lname := strings.ToLower(name)
	var best *hub.Model
	bestRank := 1 << 30
	for _, m := range ms {
		if m.Type != "image" {
			continue
		}
		if lname != "" && (strings.Contains(strings.ToLower(m.Name), lname) || strings.Contains(strings.ToLower(m.DisplayName), lname)) {
			return m, nil
		}
		rank := 300
		if nativeImageModel(m) {
			rank = 200
		}
		for i, e := range hub.ImageCatalog {
			if e.Ref == m.Type+"/"+m.Name+":"+m.Tag {
				rank = i
				if !e.Recommended {
					rank += 50
				}
			}
		}
		if rank < bestRank {
			best, bestRank = m, rank
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no image model installed yet — install one from the Images page")
	}
	return best, nil
}

// imageGenRequest is the body of /api/image/generate.
type imageGenRequest struct {
	Model     string  `json:"model"`
	Prompt    string  `json:"prompt"`
	Negative  string  `json:"negative_prompt"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Steps     int     `json:"steps"`
	CFG       float64 `json:"cfg"`
	Guidance  float64 `json:"guidance"`
	Sampler   string  `json:"sampler"`
	Scheduler string  `json:"scheduler"`
	Seed      *int64  `json:"seed"`
	Count     int     `json:"count"`
	Save      *bool   `json:"save"` // default true: also write the PNGs to the output folder
}

func (q imageGenRequest) native() runtime.ImageRequest {
	return runtime.ImageRequest{
		Prompt: q.Prompt, Negative: q.Negative, Width: q.Width, Height: q.Height,
		Steps: q.Steps, CFG: q.CFG, Guidance: q.Guidance, Sampler: q.Sampler,
		Scheduler: q.Scheduler, Seed: q.Seed, Count: q.Count,
	}
}

// saveImages writes PNGs to the output folder and returns their paths.
func saveImages(images [][]byte) []string {
	dir := runtime.DefaultOutputDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	stamp := time.Now().Format("20060102-150405")
	var paths []string
	for i, b := range images {
		p := filepath.Join(dir, fmt.Sprintf("vortelio-image-%s-%d.png", stamp, i+1))
		if os.WriteFile(p, b, 0o644) == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

// POST /api/image/generate {model?, prompt, negative_prompt?, width?, height?,
// steps?, cfg?, guidance?, sampler?, scheduler?, seed?, count?}
// → text/event-stream: "progress" {pct, msg}… then "done" {images, seed, …}
// or "error" {error}.
func handleImageGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req imageGenRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		jsonError(w, 400, "describe the image you want")
		return
	}
	m, err := pickImageModel(req.Model)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if !nativeImageModel(m) {
		jsonError(w, 400, m.Name+" is not a native image model — pick one from the Images catalog")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	events := make(chan [2]string, 64)
	send := func(typ string, v any) {
		b, _ := json.Marshal(v)
		select {
		case events <- [2]string{typ, string(b)}:
		default: // never block generation on a slow client
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Minute)
	defer cancel()
	started := time.Now()
	type outcome struct {
		res *runtime.ImageResult
		err error
	}
	fin := make(chan outcome, 1)
	go func() {
		last := -1
		res, err := runtime.NativeGenerateImage(ctx, m, getHardware(), req.native(), func(pct int, msg string) {
			if pct < last {
				pct = last
			}
			last = pct
			send("progress", map[string]any{"pct": pct, "msg": msg})
		})
		fin <- outcome{res, err}
	}()

	write := func(typ, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev := <-events:
			write(ev[0], ev[1])
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		case o := <-fin:
			for len(events) > 0 {
				ev := <-events
				write(ev[0], ev[1])
			}
			if o.err != nil {
				b, _ := json.Marshal(map[string]string{"error": o.err.Error()})
				write("error", string(b))
				return
			}
			var paths []string
			if req.Save == nil || *req.Save {
				paths = saveImages(o.res.Images)
			}
			imgs := make([]string, len(o.res.Images))
			for i, b := range o.res.Images {
				imgs[i] = base64.StdEncoding.EncodeToString(b)
			}
			b, _ := json.Marshal(map[string]any{
				"model": m.Type + "/" + m.Name + ":" + m.Tag, "images": imgs, "paths": paths,
				"seed": o.res.Seed, "width": o.res.Width, "height": o.res.Height, "steps": o.res.Steps,
				"elapsed": time.Since(started).Seconds(),
			})
			write("done", string(b))
			return
		}
	}
}

// POST /api/image/unload → stop the image engine and free its memory.
func handleImageUnload(w http.ResponseWriter, r *http.Request) {
	runtime.StopImageServer()
	respond(w, 200, map[string]any{"status": "ok"})
}
