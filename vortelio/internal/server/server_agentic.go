package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/mcp"
	rt "github.com/vortelio/vortelio/internal/runtime"
)

// ── Approval broker ────────────────────────────────────────────────────────────
//
// Risky coding tool calls (shell, write, edit) block on a decision delivered out
// of band by POST /api/agentic/approve. The streaming chat connection emits an
// "approval_request" tool event; the UI shows approve/deny and resolves it.

type approvalReq struct {
	ch        chan bool
	createdAt time.Time
}

var (
	approvalsMu sync.Mutex
	approvals   = map[string]*approvalReq{}
)

func registerApproval(id string) chan bool {
	ch := make(chan bool, 1)
	approvalsMu.Lock()
	approvals[id] = &approvalReq{ch: ch, createdAt: time.Now()}
	approvalsMu.Unlock()
	return ch
}

func resolveApproval(id string, ok bool) bool {
	approvalsMu.Lock()
	a, found := approvals[id]
	if found {
		delete(approvals, id)
	}
	approvalsMu.Unlock()
	if !found {
		return false
	}
	a.ch <- ok
	return true
}

// ── ask_user (interactive question with options) ────────────────────
var (
	asksMu sync.Mutex
	asks   = map[string]chan string{}
)

func registerAsk(id string) chan string {
	ch := make(chan string, 1)
	asksMu.Lock()
	asks[id] = ch
	asksMu.Unlock()
	return ch
}

func resolveAsk(id, answer string) bool {
	asksMu.Lock()
	ch, ok := asks[id]
	if ok {
		delete(asks, id)
	}
	asksMu.Unlock()
	if !ok {
		return false
	}
	ch <- answer
	return true
}

// POST /api/agentic/answer  — {id, answer}
func handleAgenticAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "use POST")
		return
	}
	var req struct {
		ID     string `json:"id"`
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON")
		return
	}
	if !resolveAsk(req.ID, req.Answer) {
		jsonError(w, 404, "no pending question with that id")
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}

// POST /api/agentic/approve  — {id, approved}
func handleAgenticApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, 405, "use POST")
		return
	}
	var req struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if !resolveApproval(req.ID, req.Approved) {
		jsonError(w, 404, "no pending approval with that id (it may have timed out)")
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}

// autoSystemPrompt prepends a short instruction telling the model it can call
// the available tools (web search, math, files, media generation) on its own.
// Used in smart/auto mode so a beginner never has to toggle anything.
func autoSystemPrompt(existing string) string {
	nudge := "You are a friendly, helpful assistant. Chat naturally with the user and answer in their own " +
		"language. You also have tools available (web search and image/audio/video/3D generation) that you may " +
		"use when the user clearly needs up-to-date information or asks you to create media. For greetings, " +
		"casual conversation, or anything you already know, just reply normally without using a tool. " +
		"After a tool returns, write a clear, complete answer to the user IN THEIR LANGUAGE using the results — " +
		"never just describe or repeat the raw JSON/output. " +
		"When a visual or interactive answer would help (a chart, diagram, calculator, table, mock UI, game, " +
		"animation…), you MAY reply with a single self-contained ```html code block (inline CSS/JS allowed) — it " +
		"will be rendered live for the user. Use this whenever building it answers the question better than text. " +
		conciseNudgeEN + " " +
		askUserNudgeEN
	if strings.TrimSpace(existing) == "" {
		return nudge
	}
	return nudge + "\n\n" + existing
}

