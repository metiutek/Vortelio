package commands

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/internal/server"
	"golang.org/x/term"
)

// ANSI helpers
const (
	cReset = "\033[0m"
	cDim   = "\033[2m"
	cBold  = "\033[1m"
	cCyan  = "\033[36m"
	cGreen = "\033[32m"
	cYell  = "\033[33m"
	cRed   = "\033[31m"
	cMag   = "\033[35m"
	cBlue  = "\033[34m"
	cInv   = "\033[7m"
	// Highlighted bar for the submitted question so it stands out from output.
	cQBg    = "\033[48;5;24m" // dark teal background
	cQFg    = "\033[97m"      // bright white text
	cThinkC = "\033[38;5;213m"
)

// spinFrames is the braille spinner used by the live working animation.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// CodeCommand is the Vortelio terminal coding agent, on the same harness as the
// Developer GUI: agentic tool loop, coding tools, web, media, skills, MCP.
type CodeCommand struct{}

func NewCodeCommand() *CodeCommand  { return &CodeCommand{} }
func (c *CodeCommand) Name() string { return "code" }

type codeSession struct {
	model         *hub.Model
	runner        *runtime.LLMRunner
	hw            *runtime.Hardware
	workdir       string
	mode          string // plan | ask | auto
	autonomous    bool
	mcpOn         bool
	skills        []string
	messages      []map[string]interface{}
	cloudProvider string
	cloudModel    string
	history       []string // typed prompts, for ↑/↓ recall in the input box
}

var slashCmds = []struct{ Cmd, Desc string }{
	{"/model", "cambia modello (locali + cloud)"},
	{"/skills", "attiva/disattiva skill"},
	{"/mcp", "attiva/disattiva tool MCP"},
	{"/mode", "plan · ask · auto (conferma azioni)"},
	{"/auto", "modalità autonoma verso l'obiettivo"},
	{"/init", "genera/aggiorna PROJECT.md (riassunto del progetto)"},
	{"/cd", "cambia cartella di lavoro"},
	{"/clear", "azzera il contesto"},
	{"/help", "elenco comandi"},
	{"/exit", "esci"},
}

func (c *CodeCommand) Run(args []string) error {
	s := &codeSession{mode: "ask"}
	s.workdir, _ = os.Getwd()
	var modelRef string
	var firstPrompt []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			if i+1 < len(args) {
				modelRef = args[i+1]
				i++
			}
		case "--dir", "-d":
			if i+1 < len(args) {
				s.workdir = args[i+1]
				i++
			}
		case "--auto", "--autonomous":
			s.autonomous = true
			s.mode = "auto"
		case "--yes", "-y":
			s.mode = "auto"
		case "--cpu":
		case "--help", "-h":
			printCodeHelp()
			return nil
		default:
			if !strings.HasPrefix(args[i], "--") {
				firstPrompt = append(firstPrompt, args[i])
			}
		}
	}

	store := hub.NewModelStore()
	if modelRef != "" {
		ref, err := hub.ParseModelRef(modelRef)
		if err != nil {
			return fmt.Errorf("modello non valido: %w", err)
		}
		s.model, err = store.Resolve(ref)
		if err != nil {
			return fmt.Errorf("modello non trovato: %w", err)
		}
	} else if !s.restoreFromPrefs(store) {
		// No explicit model and nothing restorable from last session → default.
		s.model = pickDefaultLLM(store)
	}
	if s.model == nil && s.cloudProvider == "" {
		if cl := server.CloudModelsForCLI(); len(cl) > 0 {
			s.cloudProvider = cl[0].Provider
			s.cloudModel = cl[0].Model
		} else {
			return fmt.Errorf("nessun LLM installato e nessun cloud configurato.\n  vortelio pull llm/qwen2.5:7b")
		}
	}
	s.hw = runtime.DetectHardware()
	for _, a := range args {
		if a == "--cpu" {
			s.hw.Backend = runtime.BackendCPU
		}
	}

	s.printBanner()
	if s.cloudProvider == "" {
		if err := s.loadModel(); err != nil {
			return err
		}
	}

	if len(firstPrompt) > 0 {
		s.runTurn(strings.Join(firstPrompt, " "))
	}

	for {
		line, exit := s.readLine()
		if exit {
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if s.handleCommand(line) {
				return nil
			}
			continue
		}
		// Record for ↑/↓ history recall (skip consecutive duplicates).
		if n := len(s.history); n == 0 || s.history[n-1] != line {
			s.history = append(s.history, line)
		}
		s.runTurn(line)
	}
}

