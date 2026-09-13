package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vortelio/vortelio/internal/cli/commands"
	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/updater"
	"golang.org/x/term"
)

// ─── model types ─────────────────────────────────────────────────────────────

var modelTypes = []struct {
	key   string
	label string
}{
	{"llm", "Language (LLM)"},
	{"image", "Image"},
	{"audio", "Audio"},
	{"video", "Video"},
	{"3d", "3D"},
}

func typeLabels() []string {
	out := make([]string, len(modelTypes))
	for i, t := range modelTypes {
		out[i] = t.label
	}
	return out
}

// ─── main menu ───────────────────────────────────────────────────────────────

func runInteractiveMenu() error {
	// The update check talks to GitHub; run it in the background so the menu
	// appears instantly (it used to block for up to 2 s on every start, and for
	// the whole timeout when offline).
	var labelMu sync.Mutex
	updateLabel := "🔄 Update Vortelio (reinstall latest)"
	go func() {
		if info, err := updater.CheckWithTimeout(5 * time.Second); err == nil && info.Available {
			labelMu.Lock()
			updateLabel = fmt.Sprintf("🔄 Update Vortelio → %s", info.Latest)
			labelMu.Unlock()
		}
	}()
	for {
		labelMu.Lock()
		updateLabel := updateLabel
		labelMu.Unlock()
		type menuAction struct {
			label string
			run   func() error
		}
		actions := []menuAction{
			{"Chat with a model", handleChatta},
			{"🧑‍💻 Code agent (terminal)", func() error { return commands.NewCodeCommand().Run(nil) }},
			{"Download a model", handleScarica},
			{"Cloud Models", handleModelloCloud},
			{"AI Agents", handleAgentiAI},
			{"🤖 CrewAI Orchestration", handleCrewAI},
			{"Import from Ollama", handleImportOllama},
			{"Advanced Tools", handleAdvancedTools},
		}
		if updateLabel != "" {
			actions = append(actions, menuAction{updateLabel, func() error {
				// Always force a reinstall of the latest main via uv (the user
				// installs from git, which can be ahead of the latest release).
				if err := commands.NewUpdateCommand().Run([]string{"--force"}); err != nil {
					fmt.Printf("\n  Update failed: %s\n", err)
				}
				os.Exit(0)
				return nil
			}})
		}
		actions = append(actions,
			menuAction{"Open Web UI", func() error { return reExec("gui") }},
			menuAction{"Show commands", func() error { return reExec("help") }},
		)
		items := make([]string, len(actions))
		for i, action := range actions {
			items[i] = action.label
		}
		sel := selectMenu("Vortelio", items)
		switch sel {
		case -1:
			os.Exit(0)
		default:
			if err := actions[sel].run(); err != nil {
				return err
			}
		}
	}
}

// ─── chat ─────────────────────────────────────────────────────────────────────

func handleChatta() error {
	for {
		tSel := selectMenu("Model type", typeLabels())
		if tSel < 0 {
			return nil // back to main menu
		}
		chosen := modelTypes[tSel]

		// installed models of that type
		store := hub.NewModelStore()
		all, _ := store.List()
		var models []*hub.Model
		for _, m := range all {
			if m.Type == chosen.key {
				models = append(models, m)
			}
		}

		if len(models) == 0 {
			waitKey(fmt.Sprintf(
				"  No %s model installed.\n  Use «Download a model» to get one.",
				chosen.label,
			))
			continue // back to type selection
		}

		labels := make([]string, len(models))
		for i, m := range models {
			name := m.DisplayName
			if name == "" {
				name = m.Name + ":" + m.Tag
			}
			labels[i] = fmt.Sprintf("%-36s %s", name, m.SizeHuman())
		}

		mSel := selectMenu("Select model — "+chosen.label, labels)
		if mSel < 0 {
			continue // back to type selection
		}

		m := models[mSel]
		fmt.Print("\033[H\033[2J")
		return reExec("run", fmt.Sprintf("%s/%s:%s", m.Type, m.Name, m.Tag))
	}
}

// ─── download ────────────────────────────────────────────────────────────────