// autonomousSystemPrompt drives a goal-seeking agent that keeps working across
// many tool rounds until the objective is fully achieved (e.g. building a whole
// project in Developer mode), without asking for confirmation at each step.
func autonomousSystemPrompt(existing string) string {
	nudge := "You are an AUTONOMOUS agent. The user gives you a GOAL; your job is to reach it on your own. " +
		"Work in a loop: (1) think briefly and break the goal into concrete steps; (2) use your tools " +
		"(read/list/glob/grep files, write_file/edit, run_shell, run code, web_search, media, create_skill) to " +
		"execute each step; (3) verify your work by reading files back and running it; (4) fix problems and " +
		"continue. Build complete, working projects: create every needed file with real content, wire them " +
		"together, and run them to confirm they work. Do NOT stop to ask for permission or confirmation — act, " +
		"and only pause if you truly cannot proceed. When you hit a reusable procedure worth keeping, call " +
		"create_skill to save it. Keep going until the goal is fully met, then end with a short summary of what " +
		"you built and how to use it. " +
		"Stay autonomous, but if you hit a genuinely ambiguous choice you cannot reasonably decide on your own, " +
		"use ask_user(question, 2-5 options) to get a quick graphical decision from the user, then continue. " +
		conciseNudgeEN
	if strings.TrimSpace(existing) == "" {
		return nudge
	}
	return nudge + "\n\n" + existing
}

// ── Agentic provider builder ───────────────────────────────────────────────────

// CodingSystemPrompt returns the system prompt for the CLI coding agent, matching
// the GUI behaviour (autonomous goal-seeking when requested).
func CodingSystemPrompt(autonomous bool) string {
	if autonomous {
		return autonomousSystemPrompt("")
	}
	return "Sei Vortelio Code, un agente di coding che lavora nel terminale dentro la cartella di lavoro " +
		"descritta nel CONTESTO WORKSPACE. Rispondi nella lingua dell'utente. " +
		"Sei orientato al progetto corrente: le richieste dell'utente riguardano quasi sempre i file e il codice " +
		"di QUESTA cartella, non attività generiche. " +
		"Prima di rispondere su \"il progetto\", \"questo\", \"qui\" o un file citato, USA gli strumenti " +
		"(list_directory, read_file, glob_search, grep_search) per guardare i file reali: non indovinare e non inventare contenuti. " +
		"Per modificare il progetto usa write_file / edit_file con percorsi relativi alla cartella di lavoro e " +
		"riferisci sempre il percorso esatto. Non affermare di aver creato o cambiato un file se non hai chiamato lo strumento. " +
		"Hai anche strumenti web (web_search) e di generazione media (immagini/audio/video/3D): usali solo quando " +
		"l'utente li chiede davvero, e per impostazione predefinita salva gli artefatti dentro la cartella di lavoro. " +
		"Dopo che uno strumento restituisce un risultato, scrivi una risposta chiara e completa nella lingua dell'utente; " +
		"non limitarti a ripetere il JSON grezzo. " +
		"Per file molto grandi (dataset, migliaia di righe) NON tentare un'unica scrittura gigante: " +
		"crea il file e poi AGGIUNGI il contenuto a blocchi con write_file e append=true, ripetendo finché è completo, " +
		"e alla fine verifica la dimensione/righe con read_file. " +
		conciseNudgeIT + " " +
		askUserNudgeIT
}

// conciseNudgeIT / conciseNudgeEN push the model to minimize output tokens:
// answer essentially, without filler, padding or redundant recap.
const conciseNudgeIT = "SII CONCISO e minimizza i token di output: vai dritto al punto, niente preamboli, " +
	"chiacchiere, ripetizioni o riassunti ridondanti. Rispondi solo a ciò che è stato chiesto, con il minor numero " +
	"di parole possibile; preferisci elenchi puntati brevi a lunghe tabelle e paragrafi, ometti dettagli ovvi o non " +
	"richiesti. Resta corretto e completo, ma essenziale: meno testo è meglio."

const conciseNudgeEN = "BE CONCISE and minimize output tokens: get straight to the point, no preamble, filler, " +
	"repetition or redundant recap. Answer only what was asked, in as few words as possible; prefer short bullet " +
	"lists over long tables and paragraphs, and omit obvious or unrequested detail. Stay correct and complete, but lean."

