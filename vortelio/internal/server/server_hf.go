package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// ramReserveBytes is held back from physical RAM for the OS and other apps when
// judging whether a model "fits".
const ramReserveBytes = int64(2) * 1024 * 1024 * 1024

// estModelBytes estimates the on-disk / in-memory footprint of a model from its
// approximate parameter count. GGUF builds are assumed ~Q4_K_M (~0.6 bytes per
// weight); everything else is assumed fp16 (2 bytes per weight). Returns 0 when
// the parameter count is unknown.
func estModelBytes(paramsB float64, gguf bool) int64 {
	if paramsB <= 0 {
		return 0
	}
	bytesPerParam := 2.0 // fp16
	if gguf {
		bytesPerParam = 0.6 // ~Q4_K_M quant
	}
	return int64(paramsB * 1e9 * bytesPerParam)
}

// memCapacityBytes returns the usable memory budget for running a model:
// available RAM (minus an OS reserve) plus any GPU VRAM, since llama.cpp can
// offload layers to the GPU.
func memCapacityBytes() int64 {
	total, _ := runtime.SystemRAM()
	usableRAM := total - ramReserveBytes
	if usableRAM < 0 {
		usableRAM = 0
	}
	var vram int64
	if hw := getHardware(); hw != nil {
		vram = hw.VRAM
	}
	return usableRAM + vram
}

// fitVerdict classifies an estimated model size against the machine's memory
// budget: "ok" (comfortable), "tight" (runs but near the limit), "over" (too
// large), or "unknown" (size could not be estimated).
func fitVerdict(estBytes int64) string {
	if estBytes <= 0 {
		return "unknown"
	}
	cap := memCapacityBytes()
	if cap <= 0 {
		return "unknown"
	}
	switch {
	case estBytes > cap:
		return "over"
	case float64(estBytes) > float64(cap)*0.75:
		return "tight"
	default:
		return "ok"
	}
}

