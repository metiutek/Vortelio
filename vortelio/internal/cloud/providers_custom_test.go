package cloud

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		in   string
		f    APIFormat
		want string
	}{
		{"http://localhost:1234/v1", FormatOpenAI, "http://localhost:1234/v1/chat/completions"},
		{"http://localhost:1234/", FormatOpenAI, "http://localhost:1234/v1/chat/completions"},
		{"https://x.ai/api/v1/chat/completions", FormatOpenAI, "https://x.ai/api/v1/chat/completions"},
		{"https://gw.example/v4", FormatOpenAI, "https://gw.example/v4/chat/completions"},
		{"https://gw.example/anthropic", FormatAnthropic, "https://gw.example/anthropic/v1/messages"},
		{"https://gw.example/v1", FormatAnthropic, "https://gw.example/v1/messages"},
	}
	for _, c := range cases {
		if got := endpointURL(c.in, c.f); got != c.want {
			t.Errorf("endpointURL(%q, %s) = %q, want %q", c.in, c.f, got, c.want)
		}
	}
}

// A keyless OpenAI-compatible server (LM Studio style) is usable end to end:
// saved, found, listed as configured, its models fetched, and chat streams
// without any Authorization header.
func TestCustomOpenAIProviderKeyless(t *testing.T) {
	t.Setenv("VORTELIO_HOME", t.TempDir())
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"qwen3-8b"},{"id":"text-embedding-nomic"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi \"}}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p, err := SaveCustomProvider(CustomProvider{Name: "LM Studio", BaseURL: srv.URL + "/v1", NoKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "custom-lm-studio" {
		t.Fatalf("id = %q", p.ID)
	}
	if _, ok := FindProvider(p.ID); !ok || !Configured(p.ID) {
		t.Fatal("custom provider not found/configured")
	}
	found := false
	for _, a := range AllProviders() {
		found = found || a.ID == p.ID
	}
	if !found {
		t.Fatal("AllProviders misses the custom provider")
	}
	models, err := ListModels(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0][0] != "qwen3-8b" {
		t.Fatalf("models = %v (embeddings must be filtered)", models)
	}
	out, err := ChatFailover(ForModel(p, "qwen3-8b"), KeysFor(p.ID), []Message{{Role: "user", Content: "hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi there" {
		t.Fatalf("chat = %q", out)
	}
	if sawAuth {
		t.Fatal("keyless endpoint received an Authorization header")
	}
	if err := DeleteCustomProvider(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindProvider(p.ID); ok {
		t.Fatal("provider still present after delete")
	}
}

// An Anthropic-compatible gateway gets x-api-key + anthropic-version on
// /v1/messages and its SSE stream is parsed.
func TestCustomAnthropicProvider(t *testing.T) {
	t.Setenv("VORTELIO_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"glm-5","display_name":"GLM 5"}]}`)
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"model":"glm-5"`) {
				http.Error(w, "bad model", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
			io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p, err := SaveCustomProvider(CustomProvider{Name: "GLM", BaseURL: srv.URL, Format: FormatAnthropic})
	if err != nil {
		t.Fatal(err)
	}
	if Configured(p.ID) {
		t.Fatal("provider without key reported configured")
	}
	if err := SaveKey(p.ID, "sk-test"); err != nil {
		t.Fatal(err)
	}
	models, err := ListModels(p.ID)
	if err != nil || len(models) != 1 || models[0][1] != "GLM 5" {
		t.Fatalf("models = %v, err = %v", models, err)
	}
	out, err := ChatFailover(ForModel(p, "glm-5"), KeysFor(p.ID), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || out != "ok" {
		t.Fatalf("chat = %q, err = %v", out, err)
	}
}