// askUserNudgeIT / askUserNudgeEN instruct the model to use the graphical
// ask_user tool instead of guessing when it needs a decision from the user.
const askUserNudgeIT = "Quando ti serve una decisione o un chiarimento dall'utente (più strade valide, dati mancanti, " +
	"conferma di un approccio), NON tirare a indovinare: chiama lo strumento ask_user con una domanda chiara e 2-5 " +
	"opzioni — all'utente comparirà un popup grafico con i pulsanti delle opzioni e un campo \"Altro\" per la risposta libera. Usalo solo quando serve davvero."

const askUserNudgeEN = "When you need a decision or clarification from the user (several valid options, missing info, " +
	"confirming an approach), call the ask_user tool with a clear question and 2-5 options instead of guessing — " +
	"the user gets a graphical popup with option buttons and an \"Other\" free-text field. Use it only when it genuinely helps."

// workspaceContext tells the agent which folder it is working in AND gives it a
// live snapshot of that folder (git branch, project type, file tree) so it knows
// "where it is" and what "this project" / "qui" refers to without having to guess.
func workspaceContext(cfg *AgenticConfig) string {
	if cfg == nil || !cfg.Coding || strings.TrimSpace(cfg.WorkingDir) == "" {
		return ""
	}
	dir := cfg.WorkingDir
	var b strings.Builder
	b.WriteString("=== CONTESTO WORKSPACE (aggiornato a questo turno) ===\n")
	b.WriteString("CARTELLA DI LAVORO (radice del progetto): " + dir + "\n")
	if branch, clean := workspaceGitInfo(dir); branch != "" {
		st := "modificato"
		if clean {
			st = "pulito"
		}
		b.WriteString("Git: branch " + branch + " (" + st + ")\n")
	}
	if kind := detectProjectKind(dir); kind != "" {
		b.WriteString("Tipo progetto: " + kind + "\n")
	}
	if tree := workspaceTree(dir); tree != "" {
		b.WriteString("Contenuto della cartella (i file REALI presenti qui ora):\n" + tree)
	}
	if sum := projectSummaryExcerpt(dir); sum != "" {
		b.WriteString("\nRIASSUNTO DEL PROGETTO (da PROJECT.md, generato da /init):\n" + sum + "\n")
		b.WriteString("Se durante il lavoro modifichi qualcosa che rende PROJECT.md obsoleto (nuovi file/moduli, comandi, dipendenze, configurazione), AGGIORNA PROJECT.md con write_file/edit_file per tenerlo allineato.\n")
	}
	b.WriteString("\nREGOLE DI CONTESTO:\n")
	b.WriteString("- Quando l'utente dice \"questo progetto\", \"qui\", \"il sistema\", \"questa cartella\" si riferisce SEMPRE alla cartella di lavoro qui sopra. Non chiedere di quale progetto si tratta: leggilo.\n")
	b.WriteString("- Prima di rispondere a domande sul progetto o di eseguire azioni su di esso, ISPEZIONA i file reali con list_directory / read_file invece di indovinare o inventare.\n")
	b.WriteString("- Se l'utente cita un percorso o un file (es. agent.py), aprilo con read_file prima di rispondere.\n")
	b.WriteString("- I percorsi relativi degli strumenti file sono relativi a questa cartella; usa write_file / edit_file per creare o modificare file e indica SEMPRE il percorso esatto.\n")
	b.WriteString("- Non dire mai che un file è stato creato o modificato se non hai davvero chiamato lo strumento corrispondente.\n")
	b.WriteString("- Non confondere la cartella di lavoro con cartelle temporanee di sistema: salva e riferisci i file dentro la cartella di lavoro salvo richiesta esplicita diversa.\n")
	return b.String()
}

// projectSummaryExcerpt returns the first lines of PROJECT.md if present, so the
// agent always has the project summary in context and can keep it up to date.
func projectSummaryExcerpt(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "PROJECT.md"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	const maxLines = 40
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "… (PROJECT.md continua)")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// workspaceGitInfo returns the current branch and whether the tree is clean.
func workspaceGitInfo(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(string(out))
	st, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	return branch, strings.TrimSpace(string(st)) == ""
}

