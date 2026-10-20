package runtime

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vortelio/vortelio/internal/hub"
)

// evictingTool behaves like generate_image: it takes the GPU, which stops the
// chat model in the middle of the tool loop.
type evictingTool struct{ calls int }

func (t *evictingTool) Tools() []ToolDef {
	return []ToolDef{{Type: "function", Function: ToolFuncDef{
		Name: "make_picture", Description: "Draws a picture of the given subject and returns its file name.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"subject":{"type":"string"}},"required":["subject"]}`),
	}}}
}

func (t *evictingTool) Execute(name, args string) (string, error) {
	t.calls++
	AcquireGPU("image")
	return "picture saved as cat.png", nil
}

// VORTELIO_LLM_IT=llm/qwen3.5:4b go test -run TestLiveToolRoundSurvivesEviction ./internal/runtime -v
func TestLiveToolRoundSurvivesEviction(t *testing.T) {
	refStr := os.Getenv("VORTELIO_LLM_IT")
	if refStr == "" {
		t.Skip("set VORTELIO_LLM_IT")
	}
	ref, err := hub.ParseModelRef(refStr)
	if err != nil {
		t.Fatal(err)
	}
	m, err := hub.NewModelStore().Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	r, err := GlobalModelManager.GetOrLoad(m, DetectHardware(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tool := &evictingTool{}
	var out strings.Builder
	err = r.StreamWithOpts(StreamOpts{
		Prompt:       "Use the make_picture tool to draw a cat, then tell me the file name.",
		ToolsEnabled: true, ToolProvider: tool,
	}, func(s string) { out.WriteString(s) }, nil)
	if err != nil {
		t.Fatalf("stream after eviction: %v", err)
	}
	if tool.calls == 0 {
		t.Fatal("model did not call the tool")
	}
	t.Logf("answer: %q", out.String())
	if !strings.Contains(out.String(), "cat.png") {
		t.Fatalf("model did not continue after the GPU was taken: %q", out.String())
	}
	if n := len(GlobalModelManager.ListLoaded()); n != 1 {
		t.Fatalf("revived runner not managed: %d loaded", n)
	}
	GlobalModelManager.UnloadAll()
}
