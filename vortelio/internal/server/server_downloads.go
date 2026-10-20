package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/downloads"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
)

// Every model and engine download runs as a background job in the shared
// download manager: it survives closing the browser, continues after a
// restart, and shows up in one Downloads panel.

var downloadsOnce sync.Once

func initDownloads() {
	downloadsOnce.Do(func() {
		m := downloads.SetHome(config.HomeDir())
		m.RegisterKind("model", func(key string, meta map[string]string) (downloads.Task, error) {
			name := meta["model"]
			if name == "" {
				name = strings.TrimPrefix(key, "model:")
			}
			ref, err := hub.ParseModelRef(name)
			if err != nil {
				return nil, err
			}
			return modelTask(ref), nil
		})
		runtime.RegisterEngineResumer(getHardware)
	})
}

func modelJobKey(ref *hub.ModelRef) string { return "model:" + ref.String() }

func modelTask(ref *hub.ModelRef) downloads.Task {
	return func(ctx context.Context, p *downloads.Progress) error {
		p.Phase("Finding the files on Hugging Face…")
		started := false
		err := hub.NewDownloaderWithContext(ctx).Pull(ref, func(done, total int64) {
			if !started {
				started = true
				p.Phase("Downloading…")
			}
			if total > 0 {
				p.SetTotal(total)
			}
			p.SetDone(done)
		})
		if err != nil && ctx.Err() != nil {
			return downloads.ErrCancelled
		}
		return err
	}
}

// StartModelDownload queues (or joins) the download of a model reference.
func StartModelDownload(name string) (downloads.Job, *hub.ModelRef, error) {
	initDownloads()
	ref, err := hub.ParseModelRef(name)
	if err != nil {
		return downloads.Job{}, nil, err
	}
	j, _ := downloads.Default().Start("model", modelJobKey(ref), prettyRef(ref), map[string]string{"model": name, "type": ref.Type}, modelTask(ref))
	return j, ref, nil
}

func prettyRef(ref *hub.ModelRef) string {
	if ref.HFDirect != nil {
		t := ref.HFDirect.Owner + "/" + ref.HFDirect.Repo
		if ref.HFDirect.FileHint != "" {
			t += " (" + ref.HFDirect.FileHint + ")"
		}
		return t
	}
	return ref.Name + ":" + ref.Tag
}

// ── HTTP ────────────────────────────────────────────────────────────────────

// GET /api/downloads → every job, newest first.
func handleDownloads(w http.ResponseWriter, r *http.Request) {
	initDownloads()
	respond(w, 200, map[string]any{"jobs": downloads.Default().List()})
}

// GET /api/downloads/events → SSE stream: a "snapshot" event, then one "job"
// event per change.
func handleDownloadEvents(w http.ResponseWriter, r *http.Request) {
	initDownloads()
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch, stop := downloads.Default().Subscribe()
	defer stop()
	send := func(ev string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
		flusher.Flush()
	}
	send("snapshot", map[string]any{"jobs": downloads.Default().List()})
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	// Coalesce progress: at most ~4 updates per job per second.
	last := map[string]time.Time{}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case j, ok := <-ch:
			if !ok {
				return
			}
			if j.State == downloads.Running && time.Since(last[j.ID]) < 250*time.Millisecond {
				continue
			}
			last[j.ID] = time.Now()
			send("job", j)
		}
	}
}

// POST /api/downloads/start {model} | {engine}
func handleDownloadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "POST only")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON")
		return
	}
	initDownloads()
	var (
		j   downloads.Job
		err error
	)
	switch {
	case req.Engine != "":
		j, err = runtime.StartEngineInstall(req.Engine, getHardware())
	case req.Model != "":
		j, _, err = StartModelDownload(req.Model)
	default:
		err = fmt.Errorf("model or engine is required")
	}
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	respond(w, 202, j)
}

// POST /api/downloads/{cancel|retry|dismiss} {id}
func handleDownloadAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonError(w, 405, "POST only")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			jsonError(w, 400, "id is required")
			return
		}
		initDownloads()
		m := downloads.Default()
		var err error
		switch action {
		case "cancel":
			err = m.Cancel(req.ID)
		case "retry":
			err = m.Retry(req.ID)
		case "dismiss":
			err = m.Dismiss(req.ID)
		}
		if err != nil {
			jsonError(w, 400, err.Error())
			return
		}
		j, _ := m.Get(req.ID)
		respond(w, 200, j)
	}
}

// GET /api/engines → status of every native engine plus its install job.
func handleEngines(w http.ResponseWriter, r *http.Request) {
	initDownloads()
	type row struct {
		runtime.EngineStatus
		Job *downloads.Job `json:"job,omitempty"`
	}
	var out []row
	for _, st := range runtime.AllEngineStatus() {
		rw := row{EngineStatus: st}
		if j, ok := downloads.Default().Get(runtime.EngineJobKey(st.ID)); ok {
			rw.Job = &j
		}
		out = append(out, rw)
	}
	respond(w, 200, map[string]any{"engines": out, "llama": runtime.CheckLlama()})
}

// POST /api/engines/{install|rollback|remove} {id}
func handleEngineAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonError(w, 405, "POST only")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			jsonError(w, 400, "id is required")
			return
		}
		initDownloads()
		var err error
		switch action {
		case "install":
			var j downloads.Job
			if j, err = runtime.StartEngineInstall(req.ID, getHardware()); err == nil {
				respond(w, 202, j)
				return
			}
		case "rollback":
			err = runtime.RollbackEngine(req.ID)
		case "remove":
			err = runtime.RemoveEngine(req.ID)
		}
		if err != nil {
			jsonError(w, 400, err.Error())
			return
		}
		respond(w, 200, runtime.GetEngineStatus(req.ID))
	}
}

// GET /api/hf/token → {set}; POST {token} stores it ("" clears).
func handleHFToken(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respond(w, 200, map[string]bool{"set": hub.HFToken() != ""})
	case http.MethodPost:
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, 400, "invalid JSON")
			return
		}
		if err := hub.SetHFToken(req.Token); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"set": hub.HFToken() != ""})
	default:
		jsonError(w, 405, "GET or POST")
	}
}