// detectProjectKind guesses the project type from marker files in the root.
func detectProjectKind(dir string) string {
	markers := []struct{ file, kind string }{
		{"go.mod", "Go"},
		{"package.json", "Node.js / JavaScript"},
		{"pyproject.toml", "Python"},
		{"requirements.txt", "Python"},
		{"Cargo.toml", "Rust"},
		{"pom.xml", "Java (Maven)"},
		{"build.gradle", "Java/Kotlin (Gradle)"},
		{"CMakeLists.txt", "C/C++ (CMake)"},
	}
	var kinds []string
	seen := map[string]bool{}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m.file)); err == nil {
			if !seen[m.kind] {
				kinds = append(kinds, m.kind)
				seen[m.kind] = true
			}
		}
	}
	return strings.Join(kinds, ", ")
}

// workspaceTree returns a compact listing of the workspace: top-level entries
// plus one level of nesting, skipping noise dirs, capped so it never floods the
// prompt. This is what lets the model know what "this project" actually contains.
func workspaceTree(dir string) string {
	skip := map[string]bool{
		".git": true, "node_modules": true, ".venv": true, "venv": true,
		"__pycache__": true, "dist": true, "build": true, ".next": true,
		"target": true, ".idea": true, ".vscode": true, "vendor": true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	var b strings.Builder
	lines := 0
	const maxLines = 60
	for _, e := range entries {
		if lines >= maxLines {
			b.WriteString("  … (altri file omessi)\n")
			break
		}
		name := e.Name()
		if e.IsDir() {
			b.WriteString("  " + name + "/\n")
			lines++
			if skip[name] {
				continue
			}
			// one level of children
			children, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			sort.Slice(children, func(i, j int) bool {
				if children[i].IsDir() != children[j].IsDir() {
					return children[i].IsDir()
				}
				return children[i].Name() < children[j].Name()
			})
			shown := 0
			for _, c := range children {
				if lines >= maxLines {
					break
				}
				if shown >= 12 {
					b.WriteString("    … (" + name + " ha altri file)\n")
					lines++
					break
				}
				suffix := ""
				if c.IsDir() {
					suffix = "/"
				}
				b.WriteString("    " + c.Name() + suffix + "\n")
				lines++
				shown++
			}
		} else {
			b.WriteString("  " + name + "\n")
			lines++
		}
	}
	return b.String()
}

// SkillInfo is a lightweight skill descriptor for the CLI.
type SkillInfo struct {
	ID      string
	Name    string
	Builtin bool
}

// ListSkillInfos returns all available skills (builtin + custom) for the CLI.
func ListSkillInfos() []SkillInfo {
	out := []SkillInfo{}
	for _, s := range listSkills() {
		out = append(out, SkillInfo{ID: s.ID, Name: s.Name, Builtin: s.Builtin})
	}
	return out
}

// CLIHarness configures the tool set of the `vortelio code` terminal agent.
type CLIHarness struct {
	WorkDir      string
	Mode         string // plan | ask | edits | auto
	Autonomous   bool
	MCP          bool
	Media        bool
	Emit         rt.ToolEventEmitter
	Approve      func(tool, summary, args string) bool
	Ask          func(question string, options []string) string
	Policy       PolicyFunc
	OnFileChange func(path string, before []byte, existed bool)
	State        *CodingState
	Ctx          context.Context
}

// NewCLIToolProvider builds the same agentic harness the Developer GUI uses
// (coding tools, web, media, MCP, skills authoring, ask_user) for the CLI.
func NewCLIToolProvider(h CLIHarness) rt.ToolProvider {
	return buildAgenticProvider(&AgenticConfig{
		Auto:         true,
		Autonomous:   h.Autonomous,
		WebSearch:    true,
		Builtins:     true,
		Coding:       true,
		Media:        h.Media,
		MCP:          h.MCP,
		Mode:         h.Mode,
		WorkingDir:   h.WorkDir,
		ApproveFunc:  h.Approve,
		AskFunc:      h.Ask,
		Policy:       h.Policy,
		OnFileChange: h.OnFileChange,
		State:        h.State,
		Ctx:          h.Ctx,
	}, h.Emit)
}

// ApplySkills appends the instructions of the enabled skills to a system prompt.
func ApplySkills(system string, ids []string) string { return applySkills(system, ids) }

// CLICloudTurn runs one turn of the CLI agent on a cloud model. prov nil = no tools.
func CLICloudTurn(ctx context.Context, providerID, model, system string, prov rt.ToolProvider, history []map[string]string, maxRounds, resultLimit int, onToken func(string), emit rt.ToolEventEmitter) (string, error) {
	p, ok := cloud.FindProvider(providerID)
	if !ok {
		return "", fmt.Errorf("unknown cloud provider: %s", providerID)
	}
	keys := cloud.KeysFor(providerID)
	if len(keys) == 0 {
		return "", fmt.Errorf("no API key saved for %s (add one with /model → Add provider, or: vortelio cloud)", p.Name)
	}
	p = cloud.ForModel(p, model)
	msgs := []cloud.Message{}
	if system != "" {
		msgs = append(msgs, cloud.Message{Role: "system", Content: system})
	}
	for _, m := range history {
		msgs = append(msgs, cloud.Message{Role: m["role"], Content: m["content"]})
	}
	var tools interface{}
	var exec func(string, string) (string, error)
	if prov != nil {
		tools, exec = prov.Tools(), prov.Execute
	}
	opts := &cloud.ToolCallOptions{Tools: tools, ExecTool: exec, OnEvent: emit, MaxRounds: maxRounds, Ctx: ctx, ResultLimit: resultLimit}
	if prov == nil {
		return cloud.ChatFailoverCtx(ctx, p, keys, msgs, onToken)
	}
	return cloud.ChatWithToolsFailover(p, keys, msgs, opts, onToken)
}

// buildAgenticProvider assembles a composite tool provider from the request's
// AgenticConfig. emit is the per-request tool event emitter (used for approvals).
func buildAgenticProvider(cfg *AgenticConfig, emit rt.ToolEventEmitter) rt.ToolProvider {
	var providers []rt.ToolProvider

	if cfg.WebSearch || cfg.Builtins {
		// With coding tools on, the builtin read/write/list duplicates must go:
		// they were listed first, so they shadowed the workspace-scoped, approval-
		// gated coding tools (plan/ask mode silently bypassed).
		providers = append(providers, &filteredBuiltins{web: cfg.WebSearch, rest: cfg.Builtins, noFiles: cfg.Coding})
	}
	if cfg.MCP {
		providers = append(providers, mcp.Default().Provider())
	}
	if cfg.Coding {
		providers = append(providers, newCodingProvider(cfg, emit))
	}
	if cfg.Media {
		providers = append(providers, newMediaProvider(emit))
	}
	// The agent can author its own skills and ask the user interactive questions.
	if len(providers) > 0 {
		providers = append(providers, &selfProvider{emit: emit, ask: cfg.AskFunc})
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "ask"
	}
	// Centralized approval gate: in Ask mode every state-changing/code-running
	// tool (run_code, media generation, install, rename, create_document) must be
	// confirmed; in Plan mode they are blocked. (write_file/edit_file/run_shell
	// are gated inside the coding provider itself, so they are not listed here to
	// avoid a double prompt.)
	return &gatedProvider{
		inner:   rt.NewCompositeProvider(providers...),
		mode:    mode,
		approve: cfg.ApproveFunc,
		policy:  cfg.Policy,
		emit:    emit,
	}
}

// gatedRiskyTools are tools that act on the system or run code and therefore need
// confirmation in Ask mode (and are blocked in Plan mode).
var gatedRiskyTools = map[string]bool{
	"run_code":        true,
	"create_document": true,
	"generate_image":  true,
	"generate_video":  true,
	"text_to_speech":  true,
	"generate_3d":     true,
	"install_model":   true,
	"rename_file":     true,
}

// gatedProvider wraps the full tool set and enforces Ask/Plan-mode approval for
// risky tools that the per-tool providers do not gate themselves.
type gatedProvider struct {
	inner   rt.ToolProvider
	mode    string
	approve func(tool, summary, args string) bool
	policy  PolicyFunc
	emit    rt.ToolEventEmitter
	mu      sync.Mutex
	counter int
}

func (g *gatedProvider) Tools() []rt.ToolDef { return g.inner.Tools() }

func (g *gatedProvider) Execute(name, args string) (string, error) {
	decision := ""
	if g.policy != nil && gatedRiskyTools[name] {
		decision = g.policy(name, args)
		if decision == PolicyDeny {
			return "", fmt.Errorf("denied by a permission rule for %s", name)
		}
	}
	if gatedRiskyTools[name] && decision != PolicyAllow {
		switch g.mode {
		case "plan":
			return "", fmt.Errorf("blocked: in Plan mode the agent cannot run code or generate/modify files. Switch to Ask or Auto to proceed")
		case "auto":
			// proceed without prompting
		default: // ask
			if !g.requestApproval(name, gatedSummary(name, args), args) {
				return "", fmt.Errorf("denied by user")
			}
		}
	}
	res, err := g.inner.Execute(name, args)
	// Surface a created document in the GUI like generated media (path + inline
	// download), otherwise the user is told "created" but never sees the file.
	if err == nil && name == "create_document" && g.emit != nil {
		g.emitDocument(res)
	}
	return res, err
}

// emitDocument reads the just-created document and emits a media_generated event
// so the GUI shows a clickable/downloadable card.
func (g *gatedProvider) emitDocument(result string) {
	var r struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Path == "" {
		return
	}
	data, err := os.ReadFile(r.Path)
	if err != nil || len(data) > 25*1024*1024 {
		// Still tell the UI where it is, even if too big to inline.
		g.emit("media_generated", map[string]interface{}{"kind": "document", "path": r.Path})
		return
	}
	mime := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(r.Path)) {
	case ".pdf":
		mime = "application/pdf"
	case ".docx":
		mime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".txt", ".md":
		mime = "text/plain"
	case ".html":
		mime = "text/html"
	case ".csv":
		mime = "text/csv"
	case ".json":
		mime = "application/json"
	}
	g.emit("media_generated", map[string]interface{}{
		"kind": "document", "path": r.Path, "mime": mime,
		"data_uri": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
	})
}