func handleScarica() error {
	tSel := selectMenu("Model type", typeLabels())
	if tSel < 0 {
		return nil
	}
	chosen := modelTypes[tSel]

	fmt.Print("\033[H\033[2J")
	fmt.Printf("\n  %s model (e.g. %s/hf.co/owner/repo:file)\n  > ", chosen.label, chosen.key)
	var ref string
	fmt.Scanln(&ref)
	if ref != "" {
		// if the user didn't already include the type prefix, add it
		prefix := chosen.key + "/"
		if !strings.HasPrefix(ref, prefix) {
			ref = prefix + ref
		}
		return reExec("pull", ref)
	}
	return nil
}

// ─── import from Ollama ──────────────────────────────────────

func handleImportOllama() error {
	sel := selectMenu("Import from Ollama", []string{
		"Import all Ollama models",
		"Dry run (preview only)",
		"← Back",
	})
	if sel < 0 || sel == 2 {
		return nil
	}
	dryRun := sel == 1
	fmt.Print("\033[H\033[2J")
	fmt.Println()
	if dryRun {
		fmt.Println("  Scanning Ollama models (dry run)…")
	} else {
		fmt.Println("  Importing Ollama models…")
	}
	fmt.Println()

	port := fmt.Sprintf("%d", config.Get().Port)
	url := fmt.Sprintf("http://localhost:%s/api/import/ollama", port)
	body, _ := json.Marshal(map[string]interface{}{"dry_run": dryRun})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  Could not reach Vortelio server:\n  %s\n\n  Make sure the server is running (vortelio serve).", err.Error()))
		return nil
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		waitKey("  ❌  Invalid response from server.")
		return nil
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return nil
	}

	imported, _ := result["imported"].([]interface{})
	skipped, _ := result["skipped"].([]interface{})
	ollamaPath, _ := result["ollama_path"].(string)
	dryLabel := ""
	if dryRun {
		dryLabel = " (dry run)"
	}
	fmt.Printf("  Ollama path: %s%s\n\n", ollamaPath, dryLabel)
	action := "Imported"
	if dryRun {
		action = "Would import"
	}
	fmt.Printf("  ✅  %s: %d model(s)\n", action, len(imported))
	for _, m := range imported {
		if mm, ok := m.(map[string]interface{}); ok {
			fmt.Printf("      + %v\n", mm["model"])
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("\n  Skipped: %d\n", len(skipped))
		for _, s := range skipped {
			if sm, ok := s.(map[string]interface{}); ok {
				fmt.Printf("      · %v — %v\n", sm["model"], sm["reason"])
			}
		}
	}
	waitKey("")
	return nil
}

// ─── advanced tools ───────────────────────────────────────────

func handleAdvancedTools() error {
	for {
		sel := selectMenu("Advanced Tools", []string{
			"📚 RAG — query a collection",
			"🔍 GGUF Inspect",
			"📋 Audit Log (last 20)",
			"⚖  Compare models",
			"{ } Structured output",
			"📝 Summarize text",
			"💡 Think API (chain-of-thought)",
			"🗺  Model Router",
			"🖥  Server status & loaded models",
			"🛠  Server config",
			"← Back",
		})
		switch sel {
		case -1, 10:
			return nil
		case 0:
			handleRAGQuery()
		case 1:
			handleGGUFInspect()
		case 2:
			handleAuditLog()
		case 3:
			handleTUICompare()
		case 4:
			handleTUIStructured()
		case 5:
			handleTUISummarize()
		case 6:
			handleTUIThink()
		case 7:
			handleTUIRoute()
		case 8:
			handleTUIServerStatus()
		case 9:
			handleTUIServerConfig()
		}
	}
}

func apiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	port := fmt.Sprintf("%d", config.Get().Port)
	url := fmt.Sprintf("http://localhost:%s%s", port, endpoint)
	var resp *http.Response
	var err error
	if payload != nil {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err = http.DefaultClient.Do(req)
	} else {
		resp, err = http.Get(url)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
		// Non-JSON body (e.g. a plain-text error page). Surface the HTTP status
		// instead of silently returning an empty map.
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("invalid server response: %w", decErr)
	}
	// If the call failed but the body carried no "error" field, synthesize one
	// so callers (which check result["error"]) report the failure.
	if resp.StatusCode >= 400 {
		if _, ok := result["error"].(string); !ok {
			result["error"] = fmt.Sprintf("server returned HTTP %d", resp.StatusCode)
		}
	}
	return result, nil
}