func (s *codeSession) loadModel() error {
	r, err := runtime.GlobalModelManager.GetOrLoad(s.model, s.hw, 30*time.Minute)
	if err != nil {
		return fmt.Errorf("caricamento modello fallito: %w", err)
	}
	s.runner = r
	return nil
}

func (s *codeSession) modelLabel() string {
	if s.cloudProvider != "" {
		return "☁ " + s.cloudModel
	}
	if s.model != nil {
		return s.model.Name + ":" + s.model.Tag
	}
	return "?"
}

func (s *codeSession) emit(ev string, data interface{}) {
	b, _ := json.Marshal(data)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	switch ev {
	case "tool_call":
		fmt.Printf("\n  %s⚙ %v%s %s%s%s\n", cCyan, m["name"], cReset, cDim, truncStr(fmt.Sprint(m["arguments"]), 140), cReset)
	case "tool_result":
		if e, ok := m["error"].(string); ok && e != "" {
			fmt.Printf("  %s✕ %s%s\n", cRed, truncStr(e, 200), cReset)
		} else {
			fmt.Printf("  %s✓%s %s%s%s\n", cGreen, cReset, cDim, truncStr(fmt.Sprint(m["result"]), 200), cReset)
		}
	case "media_generated":
		fmt.Printf("  %s🎨 %v%s\n", cMag, m["path"], cReset)
	}
}

// approve is the synchronous terminal approval for "ask" mode.
func (s *codeSession) approve(tool, summary, args string) bool {
	// Once the user picked "a" (auto) — or the session is autonomous — stop
	// prompting: the gate keeps calling approve with its build-time mode, so this
	// is what actually silences further confirmations for the rest of the session.
	if s.mode == "auto" || s.autonomous {
		return true
	}
	fmt.Printf("\n  %s⚠ Conferma azione%s  %s%s%s\n", cYell, cReset, cBold, summary, cReset)
	fmt.Printf("  %s%s%s\n", cDim, truncStr(args, 200), cReset)
	fmt.Printf("  [%sy%s] sì   [%sn%s] no   [%sa%s] sì a tutto (auto)  ", cGreen, cReset, cRed, cReset, cCyan, cReset)
	in := promptLineRaw("")
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "y", "yes", "s", "si", "":
		return true
	case "a", "all":
		s.mode = "auto"
		fmt.Printf("  %smodalità auto attivata%s\n", cDim, cReset)
		return true
	default:
		return false
	}
}

// askUser is the terminal answer for the ask_user tool. With options it shows an
// arrow-selectable list (plus a "write your own" entry); without options (free
// text) it reads a line. Input is read in raw mode so it works even mid tool-loop.
func (s *codeSession) askUser(question string, options []string) string {
	fmt.Printf("\n  %s❓ %s%s\n", cYell, question, cReset)
	if len(options) > 0 {
		items := make([]string, 0, len(options)+1)
		items = append(items, options...)
		items = append(items, "✏️  Scrivi una risposta…")
		sel := selectList("↑↓ scegli · Invio conferma:", items, 0)
		if sel >= 0 && sel < len(options) {
			return options[sel]
		}
		// "write your own" or cancelled → fall through to free text
	}
	return promptLineRaw("   ✏️  Risposta: ")
}

// liveStatus renders the working animation: a spinner with a live phase label
// (thinking · writing · running a tool), elapsed time and a running token count.
// It shares stdout with the streamed answer, so content printing and the ticker
// are serialised through mu; the status line is only drawn at a line boundary and
// is erased the instant new content arrives, so it never corrupts the output.
type liveStatus struct {
	mu          sync.Mutex
	start       time.Time
	bytes       int
	phase       string // pensa | scrive | tool | avvio
	spin        int
	atLineStart bool
	shown       bool
	tty         bool
	done        bool
	resp        *strings.Builder
}