func gatedSummary(name, args string) string {
	var m map[string]interface{}
	json.Unmarshal([]byte(args), &m)
	clip := func(s string, n int) string {
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	}
	switch name {
	case "run_code":
		return "Esegui codice: " + clip(fmt.Sprint(m["code"]), 120)
	case "create_document":
		return fmt.Sprintf("Crea documento: %v", m["path"])
	case "install_model":
		return fmt.Sprintf("Installa modello: %v", m["model"])
	case "rename_file":
		return fmt.Sprintf("Rinomina file: %v", m["path"])
	case "generate_image", "generate_video", "generate_3d", "text_to_speech":
		return fmt.Sprintf("%s: %v", name, clip(fmt.Sprint(m["prompt"]), 100))
	}
	return name
}

func (g *gatedProvider) requestApproval(tool, summary, argsJSON string) bool {
	if g.approve != nil { // CLI synchronous y/n
		return g.approve(tool, summary, argsJSON)
	}
	g.mu.Lock()
	g.counter++
	id := fmt.Sprintf("gappr_%d_%d", time.Now().UnixNano(), g.counter)
	g.mu.Unlock()
	ch := registerApproval(id)
	if g.emit != nil {
		g.emit("approval_request", map[string]interface{}{
			"id": id, "tool": tool, "summary": summary, "arguments": json.RawMessage(argsJSON),
		})
	}
	select {
	case ok := <-ch:
		return ok
	case <-time.After(5 * time.Minute):
		resolveApproval(id, false)
		return false
	}
}

