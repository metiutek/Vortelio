package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vortelio/vortelio/internal/cloud"
)

// proxyCloudChatCompletion turns /v1/chat/completions into a unified gateway:
// when the requested model isn't a local one, it's treated as "provider/model"
// (e.g. "anthropic/claude-3-5-haiku-20241022") and forwarded — verbatim, tools
// and streaming included — to that cloud provider's OpenAI-compatible endpoint
// using the API key stored in Vortelio. Returns true if it handled the request.
func proxyCloudChatCompletion(w http.ResponseWriter, modelID string, rawBody []byte) bool {
	slash := strings.Index(modelID, "/")
	if slash <= 0 || slash >= len(modelID)-1 {
		return false // not a "provider/model" id
	}
	provID := modelID[:slash]
	realModel := modelID[slash+1:]

	p, ok := cloud.FindProvider(provID)
	if !ok {
		return false // unknown provider — let the caller 404
	}
	keys := cloud.LoadKeys(provID)
	if len(keys) == 0 {
		jsonError(w, 402, "no API key configured for cloud provider "+provID+" — add one in Vortelio (Cloud Models)")
		return true
	}

	// Rewrite only the model field; keep messages, tools, stream, etc. intact.
	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return true
	}
	body["model"] = realModel
	outBody, _ := json.Marshal(body)

	preq, err := http.NewRequest(http.MethodPost, cloud.ChatCompletionsURL(p), bytes.NewReader(outBody))
	if err != nil {
		jsonError(w, 500, err.Error())
		return true
	}
	preq.Header.Set("Content-Type", "application/json")
	preq.Header.Set("Authorization", "Bearer "+keys[0])
	// Anthropic's OpenAI-compatible endpoint still wants an API version header.
	if p.ID == "anthropic" {
		preq.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := http.DefaultClient.Do(preq)
	if err != nil {
		jsonError(w, 502, "cloud request failed: "+err.Error())
		return true
	}
	defer resp.Body.Close()

	// Relay status, content-type and the (possibly streamed) body verbatim.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	return true
}
