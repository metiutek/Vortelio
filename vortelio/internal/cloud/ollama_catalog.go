package cloud

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ollamaCatalogURL lists every model served by Ollama Cloud (public, no key).
const ollamaCatalogURL = "https://ollama.com/api/tags"

// ollamaPrices is USD per million tokens (input, output) from ollama.com/pricing
// (2026-09). Used to order the picker cheapest-first and to show the cost in the
// label; models missing here are listed after the priced ones.
var ollamaPrices = map[string][2]float64{
	"nemotron-3-nano":     {0.06, 0.24},
	"gpt-oss:20b":         {0.07, 0.30},
	"nemotron-3-super":    {0.015, 0.60},
	"gemma4":              {0.14, 0.40},
	"gpt-oss:120b":        {0.15, 0.60},
	"glm-5.3-flash":       {0.15, 0.50},
	"deepseek-v4.1-flash": {0.30, 1.20},
	"minimax-m2.7":        {0.30, 1.20},
	"mistral-large-3":     {0.50, 1.50},
	"minimax-m3":          {0.60, 2.40},
	"nemotron-3-ultra":    {0.10, 3.00},
	"kimi-k2.6":           {0.95, 4.00},
	"kimi-k2.7-code":      {0.95, 4.00},
	"deepseek-v4-pro":     {1.32, 3.96},
	"glm-5.3":             {1.40, 4.40},
	"glm-5.2":             {1.40, 4.40},
	"kimi-k3":             {3.00, 15.00},
}

// ollamaPrice finds the price for a model id, trying the full id and then the
// family (the part before ':'), since pricing lists e.g. "gemma4" for gemma4:31b.
func ollamaPrice(id string) ([2]float64, bool) {
	if p, ok := ollamaPrices[id]; ok {
		return p, true
	}
	if i := strings.IndexByte(id, ':'); i > 0 {
		p, ok := ollamaPrices[id[:i]]
		return p, ok
	}
	return [2]float64{}, false
}

var (
	ollamaMu      sync.Mutex
	ollamaCached  [][2]string
	ollamaFetched time.Time
)

// Choices returns the model picker entries for a provider. Ollama Cloud's
// catalog changes often, so it is fetched live (cached 6h) and falls back to
// the curated list offline.
func Choices(providerID string) [][2]string {
	if providerID == "ollamacloud" {
		if live := ollamaCloudChoices(); len(live) > 0 {
			return live
		}
	}
	if c, ok := ModelChoices[providerID]; ok {
		return c
	}
	// Custom endpoint: whatever it serves, else the model it was saved with.
	if p, ok := FindProvider(providerID); ok && p.Custom {
		if live, err := ListModels(providerID); err == nil && len(live) > 0 {
			return live
		}
		if p.DefaultModel != "" {
			return [][2]string{{p.DefaultModel, p.DefaultModel}}
		}
	}
	return nil
}

func ollamaCloudChoices() [][2]string {
	ollamaMu.Lock()
	defer ollamaMu.Unlock()
	if !ollamaFetched.IsZero() && time.Since(ollamaFetched) < 6*time.Hour {
		return ollamaCached
	}
	ollamaFetched = time.Now() // also throttles retries when offline
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ollamaCatalogURL)
	if err != nil {
		return ollamaCached
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Models) == 0 {
		return ollamaCached
	}
	ids := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if m.Name != "" {
			ids = append(ids, m.Name)
		}
	}
	ollamaCached = ollamaChoicesFor(ids)
	return ollamaCached
}

// ollamaChoicesFor orders ids cheapest-first (by output price) and labels them.
func ollamaChoicesFor(ids []string) [][2]string {
	curated := map[string]string{}
	for _, c := range ModelChoices["ollamacloud"] {
		curated[c[0]] = c[1]
	}
	sort.SliceStable(ids, func(i, j int) bool {
		pi, oki := ollamaPrice(ids[i])
		pj, okj := ollamaPrice(ids[j])
		if oki != okj {
			return oki
		}
		if pi[1] != pj[1] {
			return pi[1] < pj[1]
		}
		return ids[i] < ids[j]
	})
	out := make([][2]string, 0, len(ids))
	for _, id := range ids {
		label := curated[id]
		if label == "" {
			label = id
		}
		if p, ok := ollamaPrice(id); ok {
			label += " · $" + trimFloat(p[0]) + "/$" + trimFloat(p[1]) + " per 1M"
		}
		out = append(out, [2]string{id, label})
	}
	return out
}

// openRouterModelsURL lists every model OpenRouter serves (public, no key).
const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

var (
	orMu      sync.Mutex
	orFree    map[string]bool
	orFetched time.Time
)

// openRouterFreeIDs returns the ids of OpenRouter's currently free models
// (cached 6h), or nil when the catalog can't be fetched.
func openRouterFreeIDs() map[string]bool {
	orMu.Lock()
	defer orMu.Unlock()
	if !orFetched.IsZero() && time.Since(orFetched) < 6*time.Hour {
		return orFree
	}
	orFetched = time.Now() // also throttles retries when offline
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(openRouterModelsURL)
	if err != nil {
		return orFree
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Data) == 0 {
		return orFree
	}
	free := map[string]bool{}
	for _, m := range body.Data {
		if strings.HasSuffix(m.ID, ":free") || (m.Pricing.Prompt == "0" && m.Pricing.Completion == "0") {
			free[m.ID] = true
		}
	}
	orFree = free
	return orFree
}

func trimFloat(f float64) string {
	s := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 3, 64), "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}