// isNoiseDir reports directories whose contents are build artefacts or VCS state
// and should be skipped by grep/glob so the model isn't fed binary garbage.
func isNoiseDir(name string) bool {
	switch name {
	case ".git", "__pycache__", "node_modules", ".venv", "venv", "dist", "build", ".idea", ".vscode", ".mypy_cache", ".pytest_cache", ".next", "target":
		return true
	}
	return false
}

// isBinaryName reports paths with a non-text extension that grep/glob should skip.
func isBinaryName(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pyc", ".pyo", ".exe", ".dll", ".so", ".dylib", ".o", ".a", ".class", ".jar",
		".zip", ".gz", ".tar", ".7z", ".rar", ".bin", ".db", ".sqlite", ".sqlite3",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".tiff",
		".mp4", ".mov", ".avi", ".mkv", ".webm", ".mp3", ".wav", ".flac", ".ogg",
		".pdf", ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return true
	}
	return false
}

// selfProvider lets the agent create reusable skills and ask the user questions
// with a graphical option picker.
type selfProvider struct {
	emit    rt.ToolEventEmitter
	ask     func(question string, options []string) string // CLI synchronous prompt; nil = GUI popup
	counter int
}

func (s *selfProvider) Tools() []rt.ToolDef {
	return []rt.ToolDef{
		{Type: "function", Function: rt.ToolFuncDef{
			Name:        "create_skill",
			Description: "Save a reusable skill to the user's skill library so it can be enabled in future sessions. Use when you find a repeatable procedure, style, or instruction set worth keeping.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Short skill name, e.g. 'React component author'"},"description":{"type":"string","description":"One-line summary of what the skill does"},"body":{"type":"string","description":"The full instructions the model should follow when this skill is active"}},"required":["name","body"]}`),
		}},
		{Type: "function", Function: rt.ToolFuncDef{
			Name:        "ask_user",
			Description: "Ask the user a question and let them choose from options in a graphical popup (with a free-text 'Other' field). Use when you need a decision or clarification before continuing. Returns the user's answer.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"The question to ask the user"},"options":{"type":"array","items":{"type":"string"},"description":"2-5 suggested answers shown as buttons"}},"required":["question"]}`),
		}},
	}
}