func handleRAGQuery() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  📚 RAG Query")
	fmt.Println()
	fmt.Print("  Embed model (e.g. llm/nomic-embed-text:latest): ")
	model := strings.TrimSpace(readLineSimple())
	if model == "" {
		return
	}
	fmt.Print("  Collection name [default]: ")
	coll := strings.TrimSpace(readLineSimple())
	if coll == "" {
		coll = "default"
	}
	fmt.Print("  Query: ")
	query := strings.TrimSpace(readLineSimple())
	if query == "" {
		return
	}

	fmt.Println("\n  Searching…")
	result, err := apiCall("POST", "/api/rag/query", map[string]interface{}{
		"model": model, "collection": coll, "query": query, "top_k": 5,
	})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}

	results, _ := result["results"].([]interface{})
	if len(results) == 0 {
		waitKey("  No results found.")
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Printf("\n  📚 RAG Results for: %q\n", query)
	fmt.Println()
	for i, r := range results {
		if rm, ok := r.(map[string]interface{}); ok {
			score, _ := rm["score"].(float64)
			text, _ := rm["text"].(string)
			text = truncRunes(text, 197)
			fmt.Printf("  #%d  score: %.4f\n", i+1, score)
			fmt.Printf("      %s\n\n", strings.ReplaceAll(text, "\n", "\n      "))
		}
	}
	waitKey("")
}

func handleGGUFInspect() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🔍 GGUF Inspect")
	fmt.Println()
	fmt.Print("  GGUF file path: ")
	path := strings.TrimSpace(readLineSimple())
	if path == "" {
		return
	}

	result, err := apiCall("POST", "/api/gguf/inspect", map[string]interface{}{"path": path})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}

	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🔍 GGUF Metadata")
	fmt.Println()
	for k, v := range result {
		val := truncRunes(fmt.Sprintf("%v", v), 57)
		fmt.Printf("  %-30s  %s\n", k, val)
	}
	waitKey("")
}

func handleAuditLog() {
	result, err := apiCall("GET", "/api/audit?limit=20", nil)
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}

	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  📋 Audit Log (last 20)")
	fmt.Println()
	entries, _ := result["entries"].([]interface{})
	if len(entries) == 0 {
		waitKey("  No audit entries.")
		return
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if e, ok := entries[i].(map[string]interface{}); ok {
			ts, _ := e["timestamp"].(string)
			if len(ts) > 19 {
				ts = ts[:19]
			}
			method, _ := e["method"].(string)
			path, _ := e["path"].(string)
			status := fmt.Sprintf("%.0f", e["status"])
			dur := fmt.Sprintf("%.0fms", e["duration_ms"])
			fmt.Printf("  %s  %-6s %-35s  %s  %s\n", ts, method, path, status, dur)
		}
	}
	waitKey("")
}

func handleTUICompare() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  ⚖  Compare Models")
	fmt.Println()
	fmt.Print("  Models (comma-separated, e.g. llm/mistral:7b,llm/llama3:8b): ")
	modelsRaw := strings.TrimSpace(readLineSimple())
	if modelsRaw == "" {
		return
	}
	models := strings.Split(modelsRaw, ",")
	for i, m := range models {
		models[i] = strings.TrimSpace(m)
	}
	fmt.Print("  System prompt (enter to skip): ")
	system := strings.TrimSpace(readLineSimple())
	fmt.Print("  Prompt: ")
	prompt := strings.TrimSpace(readLineSimple())
	if prompt == "" {
		return
	}
	fmt.Println("\n  Running comparison…")
	result, err := apiCall("POST", "/api/compare", map[string]interface{}{
		"models": models, "prompt": prompt, "system": system,
	})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  ⚖  Comparison Results")
	fmt.Println()
	results, _ := result["results"].([]interface{})
	for _, r := range results {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		model, _ := rm["model"].(string)
		resp, _ := rm["response"].(string)
		durMS, _ := rm["duration_ms"].(float64)
		errStr, _ := rm["error"].(string)
		fmt.Printf("  ── %s  (%.0fms) ──\n", model, durMS)
		if errStr != "" {
			fmt.Printf("  ❌ %s\n\n", errStr)
			continue
		}
		resp = truncRunes(resp, 397)
		fmt.Printf("  %s\n\n", strings.ReplaceAll(resp, "\n", "\n  "))
	}
	waitKey("")
}

