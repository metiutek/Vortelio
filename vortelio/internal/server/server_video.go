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

// Video page API: native text- and image-to-video (Wan on
// stable-diffusion.cpp). Same streaming contract as /api/image/generate.

func nativeVideoModel(m *hub.Model) bool {
	f := hub.ImageFamilyOf(m)
	return m.Type == "video" && f != nil && f.Kind == "video" && strings.EqualFold(filepath.Ext(m.LocalPath), ".gguf")
}

func installedVideoModels() []imageModelInfo {
	ms, _ := hub.NewModelStore().List()
	var out []imageModelInfo
	for _, m := range ms {
		if !nativeVideoModel(m) {
			continue
		}
		ref := &hub.ModelRef{Type: m.Type, Name: m.Name, Tag: m.Tag}
		name := m.DisplayName
		if e, ok := hub.VideoCatalogFor(ref); ok && name == "" {
			name = e.Name
		}
		if name == "" {
			name = m.Name + ":" + m.Tag
		}
		fam := hub.ImageFamilyOf(m)
		desc := m.Description
		if desc == "" {
			desc = fam.Label
		}
		out = append(out, imageModelInfo{Ref: ref.String(), Name: name, Description: desc, Family: fam.ID,
			Native: true, Defaults: fam, SizeBytes: m.SizeBytes})
	}
	return out
}

// GET /api/video/catalog → curated models (with installed flag) + installed models.
func handleVideoCatalog(w http.ResponseWriter, r *http.Request) {
	installed := installedVideoModels()
	have := map[string]bool{}
	for _, m := range installed {
		have[m.Ref] = true
	}
	type row struct {
		hub.ImageCatalogEntry
		Installed bool `json:"installed"`
	}
	var cat []row
	for _, e := range hub.VideoCatalog {
		ref, _ := hub.ParseModelRef(e.Ref)
		cat = append(cat, row{ImageCatalogEntry: e, Installed: ref != nil && have[ref.String()]})
	}
	respond(w, 200, map[string]any{"catalog": cat, "models": installed, "ffmpeg": runtime.FindFFmpeg() != ""})
}

// pickVideoModel returns the named native video model, or the best installed one.
func pickVideoModel(name string) (*hub.Model, error) {
	store := hub.NewModelStore()
	if name = strings.TrimSpace(name); name != "" {
		refName := name
		if !strings.Contains(refName, "/") {
			refName = "video/" + refName
		}
		if ref, err := hub.ParseModelRef(refName); err == nil {
			if m, err := store.Resolve(ref); err == nil && nativeVideoModel(m) {
				return m, nil
			}
		}
	}
	ms, _ := store.List()
	lname := strings.ToLower(name)
	var best *hub.Model
	bestRank := 1 << 30
	for _, m := range ms {
		if !nativeVideoModel(m) {
			continue
		}
		if lname != "" && (strings.Contains(strings.ToLower(m.Name), lname) || strings.Contains(strings.ToLower(m.DisplayName), lname)) {
			return m, nil
		}
		rank := 200
		for i, e := range hub.VideoCatalog {
			if e.Ref == m.Type+"/"+m.Name+":"+m.Tag {
				rank = i
			}
		}
		if rank < bestRank {
			best, bestRank = m, rank
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no video model installed yet — install one from the Video page")
	}
	return best, nil
}

// saveVideo writes a clip to the output folder and returns its path ("" on error).
func saveVideo(data []byte, ext string) string {
	dir := runtime.DefaultOutputDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return ""
	}
	p := filepath.Join(dir, fmt.Sprintf("vortelio-video-%s.%s", time.Now().Format("20060102-150405"), ext))
	if os.WriteFile(p, data, 0o644) != nil {
		return ""
	}
	return p
}

// POST /api/video/generate {model?, prompt, negative_prompt?, width?, height?,
// frames?, fps?, steps?, cfg?, seed?, image? (base64 start picture)}
// → text/event-stream: "progress"… then "done" {video, mime, …} or "error".
func handleVideoGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model    string  `json:"model"`
		Prompt   string  `json:"prompt"`
		Negative string  `json:"negative_prompt"`
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		Frames   int     `json:"frames"`
		FPS      int     `json:"fps"`
		Steps    int     `json:"steps"`
		CFG      float64 `json:"cfg"`
		Seed     *int64  `json:"seed"`
		Image    string  `json:"image"`
		Save     *bool   `json:"save"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 40<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		jsonError(w, 400, "describe the video you want")
		return
	}
	m, err := pickVideoModel(req.Model)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	vr := runtime.VideoRequest{Prompt: req.Prompt, Negative: req.Negative, Width: req.Width, Height: req.Height,
		Frames: req.Frames, FPS: req.FPS, Steps: req.Steps, CFG: req.CFG, Seed: req.Seed}
	if req.Image != "" {
		b64 := req.Image
		if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
			b64 = b64[i+1:]
		}
		img, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(img) == 0 {
			jsonError(w, 400, "the start image must be base64-encoded")
			return
		}
		if f := hub.ImageFamilyOf(m); !f.ImageInput {
			jsonError(w, 400, m.Name+" cannot start from a picture — pick Wan 2.2")
			return
		}
		vr.InitImage = img
	}
	streamGeneration(w, r, func(ctx context.Context, progress runtime.ImageProgress) (any, error) {
		started := time.Now()
		res, err := runtime.NativeGenerateVideo(ctx, m, getHardware(), vr, progress)
		if err != nil {
			return nil, err
		}
		path := ""
		if req.Save == nil || *req.Save {
			path = saveVideo(res.Data, res.Ext)
		}
		return map[string]any{
			"model": m.Type + "/" + m.Name + ":" + m.Tag, "video": base64.StdEncoding.EncodeToString(res.Data),
			"mime": res.Mime, "ext": res.Ext, "path": path, "seed": res.Seed, "width": res.Width, "height": res.Height,
			"frames": res.Frames, "fps": res.FPS, "steps": res.Steps, "elapsed": time.Since(started).Seconds(),
		}, nil
	})
}
