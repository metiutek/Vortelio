package runtime

import (
	"errors"
	"fmt"
	"log"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
)

// DefaultKeepAliveDuration is how long a loaded model stays alive after last use.
const DefaultKeepAliveDuration = 5 * time.Minute

// ModelManager keeps llama-server processes alive between HTTP requests.
//
// Residency policy (as in Ollama/Unsloth): by default only ONE model stays
// loaded, so two models never fight for the same VRAM — loading another one
// (or a different context size, or an image/audio generation that needs the
// GPU) first stops the resident one. VORTELIO_MAX_LOADED_MODELS raises the
// limit on machines with memory to spare.
type ModelManager struct {
	mu      sync.Mutex
	entries map[string]*managedEntry
	loading map[string]*loadCall
}

type managedEntry struct {
	runner    *LLMRunner
	ctxSize   int
	keepAlive time.Duration
	expiresAt time.Time // zero = never expire
	lastUsed  time.Time
}

// loadCall lets concurrent requests for the same model share one load.
type loadCall struct {
	done    chan struct{}
	runner  *LLMRunner
	err     error
	loading *LLMRunner // the runner being started, so a newer request can abort it
}

// LoadedModel is returned by ListLoaded.
type LoadedModel struct {
	Model       *hub.Model
	ExpiresAt   time.Time
	SizeVRAM    int64
	GPULayers   int
	TotalLayers int
	ContextSize int
	Embedding   bool
}

// GlobalModelManager is the singleton used by the HTTP server.
var GlobalModelManager = newModelManager()

func newModelManager() *ModelManager {
	m := &ModelManager{entries: map[string]*managedEntry{}, loading: map[string]*loadCall{}}
	RegisterGPUEvictor("chat", m.UnloadAll)
	go m.evictLoop()
	return m
}

func modelKey(model *hub.Model) string {
	return fmt.Sprintf("%s/%s:%s", model.Type, model.Name, model.Tag)
}

// maxLoaded is how many llama-servers may run at once.
func maxLoaded() int {
	if v, err := strconv.Atoi(os.Getenv("VORTELIO_MAX_LOADED_MODELS")); err == nil && v > 0 {
		return v
	}
	return 1
}

// GetOrLoad returns a running LLMRunner, starting llama-server if needed.
// keepAlive = 0 → DefaultKeepAliveDuration; keepAlive < 0 → never expire.
func (m *ModelManager) GetOrLoad(model *hub.Model, hw *Hardware, keepAlive time.Duration) (*LLMRunner, error) {
	return m.getOrLoad(model, hw, keepAlive, 0, false)
}

// GetOrLoadWithContext is like GetOrLoad but pins the llama-server to a
// context size (<= 0 = server default). A different size restarts the model.
func (m *ModelManager) GetOrLoadWithContext(model *hub.Model, hw *Hardware, keepAlive time.Duration, ctxSize int) (*LLMRunner, error) {
	return m.getOrLoad(model, hw, keepAlive, ctxSize, false)
}

// GetOrLoadEmbed returns a llama-server started with --embeddings for model.
// A chat server cannot embed (llama.cpp answers null vectors), so embeddings
// always get their own server.
func (m *ModelManager) GetOrLoadEmbed(model *hub.Model, hw *Hardware, keepAlive time.Duration) (*LLMRunner, error) {
	return m.getOrLoad(model, hw, keepAlive, 0, true)
}

func (m *ModelManager) getOrLoad(model *hub.Model, hw *Hardware, keepAlive time.Duration, ctxSize int, embed bool) (*LLMRunner, error) {
	if keepAlive == 0 {
		keepAlive = DefaultKeepAliveDuration
	}
	key := modelKey(model)
	if embed {
		key += "#embed"
	}

	for {
		m.mu.Lock()
		if e, ok := m.entries[key]; ok {
			sameCtx := ctxSize <= 0 || e.ctxSize == ctxSize
			if e.runner.Alive() && sameCtx {
				m.touchLocked(e, keepAlive)
				r := e.runner
				m.mu.Unlock()
				return r, nil
			}
			// Crashed, or asked for another context size: reload.
			log.Printf("[models] reloading %s: alive=%v ctx %d -> %d", key, e.runner.Alive(), e.ctxSize, ctxSize)
			e.runner.stopServer()
			delete(m.entries, key)
		}
		if call, ok := m.loading[key]; ok {
			m.mu.Unlock()
			<-call.done
			if call.err != nil {
				return nil, call.err
			}
			continue // pick up the entry the other request registered
		}
		// With room for one model, a newer request wins: stop loading the
		// model it replaces instead of loading both at once.
		if maxLoaded() == 1 {
			for k, c := range m.loading {
				if strings.TrimSuffix(k, "#embed") != modelKey(model) && c.loading != nil && !c.loading.aborted() {
					close(c.loading.abort)
				}
			}
		}
		call := &loadCall{done: make(chan struct{})}
		m.loading[key] = call
		m.mu.Unlock()

		log.Printf("[models] loading %s (ctx %d) for %s", key, ctxSize, callerName())
		r, err := m.load(model, hw, ctxSize, embed, call)

		m.mu.Lock()
		delete(m.loading, key)
		if err == nil {
			e := &managedEntry{runner: r, ctxSize: ctxSize}
			m.touchLocked(e, keepAlive)
			m.entries[key] = e
		}
		call.runner, call.err = r, err
		close(call.done)
		m.mu.Unlock()
		return r, err
	}
}

func (m *ModelManager) touchLocked(e *managedEntry, keepAlive time.Duration) {
	e.keepAlive = keepAlive
	e.lastUsed = time.Now()
	if keepAlive > 0 {
		e.expiresAt = time.Now().Add(keepAlive)
	} else {
		e.expiresAt = time.Time{}
	}
}