func handleTUIStructured() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  { } Structured Output")
	fmt.Println()
	fmt.Print("  Model (e.g. llm/mistral:7b): ")
	model := strings.TrimSpace(readLineSimple())
	if model == "" {
		return
	}
	fmt.Print("  Prompt: ")
	prompt := strings.TrimSpace(readLineSimple())
	if prompt == "" {
		return
	}
	fmt.Print("  JSON schema (enter to skip — free JSON): ")
	schema := strings.TrimSpace(readLineSimple())

	payload := map[string]interface{}{"model": model, "prompt": prompt}
	if schema != "" {
		payload["schema"] = json.RawMessage(schema)
	}
	fmt.Println("\n  Generating…")
	result, err := apiCall("POST", "/api/structured", payload)
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  { } Structured Result")
	fmt.Println()
	raw, _ := result["raw"].(string)
	raw = truncRunes(raw, 1997)
	fmt.Printf("  %s\n", strings.ReplaceAll(raw, "\n", "\n  "))
	if pe, ok := result["parse_err"].(string); ok && pe != "" {
		fmt.Printf("\n  ⚠  parse error: %s\n", pe)
	}
	waitKey("")
}

func handleTUISummarize() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  📝 Summarize")
	fmt.Println()
	fmt.Print("  Model (e.g. llm/mistral:7b): ")
	model := strings.TrimSpace(readLineSimple())
	if model == "" {
		return
	}
	fmt.Println("  Style: [1] paragraph  [2] bullets  [3] tldr")
	fmt.Print("  Choice [1]: ")
	styleChoice := strings.TrimSpace(readLineSimple())
	style := "paragraph"
	switch styleChoice {
	case "2":
		style = "bullets"
	case "3":
		style = "tldr"
	}
	fmt.Print("  Paste text (end with a line containing only '---')\n  ")
	var textLines []string
	for {
		line := readLineSimple()
		if line == "---" {
			break
		}
		textLines = append(textLines, line)
		fmt.Print("  ")
	}
	text := strings.Join(textLines, "\n")
	if text == "" {
		return
	}
	fmt.Println("\n  Summarizing…")
	result, err := apiCall("POST", "/api/summarize", map[string]interface{}{
		"model": model, "text": text, "style": style,
	})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  📝 Summary")
	fmt.Println()
	summary, _ := result["summary"].(string)
	chunks, _ := result["chunks"].(float64)
	fmt.Printf("  Chunks processed: %.0f\n\n", chunks)
	fmt.Printf("  %s\n", strings.ReplaceAll(summary, "\n", "\n  "))
	waitKey("")
}

func handleTUIThink() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  💡 Think API (chain-of-thought)")
	fmt.Println()
	fmt.Print("  Model (e.g. llm/deepseek-r1:8b): ")
	model := strings.TrimSpace(readLineSimple())
	if model == "" {
		return
	}
	fmt.Print("  System prompt (enter to skip): ")
	system := strings.TrimSpace(readLineSimple())
	fmt.Print("  Prompt: ")
	prompt := strings.TrimSpace(readLineSimple())
	if prompt == "" {
		return
	}
	fmt.Println("\n  Thinking…")
	result, err := apiCall("POST", "/api/think", map[string]interface{}{
		"model": model, "prompt": prompt, "system": system,
	})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  💡 Think Result")
	fmt.Println()
	thinking, _ := result["thinking"].(string)
	answer, _ := result["answer"].(string)
	if thinking != "" {
		thinking = truncRunes(thinking, 597)
		fmt.Println("  ── Reasoning ──")
		fmt.Printf("  \033[2m%s\033[0m\n\n", strings.ReplaceAll(thinking, "\n", "\n  "))
	}
	fmt.Println("  ── Answer ──")
	fmt.Printf("  %s\n", strings.ReplaceAll(answer, "\n", "\n  "))
	waitKey("")
}

func handleTUIRoute() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🗺  Model Router")
	fmt.Println()
	fmt.Println("  Task types: chat, code, embed, vision, image, audio, video, 3d")
	fmt.Print("  Task [chat]: ")
	task := strings.TrimSpace(readLineSimple())
	if task == "" {
		task = "chat"
	}
	fmt.Print("  Optional prompt for heuristic detect (enter to skip): ")
	prompt := strings.TrimSpace(readLineSimple())

	result, err := apiCall("POST", "/api/route", map[string]interface{}{
		"task": task, "prompt": prompt,
	})
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := result["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🗺  Router Result")
	fmt.Println()
	model, _ := result["model"].(string)
	size, _ := result["size"].(string)
	if model == "" {
		waitKey(fmt.Sprintf("  ⚠  No model found for task: %s", task))
		return
	}
	fmt.Printf("  Task:   %s\n", task)
	fmt.Printf("  Model:  %s\n", model)
	if size != "" {
		fmt.Printf("  Size:   %s\n", size)
	}
	if cands, ok := result["candidates"].([]interface{}); ok && len(cands) > 1 {
		fmt.Printf("  Others: ")
		for i, c := range cands {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(c)
		}
		fmt.Println()
	}
	waitKey("")
}

