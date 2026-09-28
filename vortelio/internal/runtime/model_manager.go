package runtime

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
)

// DefaultKeepAliveDuration is how long a loaded model stays alive after last use.
const DefaultKeepAliveDuration = 5 * time.Minute

// ModelManager keeps llama-server processes alive between HTTP requests.
type ModelManager struct {
	mu      sync.Mutex
	entries map[string]*managedEntry
}

type managedEntry struct {
	runner    *LLMRunner
	expiresAt time.Time // zero = never expire
}

// LoadedModel is returned by ListLoaded.
type LoadedModel struct {
	Model     *hub.Model
	ExpiresAt time.Time
	SizeVRAM  int64
}

// GlobalModelManager is the singleton used by the HTTP server.
var GlobalModelManager = newModelManager()

func newModelManager() *ModelManager {
	m := &ModelManager{entries: make(map[string]*managedEntry)}
	go m.evictLoop()
	return m
}

func modelKey(model *hub.Model) string {
	return fmt.Sprintf("%s/%s:%s", model.Type, model.Name, model.Tag)
}

// modelKeyCtx keys an entry by model AND context size, so requests asking for a
// different context size get their own warm process instead of reusing one
// started with the wrong --ctx-size.
func modelKeyCtx(model *hub.Model, ctxSize int) string {
	if ctxSize <= 0 {
		return modelKey(model)
	}
	return fmt.Sprintf("%s#ctx%d", modelKey(model), ctxSize)
}

// GetOrLoad returns a running LLMRunner, starting llama-server if needed.
// keepAlive = 0 → DefaultKeepAliveDuration; keepAlive < 0 → never expire.
func (m *ModelManager) GetOrLoad(model *hub.Model, hw *Hardware, keepAlive time.Duration) (*LLMRunner, error) {
	return m.GetOrLoadWithContext(model, hw, keepAlive, 0)
}

// GetOrLoadWithContext is like GetOrLoad but pins the llama-server to a specific
// context size. ctxSize <= 0 means "server default". Requests with a context
// size still reuse a warm process (keyed by model+ctxSize) instead of spawning
// and killing llama-server on every message.
func (m *ModelManager) GetOrLoadWithContext(model *hub.Model, hw *Hardware, keepAlive time.Duration, ctxSize int) (*LLMRunner, error) {
	if keepAlive == 0 {
		keepAlive = DefaultKeepAliveDuration
	}
	key := modelKeyCtx(model, ctxSize)

	m.mu.Lock()
	if e, ok := m.entries[key]; ok {
		if keepAlive > 0 {
			e.expiresAt = time.Now().Add(keepAlive)
		} else {
			e.expiresAt = time.Time{}
		}
		runner := e.runner
		m.mu.Unlock()
		return runner, nil
	}
	m.mu.Unlock()

	runner := NewLLMRunnerForServer(model, hw)
	if ctxSize > 0 {
		runner.SetContextSize(ctxSize)
	}
	if err := runner.EnsureServer(); err != nil {
		// A model newer than the installed llama.cpp: update the engine once and
		// retry, so new architectures just work.
		if !IsArchUnsupported(err) || !m.updateLlamaForNewModel(hw) {
			return nil, err
		}
		if err := runner.EnsureServer(); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		// Another goroutine loaded the same model concurrently
		runner.stopServer()
		if keepAlive > 0 {
			e.expiresAt = time.Now().Add(keepAlive)
		}
		return e.runner, nil
	}
	expiry := time.Time{}
	if keepAlive > 0 {
		expiry = time.Now().Add(keepAlive)
	}
	m.entries[key] = &managedEntry{runner: runner, expiresAt: expiry}
	return runner, nil
}

// Unload stops a model and removes it from the manager. It removes every
// context-size variant of the model (base key and any "…#ctxN" entries) so a
// manual unload actually frees the VRAM.
func (m *ModelManager) Unload(model *hub.Model) {
	base := modelKey(model)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.entries {
		if key == base || strings.HasPrefix(key, base+"#ctx") {
			e.runner.stopServer()
			delete(m.entries, key)
		}
	}
}

// UnloadAll stops every running llama-server (e.g. before replacing llama.cpp).
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

// ListLoaded returns all currently loaded models with expiry info.
func (m *ModelManager) ListLoaded() []LoadedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []LoadedModel
	for _, e := range m.entries {
		result = append(result, LoadedModel{
			Model:     e.runner.model,
			ExpiresAt: e.expiresAt,
		})
	}
	return result
}

func (m *ModelManager) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for key, e := range m.entries {
			if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
				e.runner.stopServer()
				delete(m.entries, key)
			}
		}
		m.mu.Unlock()
	}
}
