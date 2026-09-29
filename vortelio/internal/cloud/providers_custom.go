package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/config"
)

// ── Custom providers ──────────────────────────────────────────────────────────
//
// Besides the built-in catalog, the user can register any OpenAI-compatible
// (/v1/chat/completions: LM Studio, vLLM, LocalAI, llama.cpp server, Ollama,
// LiteLLM, Azure-style gateways, …) or Anthropic-compatible (/v1/messages)
// endpoint. They are stored in ~/.vortelio/cloud_providers.json and behave
// exactly like built-in providers everywhere (GUI, `vortelio code`, the /v1
// gateway, agents), since FindProvider and AllProviders include them.

// CustomProvider is the persisted form of a user-defined endpoint.
type CustomProvider struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	BaseURL      string    `json:"base_url"` // as entered, e.g. http://localhost:1234/v1
	Format       APIFormat `json:"format"`   // openai | anthropic
	DefaultModel string    `json:"default_model,omitempty"`
	NoKey        bool      `json:"no_key,omitempty"` // endpoint needs no API key
}

var customMu sync.Mutex

func customPath() string {
	return filepath.Join(config.HomeDir(), "cloud_providers.json")
}

// LoadCustomProviders returns the user-defined endpoints.
func LoadCustomProviders() []CustomProvider {
	customMu.Lock()
	defer customMu.Unlock()
	return loadCustomLocked()
}