// load frees room for one more model and starts it.
func (m *ModelManager) load(model *hub.Model, hw *Hardware, ctxSize int, embed bool, call *loadCall) (*LLMRunner, error) {
	// Image/video/audio engines release the GPU first, then older models.
	AcquireGPU("chat")
	m.makeRoom(maxLoaded() - 1)

	runner := NewLLMRunnerForServer(model, hw)
	runner.embedding = embed
	runner.abort = make(chan struct{})
	m.mu.Lock()
	call.loading = runner
	m.mu.Unlock()
	if ctxSize > 0 {
		runner.SetContextSize(ctxSize)
	}
	if err := runner.EnsureServer(); err != nil {
		// A model newer than the installed llama.cpp: update the engine once and
		// retry, so new architectures just work.
		if errors.Is(err, ErrLoadAborted) || !IsArchUnsupported(err) || !m.updateLlamaForNewModel(hw) {
			return nil, err
		}
		if err := runner.EnsureServer(); err != nil {
			return nil, err
		}
	}
	return runner, nil
}

// makeRoom stops the least recently used models until at most keep remain.
// A model answering a request is stopped only once the answer is complete
// (at most a few minutes), so switching models never cuts a reply in half.
func (m *ModelManager) makeRoom(keep int) {
	var victims []*LLMRunner
	m.mu.Lock()
	for len(m.entries) > keep && len(m.entries) > 0 {
		var oldKey string
		var old *managedEntry
		for k, e := range m.entries {
			if old == nil || e.lastUsed.Before(old.lastUsed) {
				oldKey, old = k, e
			}
		}
		victims = append(victims, old.runner)
		delete(m.entries, oldKey)
	}
	m.mu.Unlock()
	for _, r := range victims {
		for deadline := time.Now().Add(5 * time.Minute); r.Busy() && time.Now().Before(deadline); {
			time.Sleep(200 * time.Millisecond)
		}
		r.stopServer()
	}
}

// revive restarts the server of a runner that was stopped while a request was
// still using it (e.g. a media tool took the GPU between tool rounds), and
// registers it again so it is managed and eventually unloaded.
func (m *ModelManager) revive(r *LLMRunner) error {
	key := modelKey(r.model)
	if r.embedding {
		key += "#embed"
	}
	AcquireGPU("chat")
	m.mu.Lock()
	if e, ok := m.entries[key]; ok && e.runner != r {
		e.runner.stopServer()
		delete(m.entries, key)
	}
	m.mu.Unlock()
	m.makeRoom(maxLoaded() - 1)
	if err := r.EnsureServer(); err != nil {
		return err
	}
	m.mu.Lock()
	e := &managedEntry{runner: r, ctxSize: r.currentCtxSize}
	m.touchLocked(e, DefaultKeepAliveDuration)
	m.entries[key] = e
	m.mu.Unlock()
	return nil
}

// Unload stops a model (chat and embedding servers) and frees its memory.
func (m *ModelManager) Unload(model *hub.Model) {
	base := modelKey(model)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range []string{base, base + "#embed"} {
		if e, ok := m.entries[key]; ok {
			e.runner.stopServer()
			delete(m.entries, key)
		}
	}
}

// UnloadAll stops every running llama-server (e.g. before replacing llama.cpp,
// or when an image/video/audio engine needs the GPU).
func (m *ModelManager) UnloadAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.entries {
		e.runner.stopServer()
		delete(m.entries, key)
	}
}

// updateLlamaForNewModel installs a newer llama.cpp when one exists. Reports
// whether an update was installed (so loading is worth retrying).
func (m *ModelManager) updateLlamaForNewModel(hw *Hardware) bool {
	invalidateLlamaLatest() // a new build may have landed since the last check
	st := CheckLlama()
	if !st.UpdateAvailable || (st.Installed && !st.Managed) {
		return false
	}
	m.UnloadAll() // Windows keeps DLLs locked while a server runs
	_, err := InstallLlamaCpp(hw, nil)
	return err == nil
}

// ListLoaded returns all currently loaded models with expiry and placement.
func (m *ModelManager) ListLoaded() []LoadedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []LoadedModel
	for _, e := range m.entries {
		r := e.runner
		lm := LoadedModel{
			Model:       r.model,
			ExpiresAt:   e.expiresAt,
			GPULayers:   r.gpuLayers,
			TotalLayers: r.totalLayers,
			ContextSize: r.ctxSize,
			Embedding:   r.embedding,
		}
		if lm.ContextSize == 0 {
			lm.ContextSize = e.ctxSize
		}
		if r.totalLayers > 0 && r.model != nil {
			lm.SizeVRAM = r.model.SizeBytes * int64(r.gpuLayers) / int64(r.totalLayers)
		}
		result = append(result, lm)
	}
	return result
}

func (m *ModelManager) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for key, e := range m.entries {
			expired := !e.expiresAt.IsZero() && now.After(e.expiresAt)
			if expired || !e.runner.Alive() {
				e.runner.stopServer()
				delete(m.entries, key)
			}
		}
		m.mu.Unlock()
	}
}

// callerName names the code outside this file that asked for a model (for the
// log: which endpoint loaded what).
func callerName() string {
	pcs := make([]uintptr, 8)
	n := goruntime.Callers(3, pcs)
	frames := goruntime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasSuffix(f.File, "model_manager.go") {
			return f.Function[strings.LastIndex(f.Function, "/")+1:]
		}
		if !more {
			return "?"
		}
	}
}