func (s *selfProvider) Execute(name, args string) (string, error) {
	switch name {
	case "create_skill":
		var a struct {
			Name, Description, Body string
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Body) == "" {
			return "", fmt.Errorf("name and body are required")
		}
		id, err := saveSkillContent("", a.Name, a.Description, a.Body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill \"%s\" created (id: %s).", a.Name, id), nil
	case "ask_user":
		var a struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(a.Question) == "" {
			return "", fmt.Errorf("question is required")
		}
		// CLI: synchronous terminal prompt.
		if s.ask != nil {
			return s.ask(a.Question, a.Options), nil
		}
		// GUI: emit a popup event and block until the user answers.
		s.counter++
		id := fmt.Sprintf("ask_%d_%d", time.Now().UnixNano(), s.counter)
		ch := registerAsk(id)
		if s.emit != nil {
			s.emit("ask_user", map[string]interface{}{"id": id, "question": a.Question, "options": a.Options})
		}
		select {
		case ans := <-ch:
			return "L'utente ha risposto: " + ans, nil
		case <-time.After(10 * time.Minute):
			resolveAsk(id, "")
			return "", fmt.Errorf("nessuna risposta dall'utente (timeout)")
		}
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

// filteredBuiltins exposes the builtin tools, optionally limited to web_search.
type filteredBuiltins struct {
	web     bool
	rest    bool
	noFiles bool // hide read_file/write_file/list_directory (coding tools provide them)
}

func (f *filteredBuiltins) Tools() []rt.ToolDef {
	var out []rt.ToolDef
	for _, t := range rt.BuiltinTools() {
		name := t.Function.Name
		if name == "web_search" {
			if f.web {
				out = append(out, t)
			}
			continue
		}
		if f.noFiles && (name == "read_file" || name == "write_file" || name == "list_directory") {
			continue
		}
		if f.rest {
			out = append(out, t)
		}
	}
	return out
}

func (f *filteredBuiltins) Execute(name, args string) (string, error) {
	return rt.ExecuteTool(name, args)
}