// hfParamsRE pulls an approximate parameter count from a model id (e.g. "7B",
// "1.5b", "8x7B") so the UI can filter/sort by model "weight".
var hfParamsRE = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*b\b`)

func hfParamsB(id string) float64 {
	// Look at the last path segment (the model name) to avoid matching org names.
	name := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		name = id[i+1:]
	}
	if m := hfParamsRE.FindStringSubmatch(name); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return v
		}
	}
	return 0
}

// hfCache keeps Hugging Face answers for a few minutes: typing, switching
// filters and reopening a model are instant and do not hit the rate limit.
var hfCache = struct {
	sync.Mutex
	m map[string]hfCached
}{m: map[string]hfCached{}}

type hfCached struct {
	at time.Time
	v  any
}

func hfCacheGet(key string) (any, bool) {
	hfCache.Lock()
	defer hfCache.Unlock()
	c, ok := hfCache.m[key]
	if !ok || time.Since(c.at) > 10*time.Minute {
		return nil, false
	}
	return c.v, true
}

func hfCachePut(key string, v any) {
	hfCache.Lock()
	defer hfCache.Unlock()
	if len(hfCache.m) > 300 {
		hfCache.m = map[string]hfCached{}
	}
	hfCache.m[key] = hfCached{time.Now(), v}
}

// hfPipelines maps a Vortelio type to the Hugging Face pipeline tag searched.
var hfPipelines = map[string]string{"llm": "text-generation", "image": "text-to-image", "video": "text-to-video", "3d": "image-to-3d"}

type hfSearchModel struct {
	ID        string  `json:"id"`
	Author    string  `json:"author"`
	Downloads int     `json:"downloads"`
	Likes     int     `json:"likes"`
	Task      string  `json:"task"`
	GGUF      bool    `json:"gguf"`
	Type      string  `json:"type"`
	Params    float64 `json:"params"` // approx. parameter count in billions (0 = unknown)
	EstGB     float64 `json:"est_gb"` // estimated memory footprint in GB (0 = unknown)
	Fit       string  `json:"fit"`    // ok | tight | over | unknown — fits this machine's RAM/VRAM
	Updated   string  `json:"updated"`
}

// handleHFSearch searches Hugging Face so users can find and download models
// from Vortelio. Without a query it lists what is trending.
// GET /api/hf/search?q=...&sort=downloads|likes|recent|trending&gguf=true&type=llm
func handleHFSearch(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	sortParam := "downloads"
	switch qv.Get("sort") {
	case "likes":
		sortParam = "likes"
	case "recent", "modified":
		sortParam = "lastModified"
	case "trending":
		sortParam = "trendingScore"
	}
	if q == "" && qv.Get("sort") == "" {
		sortParam = "trendingScore"
	}
	api := "https://huggingface.co/api/models?limit=50&full=false&direction=-1&sort=" + sortParam
	if q != "" {
		api += "&search=" + url.QueryEscape(q)
	}
	if qv.Get("gguf") == "true" {
		api += "&filter=gguf"
	}
	if p := hfPipelines[qv.Get("type")]; p != "" {
		api += "&pipeline_tag=" + p
	}
	for _, f := range []string{"author", "downloads", "likes", "pipeline_tag", "tags", "library_name", "lastModified"} {
		api += "&expand[]=" + f
	}

	var out []hfSearchModel
	if v, ok := hfCacheGet(api); ok {
		out = v.([]hfSearchModel)
	} else {
		var raw []struct {
			ID           string   `json:"id"`
			Author       string   `json:"author"`
			Likes        int      `json:"likes"`
			Downloads    int      `json:"downloads"`
			PipelineTag  string   `json:"pipeline_tag"`
			Tags         []string `json:"tags"`
			LibraryName  string   `json:"library_name"`
			LastModified string   `json:"lastModified"`
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", api, nil)
		req.Header.Set("User-Agent", "Vortelio")
		if t := hub.HFToken(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			jsonError(w, 502, "Hugging Face is not reachable: "+err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			jsonError(w, 502, "Hugging Face: "+resp.Status)
			return
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			jsonError(w, 502, "invalid answer from Hugging Face")
			return
		}
		out = make([]hfSearchModel, 0, len(raw))
		for _, m := range raw {
			gguf := strings.EqualFold(m.LibraryName, "gguf")
			for _, t := range m.Tags {
				if strings.EqualFold(t, "gguf") {
					gguf = true
				}
			}
			author := m.Author
			if author == "" {
				author, _, _ = strings.Cut(m.ID, "/")
			}
			paramsB := hfParamsB(m.ID)
			estBytes := estModelBytes(paramsB, gguf)
			out = append(out, hfSearchModel{
				ID: m.ID, Author: author, Downloads: m.Downloads, Likes: m.Likes, Task: m.PipelineTag, GGUF: gguf,
				Type: hfTypeFor(m.PipelineTag, m.Tags), Params: paramsB, EstGB: float64(estBytes) / 1e9,
				Updated: m.LastModified,
			})
		}
		hfCachePut(api, out)
	}
	// The verdict depends on this machine (and on what is free right now).
	for i := range out {
		out[i].Fit = fitVerdict(int64(out[i].EstGB * 1e9))
	}
	total, avail := runtime.SystemRAM()
	var vram int64
	if hw := getHardware(); hw != nil {
		vram = hw.VRAM
	}
	respond(w, 200, map[string]interface{}{
		"models": out,
		"system": map[string]interface{}{
			"ram_total_gb": float64(total) / 1e9,
			"ram_avail_gb": float64(avail) / 1e9,
			"vram_gb":      float64(vram) / 1e9,
		},
	})
}

// handleHFModel returns one repository: model card (README), GGUF quants with
// their real size and whether each fits this machine, licence and stats.
// GET /api/hf/model?id=owner/name
func handleHFModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	key := "repo:" + id
	var repo *hub.HFRepo
	if v, ok := hfCacheGet(key); ok {
		repo = v.(*hub.HFRepo)
	} else {
		var err error
		repo, err = hub.FetchHFRepo(r.Context(), id)
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		hfCachePut(key, repo)
	}
	type fileOut struct {
		hub.HFRepoFile
		Fit string `json:"fit"`
	}
	files := make([]fileOut, 0, len(repo.Files))
	for _, f := range repo.Files {
		// Running a GGUF needs about its size plus the context cache.
		files = append(files, fileOut{f, fitVerdict(f.Size + f.Size/8)})
	}
	respond(w, 200, map[string]any{"repo": repo, "files": files, "type": hfTypeFor(repo.Pipeline, repo.Tags)})
}

// handleSystemResources reports the machine's memory budget so the UI can tell
// the user, up front, which models will fit.
// GET /api/system/resources
func handleSystemResources(w http.ResponseWriter, r *http.Request) {
	total, avail := runtime.SystemRAM()
	hw := getHardware()
	var vram int64
	device := "CPU"
	if hw != nil {
		vram = hw.VRAM
		device = hw.DeviceName
	}
	respond(w, 200, map[string]interface{}{
		"ram_total_gb": float64(total) / 1e9,
		"ram_avail_gb": float64(avail) / 1e9,
		"vram_gb":      float64(vram) / 1e9,
		"device":       device,
		"budget_gb":    float64(memCapacityBytes()) / 1e9,
	})
}

// hfTypeFor maps a HF pipeline tag to a Vortelio model type prefix. Chat
// models that also read images ("image-text-to-text") are LLMs.
func hfTypeFor(task string, tags []string) string {
	switch strings.ToLower(task) {
	case "text-to-image", "image-to-image", "unconditional-image-generation":
		return "image"
	case "text-to-video", "image-to-video", "video-to-video":
		return "video"
	case "text-to-speech", "text-to-audio", "automatic-speech-recognition", "audio-to-audio", "audio-classification":
		return "audio"
	case "image-to-3d", "text-to-3d":
		return "3d"
	case "":
		for _, t := range tags {
			switch strings.ToLower(t) {
			case "text-to-image", "diffusers", "stable-diffusion", "flux":
				return "image"
			case "text-to-speech", "tts", "whisper":
				return "audio"
			case "text-to-video":
				return "video"
			}
		}
	}
	return "llm"
}