func (l *liveStatus) tokens() int { return l.bytes/4 + 1 }

func (l *liveStatus) phaseLabel() string {
	switch l.phase {
	case "pensa":
		return cThinkC + "🧠 sta pensando…" + cReset
	case "scrive":
		return cGreen + "✍  scrivo la risposta…" + cReset
	case "tool":
		return cCyan + "⚙  eseguo strumento…" + cReset
	default:
		return cCyan + "⏳ elaboro…" + cReset
	}
}

// draw paints the status line in place. Caller holds mu.
func (l *liveStatus) draw() {
	if !l.tty || l.done || !l.atLineStart {
		return
	}
	frame := spinFrames[l.spin%len(spinFrames)]
	l.spin++
	secs := time.Since(l.start).Seconds()
	fmt.Printf("\r\033[K  %s%s%s %s  %s· %.1fs · %d token%s",
		cCyan, frame, cReset, l.phaseLabel(), cDim, secs, l.tokens(), cReset)
	l.shown = true
}

// erase clears the status line if present. Caller holds mu.
func (l *liveStatus) erase() {
	if l.shown {
		fmt.Print("\r\033[K")
		l.shown = false
	}
}

// content streams a visible answer token.
func (l *liveStatus) content(tok string) {
	l.mu.Lock()
	l.erase()
	fmt.Print(tok)
	if l.resp != nil {
		l.resp.WriteString(tok)
	}
	l.bytes += len(tok)
	l.phase = "scrive"
	l.atLineStart = strings.HasSuffix(tok, "\n")
	l.mu.Unlock()
}

// think receives reasoning tokens: not printed, only surfaced as the "thinking"
// state so the user sees the model is reasoning without flooding the answer.
func (l *liveStatus) think(tok string) {
	l.mu.Lock()
	l.phase = "pensa"
	l.bytes += len(tok)
	l.mu.Unlock()
}

func (s *codeSession) runTurn(line string) {
	line = s.expandFileRefs(line)
	s.messages = append(s.messages, map[string]interface{}{"role": "user", "content": line})
	fmt.Print("\n")

	var resp strings.Builder
	l := &liveStatus{
		start:       time.Now(),
		phase:       "avvio",
		atLineStart: true,
		tty:         term.IsTerminal(int(os.Stdout.Fd())),
		resp:        &resp,
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				l.mu.Lock()
				l.draw()
				l.mu.Unlock()
			}
		}
	}()

	// Tool events must not collide with the ticker: erase the status, let emit
	// print its own line(s), then mark the cursor back at a line start.
	wrappedEmit := func(ev string, data interface{}) {
		l.mu.Lock()
		l.erase()
		l.phase = "tool"
		l.mu.Unlock()
		s.emit(ev, data)
		l.mu.Lock()
		l.atLineStart = true
		l.mu.Unlock()
	}

	var err error
	if s.cloudProvider != "" {
		var hist []map[string]string
		for _, m := range s.messages {
			hist = append(hist, map[string]string{"role": fmt.Sprint(m["role"]), "content": fmt.Sprint(m["content"])})
		}
		_, err = server.RunCLICloudTurn(s.cloudProvider, s.cloudModel, s.workdir, s.mode, s.autonomous, s.mcpOn, s.skills, hist, l.content, wrappedEmit, s.approve, s.askUser)
	} else {
		prov, sys := server.BuildCLIHarness(s.workdir, s.mode, s.autonomous, s.mcpOn, s.skills, wrappedEmit, s.approve, s.askUser)
		sopts := runtime.StreamOpts{System: sys, Messages: s.messages, ToolsEnabled: true, ToolProvider: prov}
		sopts.ThinkEmit = l.think
		// Raise the per-round token cap: the 512 default truncates any real code
		// generation (a file the model writes streams as tool arguments here too).
		sopts.Options.MaxTokens = 4096
		if s.autonomous {
			sopts.MaxToolRounds = 40
		} else {
			sopts.MaxToolRounds = 16
		}
		err = s.runner.StreamWithOpts(sopts, l.content, wrappedEmit)
	}

	// Tear down the ticker and clear any lingering status line.
	close(stop)
	l.mu.Lock()
	l.done = true
	l.erase()
	l.mu.Unlock()

	fmt.Print("\n")
	if err != nil {
		fmt.Printf("%s✕ errore: %v%s\n", cRed, err, cReset)
		return
	}
	secs := time.Since(l.start).Seconds()
	fmt.Printf("%s⏱ %.1fs · ~%d token · %s%s\n", cDim, secs, l.tokens(), s.modelLabel(), cReset)
	s.messages = append(s.messages, map[string]interface{}{"role": "assistant", "content": resp.String()})
}