func handleTUIServerStatus() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🖥  Server Status")
	fmt.Println()
	status, err := apiCall("GET", "/api/status", nil)
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	name, _ := status["name"].(string)
	ver, _ := status["version"].(string)
	hw, _ := status["hardware"].(string)
	cnt, _ := status["model_count"].(float64)
	fmt.Printf("  Server:   %s %s\n", name, ver)
	fmt.Printf("  Hardware: %s\n", hw)
	fmt.Printf("  Models:   %.0f installed\n\n", cnt)

	ps, err := apiCall("GET", "/api/ps", nil)
	if err == nil {
		models, _ := ps["models"].([]interface{})
		if len(models) == 0 {
			fmt.Println("  No models currently loaded in memory.")
		} else {
			fmt.Println("  Loaded in memory:")
			for _, m := range models {
				if mm, ok := m.(map[string]interface{}); ok {
					name := mm["name"]
					if name == nil {
						name = mm["model"]
					}
					fmt.Printf("    • %v\n", name)
				}
			}
		}
	}
	waitKey("")
}

func handleTUIServerConfig() {
	result, err := apiCall("GET", "/api/config", nil)
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	fmt.Print("\033[H\033[2J")
	fmt.Println("\n  🛠  Server Config")
	fmt.Println()
	port, _ := result["port"].(float64)
	bind, _ := result["bind_addr"].(string)
	apiKey, _ := result["api_key"].(string)
	ollamaPort, _ := result["ollama_port"].(float64)
	timeout, _ := result["cloud_timeout_sec"].(float64)
	python, _ := result["python_bin"].(string)
	fmt.Printf("  Port:             %.0f\n", port)
	fmt.Printf("  Bind addr:        %s\n", bind)
	fmt.Printf("  API key:          %s\n", func() string {
		if apiKey == "" {
			return "(none — no auth)"
		}
		return "(set)"
	}())
	fmt.Printf("  Ollama port:      %.0f\n", ollamaPort)
	fmt.Printf("  Cloud timeout:    %.0fs\n", timeout)
	fmt.Printf("  Python bin:       %s\n", func() string {
		if python == "" {
			return "(auto-detect)"
		}
		return python
	}())
	fmt.Println()
	fmt.Println("  Edit config? [y/N]: ")
	choice := strings.TrimSpace(readLineSimple())
	if strings.ToLower(choice) != "y" {
		return
	}

	patch := map[string]interface{}{}
	fmt.Printf("  New port [%.0f]: ", port)
	if v := strings.TrimSpace(readLineSimple()); v != "" {
		var p int
		fmt.Sscanf(v, "%d", &p)
		if p > 0 {
			patch["port"] = p
		}
	}
	fmt.Printf("  New bind addr [%s]: ", bind)
	if v := strings.TrimSpace(readLineSimple()); v != "" {
		patch["bind_addr"] = v
	}
	fmt.Print("  New API key (enter to clear, '-' to keep): ")
	if v := strings.TrimSpace(readLineSimple()); v != "-" {
		patch["api_key"] = v
	}
	fmt.Printf("  New Python bin [%s]: ", func() string {
		if python == "" {
			return "auto"
		}
		return python
	}())
	if v := strings.TrimSpace(readLineSimple()); v != "" {
		patch["python_bin"] = v
	}

	if len(patch) == 0 {
		waitKey("  No changes.")
		return
	}
	saveResult, err := apiCall("POST", "/api/config", patch)
	if err != nil {
		waitKey(fmt.Sprintf("  ❌  %s", err.Error()))
		return
	}
	if errMsg, ok := saveResult["error"].(string); ok {
		waitKey(fmt.Sprintf("  ❌  %s", errMsg))
		return
	}
	waitKey("  ✅ Config saved. Restart Vortelio for port/bind changes to take effect.")
}

