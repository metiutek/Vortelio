package server

import (
	"time"

	"github.com/vortelio/vortelio/internal/runtime"
)

// addGenStats fills the Ollama timing fields of a final ("done") response
// from what llama-server reported.
func addGenStats(obj map[string]interface{}, s *runtime.GenStats, load time.Duration, start time.Time) {
	obj["total_duration"] = time.Since(start).Nanoseconds()
	obj["load_duration"] = load.Nanoseconds()
	obj["prompt_eval_count"] = s.PromptTokens
	obj["prompt_eval_duration"] = s.PromptDuration.Nanoseconds()
	obj["eval_count"] = s.EvalTokens
	obj["eval_duration"] = s.EvalDuration.Nanoseconds()
}

// addContextStats reports how full the context window is, for the chat's
// context meter.
func addContextStats(obj map[string]interface{}, s *runtime.GenStats, r *runtime.LLMRunner) {
	if s.ContextTokens > 0 {
		obj["context_tokens"] = s.ContextTokens
	}
	if r != nil {
		if n := r.ContextSize(); n > 0 {
			obj["context_size"] = n
		}
	}
}