// projectSummaryFile is the canonical project-summary document /init maintains.
const projectSummaryFile = "PROJECT.md"

// runInit generates (or updates) PROJECT.md: a concise, always-current summary of
// the project. It drives the same agentic harness autonomously so the model reads
// the real files and writes the document itself.
func (s *codeSession) runInit() {
	target := filepath.Join(s.workdir, projectSummaryFile)
	_, statErr := os.Stat(target)
	exists := statErr == nil

	if exists {
		fmt.Printf("\n  %s⟳ Aggiorno %s…%s\n", cCyan, projectSummaryFile, cReset)
	} else {
		fmt.Printf("\n  %s✚ Genero %s…%s\n", cCyan, projectSummaryFile, cReset)
	}

	var sb strings.Builder
	if exists {
		sb.WriteString("Aggiorna il file " + projectSummaryFile + " nella cartella di lavoro. ")
		sb.WriteString("PRIMA leggilo con read_file, poi confronta con lo stato reale del progetto e aggiorna SOLO le parti " +
			"diventate obsolete o mancanti, preservando le note scritte a mano. ")
	} else {
		sb.WriteString("Crea il file " + projectSummaryFile + " nella cartella di lavoro: un riassunto chiaro e sintetico del progetto. ")
	}
	sb.WriteString("Esplora prima il progetto con gli strumenti (list_directory, read_file, glob, grep): " +
		"leggi i file chiave reali (README, manifest delle dipendenze come go.mod/package.json/requirements.txt/pyproject.toml, " +
		"entrypoint principali, file di configurazione). NON inventare: descrivi solo ciò che esiste davvero. ")
	sb.WriteString("Il documento deve essere in Markdown, conciso e ben strutturato, con queste sezioni:\n" +
		"1. Nome e scopo del progetto (1-2 frasi)\n" +
		"2. Stack e tecnologie principali\n" +
		"3. Struttura delle cartelle (le directory principali e a cosa servono)\n" +
		"4. Componenti / moduli chiave (file principali e loro responsabilità)\n" +
		"5. Come si installa, avvia, builda e testa (comandi reali)\n" +
		"6. Configurazione ed env necessari (variabili, chiavi, file di config)\n" +
		"7. Note di architettura / flusso principale\n" +
		"8. TODO o punti aperti noti (se rilevabili)\n")
	sb.WriteString("Inizia il file con una riga di commento HTML: <!-- Generato/aggiornato da `vortelio code /init`. Modificabile a mano. -->\n")
	sb.WriteString("Alla fine SCRIVI davvero il file con write_file e conferma il percorso completo. " +
		"Termina con una frase che riassume cosa hai scritto o aggiornato.")

	// /init always runs autonomously for this turn so the tools actually execute
	// without per-step confirmation, then the previous mode is restored.
	prevAuto, prevMode := s.autonomous, s.mode
	s.autonomous, s.mode = true, "auto"
	s.runTurn(sb.String())
	s.autonomous, s.mode = prevAuto, prevMode
}

var fileRefRE = regexp.MustCompile(`@([^\s"']+)`)

func (s *codeSession) expandFileRefs(line string) string {
	var extras []string
	out := fileRefRE.ReplaceAllStringFunc(line, func(tok string) string {
		p := tok[1:]
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(s.workdir, p)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return tok
		}
		if len(data) > 40000 {
			data = data[:40000]
		}
		extras = append(extras, fmt.Sprintf("\n\n--- File \"%s\" ---\n%s", p, string(data)))
		fmt.Printf("  %s📎 incluso %s (%d byte)%s\n", cDim, p, len(data), cReset)
		return p
	})
	return out + strings.Join(extras, "")
}