// readLineSimple reads a line from stdin in normal (cooked) terminal mode.
// The terminal driver already echoes typed characters and handles backspace,
// so we must NOT echo again here (doing so caused every keystroke to appear
// twice). We read byte-by-byte — rather than buffering — so that any extra
// bytes the user pastes remain in the OS buffer for the next read (important
// for the multi-line paste in the summarize flow). CR is skipped so Windows
// CRLF line endings don't produce spurious empty fields.
func readLineSimple() string {
	var line strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			break
		}
		switch buf[0] {
		case '\n':
			return line.String()
		case '\r':
			// ignore — CRLF on Windows
		default:
			line.WriteByte(buf[0])
		}
	}
	return line.String()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// truncRunes shortens s to at most n runes (plus an ellipsis). Slicing bytes
// (s[:n]) could cut a multi-byte UTF-8 character in half and print garbage.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func waitKey(msg string) {
	fmt.Print("\033[H\033[2J")
	fmt.Println()
	fmt.Println(msg)
	fmt.Println()
	fmt.Print("  \033[2mPress any key to continue…\033[0m")

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err == nil {
		// Read the whole key sequence: an arrow key is 3 bytes, and leftover
		// bytes would otherwise be consumed as navigation by the next menu.
		buf := make([]byte, 8)
		os.Stdin.Read(buf)
		term.Restore(fd, old)
	}
}

// ─── selectMenu ──────────────────────────────────────────────────────────────
//
// Shows an interactive menu and returns the selected index, or -1
// if the user presses Esc or Ctrl-C.
//
// Layout (N = len(items), final cursor = end of hint line, no \n):
//
//	[blank]
//	  <title>
//	[blank]
//	  item 0          ← N+1 lines above cursor
//	  item 1
//	  …
//	  item N-1
//	[blank]
//	  ↑/↓  enter  esc ← cursor here
func selectMenu(title string, items []string) int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return -1
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1
	}
	defer term.Restore(fd, old)

	sel := 0
	menuDrawFull(title, items, sel)

	buf := make([]byte, 4)
	for {
		n, _ := os.Stdin.Read(buf)
		if n == 0 {
			break
		}
		b := buf[:n]
		switch {
		case b[0] == 27 && n == 1, b[0] == 3: // esc / ctrl-c
			fmt.Print("\033[H\033[2J")
			return -1
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A': // up
			if sel > 0 {
				sel--
				menuDrawItems(items, sel)
			}
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B': // down
			if sel < len(items)-1 {
				sel++
				menuDrawItems(items, sel)
			}
		case b[0] == 13 || b[0] == 10: // enter
			fmt.Print("\033[H\033[2J")
			return sel
		}
	}
	return -1
}

// The menu is drawn while the terminal is in raw mode. On Unix raw mode turns
// off output post-processing, so a bare "\n" no longer returns the carriage and
// every line would start further right than the previous one. Always emit CRLF.
func menuDrawFull(title string, items []string, sel int) {
	fmt.Print("\033[H\033[2J")
	fmt.Print("\r\n")
	fmt.Printf("  %s\r\n", title)
	fmt.Print("\r\n")
	for i, item := range items {
		if i == sel {
			fmt.Printf("  \033[1m> %s\033[0m\r\n", item)
		} else {
			fmt.Printf("    %s\r\n", item)
		}
	}
	fmt.Print("\r\n")
	fmt.Print("  \033[2m↑/↓  enter  esc\033[0m")
}

// menuDrawItems updates only the items in-place (no full-screen clear).
// It moves up len(items)+1 lines (items + final blank line), rewrites everything
// up to the hint line, and leaves the cursor where it was.
func menuDrawItems(items []string, sel int) {
	fmt.Printf("\033[%dA\r", len(items)+1)
	for i, item := range items {
		fmt.Print("\033[2K")
		if i == sel {
			fmt.Printf("  \033[1m> %s\033[0m\r\n", item)
		} else {
			fmt.Printf("    %s\r\n", item)
		}
	}
	fmt.Print("\033[2K\r\n\033[2K  \033[2m↑/↓  enter  esc\033[0m")
}

// ─── reExec ──────────────────────────────────────────────────────────────────

func reExec(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	proc, err := os.StartProcess(exe, append([]string{exe}, args...), &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return err
	}
	s, err := proc.Wait()
	if err != nil {
		return err
	}
	if !s.Success() {
		os.Exit(s.ExitCode())
	}
	return nil
}
