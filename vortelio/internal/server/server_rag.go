package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// ─────────────────────────────────────────────────────────────────────────────
// /api/rag/ingest + /api/rag/query — minimal RAG with cosine similarity.
// Storage: ~/.vortelio/rag/<collection>.json — array of {id, text, vec, meta}
// ─────────────────────────────────────────────────────────────────────────────

type ragChunk struct {
	ID    string                 `json:"id"`
	Text  string                 `json:"text"`
	Vec   []float64              `json:"vec"`
	Meta  map[string]interface{} `json:"meta,omitempty"`
	Coll  string                 `json:"collection,omitempty"`
	Added string                 `json:"added_at"`
}

var (
	ragMu sync.Mutex
)

func ragDir() string {
	return filepath.Join(config.HomeDir(), "rag")
}

func ragCollectionPath(coll string) string {
	if coll == "" {
		coll = "default"
	}
	safe := strings.ReplaceAll(coll, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	return filepath.Join(ragDir(), safe+".json")
}

func loadCollection(coll string) []ragChunk {
	data, err := os.ReadFile(ragCollectionPath(coll))
	if err != nil {
		return nil
	}
	var out []ragChunk
	json.Unmarshal(data, &out)
	return out
}

func saveCollection(coll string, chunks []ragChunk) error {
	os.MkdirAll(ragDir(), 0755)
	data, err := json.MarshalIndent(chunks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ragCollectionPath(coll), data, 0644)
}

func handleRAGIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model      string                   `json:"model"`
		Collection string                   `json:"collection"`
		Documents  []map[string]interface{} `json:"documents"` // {text, meta}
		ChunkSize  int                      `json:"chunk_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || len(req.Documents) == 0 {
		jsonError(w, 400, "model and documents required")
		return
	}
	if req.ChunkSize == 0 {
		req.ChunkSize = 800
	}

	runner, err := resolveRunner(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}

	ragMu.Lock()
	defer ragMu.Unlock()

	existing := loadCollection(req.Collection)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	added := 0

	for di, doc := range req.Documents {
		text, _ := doc["text"].(string)
		if text == "" {
			continue
		}
		meta, _ := doc["meta"].(map[string]interface{})
		chunks := chunkText(text, req.ChunkSize)

		vecs, err := runner.EmbedBatch(chunks)
		if err != nil {
			jsonError(w, 500, "embed failed: "+err.Error())
			return
		}
		for ci, c := range chunks {
			existing = append(existing, ragChunk{
				ID:    fmt.Sprintf("d%d-c%d-%d", di, ci, time.Now().UnixNano()),
				Text:  c,
				Vec:   vecs[ci],
				Meta:  meta,
				Coll:  req.Collection,
				Added: now,
			})
			added++
		}
	}

	if err := saveCollection(req.Collection, existing); err != nil {
		jsonError(w, 500, "save failed: "+err.Error())
		return
	}

	respond(w, 200, map[string]interface{}{
		"collection": req.Collection,
		"added":      added,
		"total":      len(existing),
	})
}

func handleRAGQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model      string `json:"model"`
		Collection string `json:"collection"`
		Query      string `json:"query"`
		TopK       int    `json:"top_k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Query == "" {
		jsonError(w, 400, "model and query required")
		return
	}
	if req.TopK == 0 {
		req.TopK = 5
	}

	runner, err := resolveRunner(req.Model)
	if err != nil {
		jsonError(w, 404, err.Error())
		return
	}
	qvec, err := runner.Embed(req.Query)
	if err != nil {
		jsonError(w, 500, "embed failed: "+err.Error())
		return
	}

	chunks := loadCollection(req.Collection)
	type scored struct {
		Score float64                `json:"score"`
		Text  string                 `json:"text"`
		ID    string                 `json:"id"`
		Meta  map[string]interface{} `json:"meta,omitempty"`
	}
	results := make([]scored, 0, len(chunks))
	for _, c := range chunks {
		results = append(results, scored{
			Score: cosineSim(qvec, c.Vec),
			Text:  c.Text,
			ID:    c.ID,
			Meta:  c.Meta,
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > req.TopK {
		results = results[:req.TopK]
	}

	respond(w, 200, map[string]interface{}{
		"collection": req.Collection,
		"query":      req.Query,
		"results":    results,
	})
}

func resolveRunner(modelName string) (*runtime.LLMRunner, error) {
	ref, err := hub.ParseModelRef(modelName)
	if err != nil {
		return nil, err
	}
	m, err := hub.NewModelStore().Resolve(ref)
	if err != nil {
		return nil, err
	}
	return runtime.GlobalModelManager.GetOrLoadEmbed(m, getHardware(), 5*time.Minute)
}

func cosineSim(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