func (s *codeSession) handleCommand(line string) bool {
	parts := strings.Fields(line)
	switch parts[0] {
	case "/exit", "/quit", "/q":
		return true
	case "/help", "/?":
		printSlashHelp()
	case "/clear":
		s.messages = nil
		fmt.Printf("  %scontesto azzerato%s\n", cDim, cReset)
	case "/auto":
		s.autonomous = !s.autonomous
		if s.autonomous {
			s.mode = "auto"
		}
		fmt.Printf("  %sautonomo: %v%s\n", cYell, s.autonomous, cReset)
		s.savePrefs()
	case "/mode":
		if len(parts) > 1 && (parts[1] == "plan" || parts[1] == "ask" || parts[1] == "auto") {
			s.mode = parts[1]
		} else {
			fmt.Printf("  %smode attuale: %s — usa /mode plan|ask|auto%s\n", cDim, s.mode, cReset)
			return false
		}
		s.autonomous = s.mode == "auto"
		fmt.Printf("  %smode: %s%s\n", cYell, s.mode, cReset)
		s.savePrefs()
	case "/mcp":
		s.mcpOn = !s.mcpOn
		fmt.Printf("  %sMCP: %v%s\n", cYell, s.mcpOn, cReset)
	case "/cd":
		if len(parts) > 1 {
			s.workdir = strings.TrimSpace(line[len("/cd "):])
			fmt.Printf("  %scartella: %s%s\n", cCyan, s.workdir, cReset)
		}
	case "/init":
		s.runInit()
	case "/model", "/m":
		s.chooseModel()
	case "/skills", "/skill":
		s.chooseSkills()
	default:
		fmt.Printf("  %scomando sconosciuto: %s — /help%s\n", cDim, parts[0], cReset)
	}
	return false
}

func (s *codeSession) chooseModel() {
	models, _ := hub.NewModelStore().List()
	var llms []*hub.Model
	for _, m := range models {
		if m.Type == "llm" {
			llms = append(llms, m)
		}
	}
	cloud := server.CloudModelsForCLI()
	if len(llms) == 0 && len(cloud) == 0 {
		fmt.Printf("  %snessun modello%s\n", cDim, cReset)
		return
	}

	var items []string
	start := 0
	for _, m := range llms {
		mark := "  "
		if s.cloudProvider == "" && s.model != nil && m.Name == s.model.Name && m.Tag == s.model.Tag {
			mark = "● "
			start = len(items)
		}
		tl := ""
		if runtime.ModelSupportsTools(m.Name + ":" + m.Tag) {
			tl = " 🛠"
		}
		items = append(items, "💻 "+mark+m.Name+":"+m.Tag+tl)
	}
	for _, c := range cloud {
		mark := "  "
		if s.cloudProvider == c.Provider && s.cloudModel == c.Model {
			mark = "● "
			start = len(items)
		}
		items = append(items, "☁ "+mark+c.Label+" · "+c.ProviderName)
	}
	sel := selectList("Scegli un modello:", items, start)
	if sel < 0 {
		return
	}
	if sel < len(llms) {
		s.cloudProvider = ""
		s.cloudModel = ""
		s.model = llms[sel]
		fmt.Printf("  %s⏳ carico…%s\n", cDim, cReset)
		if err := s.loadModel(); err != nil {
			fmt.Printf("  %s%v%s\n", cRed, err, cReset)
		} else {
			fmt.Printf("  %s✓ %s:%s%s\n", cGreen, s.model.Name, s.model.Tag, cReset)
		}
	} else {
		c := cloud[sel-len(llms)]
		s.cloudProvider = c.Provider
		s.cloudModel = c.Model
		fmt.Printf("  %s✓ ☁ %s%s\n", cGreen, c.Label, cReset)
	}
	s.savePrefs()
}