func loadCustomLocked() []CustomProvider {
	data, err := os.ReadFile(customPath())
	if err != nil {
		return nil
	}
	var out []CustomProvider
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func saveCustomLocked(list []CustomProvider) error {
	if err := os.MkdirAll(filepath.Dir(customPath()), 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	return os.WriteFile(customPath(), data, 0600)
}

var (
	slugRe    = regexp.MustCompile(`[^a-z0-9]+`)
	versionRe = regexp.MustCompile(`/v\d+[a-z0-9]*$`)
)

// SaveCustomProvider adds (or, when cp.ID matches an existing entry, updates) a
// custom endpoint and returns it as a usable Provider.
func SaveCustomProvider(cp CustomProvider) (Provider, error) {
	cp.Name = strings.TrimSpace(cp.Name)
	cp.BaseURL = strings.TrimSpace(cp.BaseURL)
	cp.DefaultModel = strings.TrimSpace(cp.DefaultModel)
	if cp.BaseURL == "" {
		return Provider{}, fmt.Errorf("base URL required")
	}
	if !strings.HasPrefix(cp.BaseURL, "http://") && !strings.HasPrefix(cp.BaseURL, "https://") {
		cp.BaseURL = "http://" + cp.BaseURL
	}
	if cp.Format != FormatAnthropic {
		cp.Format = FormatOpenAI
	}
	if cp.Name == "" {
		cp.Name = hostOf(cp.BaseURL)
	}

	customMu.Lock()
	defer customMu.Unlock()
	list := loadCustomLocked()
	if cp.ID != "" {
		for i := range list {
			if list[i].ID == cp.ID {
				list[i] = cp
				return cp.provider(), saveCustomLocked(list)
			}
		}
	}
	base := "custom-" + strings.Trim(slugRe.ReplaceAllString(strings.ToLower(cp.Name), "-"), "-")
	if base == "custom-" {
		base = "custom-endpoint"
	}
	id := base
	for n := 2; idTaken(id, list); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	cp.ID = id
	list = append(list, cp)
	return cp.provider(), saveCustomLocked(list)
}

func idTaken(id string, list []CustomProvider) bool {
	for _, p := range Providers {
		if p.ID == id {
			return true
		}
	}
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// DeleteCustomProvider removes a custom endpoint and its stored keys.
func DeleteCustomProvider(id string) error {
	customMu.Lock()
	list := loadCustomLocked()
	out := list[:0]
	found := false
	for _, c := range list {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	var err error
	if found {
		err = saveCustomLocked(out)
	}
	customMu.Unlock()
	if !found {
		return fmt.Errorf("unknown custom provider %q", id)
	}
	_ = DeleteKey(id)
	return err
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// endpointURL turns what the user typed into the chat endpoint for the format:
// "host:port", ".../v1" or the full ".../chat/completions" / ".../messages".
func endpointURL(base string, f APIFormat) string {
	u := strings.TrimRight(strings.TrimSpace(base), "/")
	suffix := "/chat/completions"
	if f == FormatAnthropic {
		suffix = "/messages"
	}
	if strings.HasSuffix(u, suffix) {
		return u
	}
	if strings.HasSuffix(u, "/v1") || versionRe.MatchString(u) || strings.HasSuffix(u, "/openai") {
		return u + suffix
	}
	return u + "/v1" + suffix
}

func (cp CustomProvider) provider() Provider {
	p := Provider{
		ID:           cp.ID,
		Name:         cp.Name,
		DefaultModel: cp.DefaultModel,
		BaseURL:      endpointURL(cp.BaseURL, cp.Format),
		Format:       cp.Format,
		KeyHint:      cp.BaseURL,
		Custom:       true,
		NoKey:        cp.NoKey,
	}
	if cp.Format == FormatOpenAI {
		p.AuthHeader, p.AuthPrefix = "Authorization", "Bearer "
	}
	return p
}

// AllProviders is the built-in catalog followed by the user's custom endpoints.
func AllProviders() []Provider {
	out := append([]Provider(nil), Providers...)
	for _, c := range LoadCustomProviders() {
		out = append(out, c.provider())
	}
	return out
}

// Configured reports whether a provider can be used right now: it has a saved
// key, or it is a custom endpoint that needs none.
func Configured(id string) bool {
	if HasKey(id) {
		return true
	}
	p, ok := FindProvider(id)
	return ok && p.NoKey
}

// KeysFor returns the keys to try for a provider. A keyless custom endpoint gets
// a single empty key so the failover helpers still make one request.
func KeysFor(id string) []string {
	if keys := LoadKeys(id); len(keys) > 0 {
		return keys
	}
	if p, ok := FindProvider(id); ok && p.NoKey {
		return []string{""}
	}
	return nil
}

// ForModel returns a copy of p targeting model (Gemini encodes the model in the
// URL, the others in the request body).
func ForModel(p Provider, model string) Provider {
	if model == "" {
		return p
	}
	p.DefaultModel = model
	if p.Format == FormatGemini && !p.Custom {
		p.BaseURL = "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	}
	return p
}

// ── Live model listing ────────────────────────────────────────────────────────

type liveEntry struct {
	models [][2]string
	at     time.Time
}

var (
	liveMu    sync.Mutex
	liveCache = map[string]liveEntry{}
)

// ListModels asks the provider which models it serves (GET …/models), using the
// first stored key. Results are cached for 30 minutes. Only chat-capable models
// are returned for providers whose catalog also lists embeddings, TTS, etc.
func ListModels(id string) ([][2]string, error) {
	p, ok := FindProvider(id)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", id)
	}
	keys := KeysFor(id)
	key := ""
	if len(keys) > 0 {
		key = keys[0]
	} else if !p.NoKey {
		return nil, fmt.Errorf("no API key for %s", p.Name)
	}
	cacheKey := id + "\x00" + key
	liveMu.Lock()
	if e, ok := liveCache[cacheKey]; ok && time.Since(e.at) < 30*time.Minute {
		liveMu.Unlock()
		return e.models, nil
	}
	liveMu.Unlock()

	models, err := fetchModels(p, key)
	if err != nil {
		return nil, err
	}
	liveMu.Lock()
	liveCache[cacheKey] = liveEntry{models, time.Now()}
	liveMu.Unlock()
	return models, nil
}

// modelsURL derives the model-list endpoint from the chat endpoint.
func modelsURL(p Provider) string {
	switch {
	case p.Format == FormatGemini:
		return "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1000"
	case p.ID == "anthropic":
		return "https://api.anthropic.com/v1/models?limit=1000"
	}
	u := p.BaseURL
	for _, suf := range []string{"/chat/completions", "/messages"} {
		u = strings.TrimSuffix(u, suf)
	}
	return u + "/models"
}

func fetchModels(p Provider, key string) ([][2]string, error) {
	url := modelsURL(p)
	if p.Format == FormatGemini && key != "" {
		url += "&key=" + key
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		switch {
		case p.Format == FormatAnthropic:
			req.Header.Set("x-api-key", key)
			if p.Custom { // Anthropic-compatible gateways often want a bearer token
				req.Header.Set("Authorization", "Bearer "+key)
			}
		case p.Format == FormatOpenAI:
			req.Header.Set(p.AuthHeader, p.AuthPrefix+key)
		}
	}
	if p.Format == FormatAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: model list returned HTTP %d", p.Name, resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
		Models []struct { // Gemini, Ollama /api/tags style
			Name        string   `json:"name"`
			Model       string   `json:"model"`
			DisplayName string   `json:"displayName"`
			Methods     []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: unreadable model list: %w", p.Name, err)
	}
	var out [][2]string
	for _, m := range body.Data {
		if m.ID == "" || !isChatModel(p, m.ID) {
			continue
		}
		label := m.ID
		if m.DisplayName != "" {
			label = m.DisplayName
		} else if m.Name != "" && m.Name != m.ID {
			label = m.Name
		}
		out = append(out, [2]string{m.ID, label})
	}
	for _, m := range body.Models {
		id := strings.TrimPrefix(firstNonEmpty(m.Model, m.Name), "models/")
		if id == "" {
			continue
		}
		if p.Format == FormatGemini && !containsString(m.Methods, "generateContent") {
			continue
		}
		label := id
		if m.DisplayName != "" {
			label = m.DisplayName
		}
		out = append(out, [2]string{id, label})
	}
	if p.Custom || p.ID == "openai" {
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	}
	return out, nil
}

// isChatModel drops the non-chat entries OpenAI-style catalogs mix in.
func isChatModel(p Provider, id string) bool {
	l := strings.ToLower(id)
	for _, bad := range []string{"embed", "whisper", "tts", "dall-e", "gpt-image", "moderation",
		"transcribe", "realtime", "audio", "davinci", "babbage", "sora", "rerank"} {
		if strings.Contains(l, bad) {
			return false
		}
	}
	return true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