func (s *codeSession) chooseSkills() {
	all := server.ListSkillInfos()
	if len(all) == 0 {
		fmt.Printf("  %snessuna skill%s\n", cDim, cReset)
		return
	}
	for {
		on := map[string]bool{}
		for _, id := range s.skills {
			on[id] = true
		}
		var items []string
		for _, sk := range all {
			box := "[ ] "
			if on[sk.ID] {
				box = "[x] "
			}
			items = append(items, box+sk.Name)
		}
		sel := selectList("Skill (Invio per attivare/disattivare · q per chiudere):", items, 0)
		if sel < 0 {
			return
		}
		id := all[sel].ID
		if on[id] {
			var ns []string
			for _, x := range s.skills {
				if x != id {
					ns = append(ns, x)
				}
			}
			s.skills = ns
		} else {
			s.skills = append(s.skills, id)
		}
	}
}

// ── Rich banner ─────────────────────────────────────────────────────
func (s *codeSession) printBanner() {
	branch, clean := gitInfo(s.workdir)
	files := countFiles(s.workdir)
	fmt.Printf("\n %s%s🤖 Vortelio Code%s\n", cBold, cCyan, cReset)
	if branch != "" {
		st := cGreen + "clean" + cReset
		if !clean {
			st = cYell + "modificato" + cReset
		}
		fmt.Printf("   %s📂 Git:%s %s (%s)\n", cDim, cReset, branch, st)
	} else {
		fmt.Printf("   %s📂 Cartella:%s %s\n", cDim, cReset, s.workdir)
	}
	fmt.Printf("   %s🗂  Progetto:%s %d file indicizzati\n", cDim, cReset, files)
	fmt.Printf("   %s🧠 Modello:%s %s   %smode:%s %s\n", cDim, cReset, s.modelLabel(), cDim, cReset, s.mode)
	fmt.Printf("\n   %sScrivi un obiettivo. %s/%s comandi · %s@%s file · %s/help%s%s\n\n", cDim, cReset, cDim, cReset, cDim, cReset, cDim, cReset)
}

func gitInfo(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(string(out))
	st, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	return branch, strings.TrimSpace(string(st)) == ""
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".venv" || name == "__pycache__" || name == "dist" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		n++
		if n > 9999 {
			return filepath.SkipAll
		}
		return nil
	})
	return n
}

func printSlashHelp() {
	fmt.Printf("\n  %sComandi:%s\n", cBold, cReset)
	for _, c := range slashCmds {
		fmt.Printf("   %s%-8s%s %s%s%s\n", cCyan, c.Cmd, cReset, cDim, c.Desc, cReset)
	}
	fmt.Printf("  %s@percorso/file → include il contenuto del file%s\n", cDim, cReset)
}

func pickDefaultLLM(store *hub.ModelStore) *hub.Model {
	models, err := store.List()
	if err != nil {
		return nil
	}
	var firstLLM *hub.Model
	for _, m := range models {
		if m.Type != "llm" {
			continue
		}
		if firstLLM == nil {
			firstLLM = m
		}
		if runtime.ModelSupportsTools(m.Name + ":" + m.Tag) {
			return m
		}
	}
	return firstLLM
}

func truncStr(s string, max int) string {
	// Strip control chars (esp. \r) so printed tool output can't move the cursor
	// to column 0 and overwrite previous lines (garbled terminal rendering).
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func printCodeHelp() {
	fmt.Println("vortelio code — agente coding nel terminale (stesso motore del Developer GUI)")
	fmt.Println("")
	fmt.Println("Uso:  vortelio code [obiettivo] [flag]")
	fmt.Println("")
	fmt.Println("Flag:")
	fmt.Println("  -m, --model <ref>   Modello (default: primo LLM tool-capable; poi cloud)")
	fmt.Println("  -d, --dir <path>    Cartella di lavoro")
	fmt.Println("      --auto / -y     Esegue le azioni senza chiedere conferma")
	fmt.Println("      --cpu           Forza CPU")
	fmt.Println("")
	fmt.Println("In chat:  /model /skills /mcp /mode /auto /init /cd /clear /help /exit  ·  @file")
}
