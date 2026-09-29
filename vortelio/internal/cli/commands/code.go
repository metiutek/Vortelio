package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/vortelio/vortelio/internal/cloud"
	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/internal/server"
	"github.com/vortelio/vortelio/internal/version"
)

// CodeCommand is `vortelio code`: an interactive coding agent in the terminal,
// modeled on Claude Code / Codex / OpenCode. It runs local models (llama.cpp)
// or cloud models with the same tool harness as the Developer GUI.
type CodeCommand struct{}

func NewCodeCommand() *CodeCommand  { return &CodeCommand{} }
func (c *CodeCommand) Name() string { return "code" }

const (
	defaultContextTokens = 16384
	cloudContextTokens   = 128000
	defaultMaxTurns      = 30
)

type codeOpts struct {
	model        string
	dir          string
	mode         string
	print        bool
	outputFormat string
	prompt       string
	cont         bool
	resume       string
	resumePick   bool
	cpu          bool
	maxTurns     int
}

type fileCheckpoint struct {
	path    string
	before  []byte
	existed bool
}

type codeSession struct {
	t        *terminal
	ui       *codeUI
	opts     codeOpts
	settings codeSettings
	warns    []string

	workdir string
	mode    string

	local         *hub.Model
	runner        *runtime.LLMRunner
	hw            *runtime.Hardware
	cloudProvider string
	cloudModel    string

	messages       []chatMsg
	summary        string // compacted earlier conversation
	pendingContext string // output of !commands, prepended to the next prompt
	sess           *savedSession
	state          *server.CodingState
	skills         []string
	mcpOn          bool
	media          bool
	showThinking   bool

	instr      []instrFile
	customCmds map[string]customCommand
	history    []string

	undo      [][]fileCheckpoint
	turnCPs   []fileCheckpoint
	turnSeen  map[string]bool
	cancel    context.CancelFunc
	cancelled bool
	sessAllow []string // "always allow" rules added during this session (not yet saved)

	toolsChars int // size of the tool definitions (context accounting)
	lastUsage  int
	mu         sync.Mutex
}

// ── entry point ──────────────────────────────────────────────────────────────

func (c *CodeCommand) Run(args []string) error {
	opts, err := parseCodeArgs(args)
	if err != nil {
		return err
	}
	if opts == nil {
		return nil // --help
	}
	s := &codeSession{opts: *opts, state: server.NewCodingState(), turnSeen: map[string]bool{}}
	s.workdir = opts.dir
	if s.workdir == "" {
		s.workdir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(s.workdir); err == nil {
		s.workdir = abs
	}
	if fi, err := os.Stat(s.workdir); err != nil || !fi.IsDir() {
		return fmt.Errorf("working directory not found: %s", s.workdir)
	}
	s.settings, s.warns = effectiveSettings(s.workdir)
	s.mode = s.settings.Mode
	if opts.mode != "" {
		s.mode = opts.mode
	}
	s.showThinking = boolOr(s.settings.ShowThinking, false)
	s.mcpOn = boolOr(s.settings.MCP, false)
	s.media = boolOr(s.settings.MediaTools, false)
	s.instr = loadInstructions(s.workdir)
	s.customCmds = loadCustomCommands(s.workdir)
	s.hw = runtime.DetectHardware()
	if opts.cpu {
		s.hw.Backend = runtime.BackendCPU
	}
	// The local llama-server belongs to this process: stop it on exit so it does
	// not linger holding RAM/VRAM.
	defer runtime.GlobalModelManager.UnloadAll()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			runtime.GlobalModelManager.UnloadAll()
			os.Exit(130)
		}
	}()

	if opts.print {
		return s.runPrint()
	}
	return s.runInteractive()
}

func parseCodeArgs(args []string) (*codeOpts, error) {
	o := &codeOpts{outputFormat: "text"}
	var words []string
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-h", "--help":
			printCodeHelp()
			return nil, nil
		case "-p", "--print":
			o.print = true
		case "-m", "--model":
			v, err := need(i, a)
			if err != nil {
				return nil, err
			}
			o.model, i = v, i+1
		case "-d", "--dir", "-C", "--cd":
			v, err := need(i, a)
			if err != nil {
				return nil, err
			}
			o.dir, i = v, i+1
		case "--mode", "--permission-mode":
			v, err := need(i, a)
			if err != nil {
				return nil, err
			}
			if !validMode(v) {
				return nil, fmt.Errorf("unknown mode %q (plan, ask, edits, auto)", v)
			}
			o.mode, i = v, i+1
		case "-y", "--yes", "--auto", "--autonomous", "--dangerously-skip-permissions":
			o.mode = "auto"
		case "--plan":
			o.mode = "plan"
		case "--output-format":
			v, err := need(i, a)
			if err != nil {
				return nil, err
			}
			if v != "text" && v != "json" {
				return nil, fmt.Errorf("--output-format must be text or json")
			}
			o.outputFormat, i = v, i+1
		case "-c", "--continue":
			o.cont = true
		case "-r", "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && looksLikeSessionID(args[i+1]) {
				o.resume, i = args[i+1], i+1
			} else {
				o.resumePick = true
			}
		case "--cpu":
			o.cpu = true
		case "--max-turns":
			v, err := need(i, a)
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("--max-turns must be a positive number")
			}
			o.maxTurns, i = n, i+1
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return nil, fmt.Errorf("unknown flag %s (see vortelio code --help)", a)
			}
			words = append(words, a)
		}
	}
	o.prompt = strings.Join(words, " ")
	return o, nil
}

var sessionIDRE = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{8}$`)

func looksLikeSessionID(s string) bool { return sessionIDRE.MatchString(s) }

func printCodeHelp() {
	fmt.Print(`vortelio code — AI coding agent in your terminal

Usage:
  vortelio code [prompt]              interactive session (optional first prompt)
  vortelio code -p "prompt"           run once, print the answer, exit (scripts/CI)
  echo "prompt" | vortelio code -p    prompt from stdin

Options:
  -m, --model <ref>          model: local ("qwen3.5:4b") or cloud ("cloud/ollamacloud/gpt-oss:120b")
  -d, -C, --dir <path>       working directory (default: current)
      --mode <mode>          permission mode: plan | ask | edits | auto (default: ask)
      --plan                 start in plan mode (read-only)
  -y, --yes, --auto          auto mode: run edits and commands without asking
  -c, --continue             continue the most recent session in this directory
  -r, --resume [id]          resume a session (picker if no id)
      --max-turns <n>        max tool rounds per request (default 30)
      --output-format <fmt>  with -p: text (default) or json
      --cpu                  run the local model on CPU

Permission modes:
  plan   read-only: explores and proposes a plan, never edits or runs commands
  ask    asks before every file edit and command (default)
  edits  file edits run freely, commands still ask
  auto   everything runs without asking (deny rules still apply)

In the session: /help for commands, ? for shortcuts, @file to attach a file,
!cmd to run a shell command, # note to save a note to AGENTS.md, Esc to interrupt.

Files:
  AGENTS.md / CLAUDE.md / VORTELIO.md   project instructions (loaded automatically; /init creates AGENTS.md)
  ~/.vortelio/code_settings.json        global settings (model, mode, permissions…)
  .vortelio/settings.json               project settings
  .vortelio/commands/*.md               custom /commands ($ARGUMENTS = text after the command)
`)
}

// ── interactive session ──────────────────────────────────────────────────────

func (s *codeSession) runInteractive() error {
	s.t = openTerminal()
	defer s.t.Close()
	s.ui = newCodeUI(os.Stdout, os.Stdout, s.t.tty && os.Getenv("NO_COLOR") == "", s.showThinking)
	s.history = loadPromptHistory()

	if err := s.pickInitialModel(); err != nil {
		return err
	}
	if err := s.initSession(); err != nil {
		return err
	}
	if b, err := json.Marshal(s.newProvider(context.Background()).Tools()); err == nil {
		s.toolsChars = len(b)
	}
	s.printBanner()
	if s.local != nil {
		if err := s.loadLocal(); err != nil {
			s.ui.println("%s✗ %v%s", cRed, err, cReset)
		}
	}

	ed := &lineEditor{t: s.t, history: s.history}
	ed.status = s.footer
	ed.complete = s.completions
	ed.cycleMode = func() { s.setMode(nextMode(s.mode), false) }
	ed.toggleThk = func() string {
		s.showThinking = !s.showThinking
		s.ui.showThinking = s.showThinking
		if s.showThinking {
			return "reasoning shown"
		}
		return "reasoning hidden"
	}

	if s.opts.prompt != "" {
		fmt.Printf("%s> %s%s\n", cGray, s.opts.prompt, cReset)
		ed.remember(s.opts.prompt)
		s.handleInput(s.opts.prompt)
	}
	for {
		fmt.Print("\n")
		line, res := ed.read()
		if res == edExit {
			s.goodbye()
			return nil
		}
		appendPromptHistory(line)
		if s.handleInput(line) {
			s.goodbye()
			return nil
		}
	}
}

func (s *codeSession) goodbye() {
	s.saveSession()
	if s.sess != nil && len(s.sess.Messages) > 0 {
		fmt.Printf("\n%sResume this session with: vortelio code --resume %s%s\n", cGray, s.sess.ID, cReset)
	}
}

// handleInput dispatches one submitted prompt. Returns true to exit.
func (s *codeSession) handleInput(line string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return false
	case strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "//"):
		return s.handleCommand(trimmed)
	case strings.HasPrefix(trimmed, "!"):
		s.runBang(strings.TrimSpace(trimmed[1:]))
		return false
	case strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "##"):
		s.addMemory(strings.TrimSpace(trimmed[1:]))
		return false
	}
	s.runTurn(line)
	return false
}

// ── models ───────────────────────────────────────────────────────────────────

// parseCloudRef accepts "cloud/<provider>/<model>" or "<provider>/<model>".
func parseCloudRef(ref string) (string, string, bool) {
	r := strings.TrimPrefix(ref, "cloud/")
	i := strings.IndexByte(r, '/')
	if i <= 0 {
		return "", "", false
	}
	prov, model := r[:i], r[i+1:]
	if _, ok := cloud.FindProvider(prov); !ok {
		return "", "", false
	}
	return prov, model, model != ""
}

func (s *codeSession) setModelRef(ref string) error {
	if prov, model, ok := parseCloudRef(ref); ok {
		if !cloud.Configured(prov) {
			p, _ := cloud.FindProvider(prov)
			return fmt.Errorf("no API key for %s — add one with /model → Add provider / API key", p.Name)
		}
		s.cloudProvider, s.cloudModel = prov, model
		s.local, s.runner = nil, nil
		return nil
	}
	full := ref
	switch strings.SplitN(ref, "/", 2)[0] {
	case "llm", "image", "audio", "video", "3d":
	default:
		full = "llm/" + ref // "qwen3.5:4b", "hf.co/owner/repo:file"
	}
	r, err := hub.ParseModelRef(full)
	if err != nil {
		return fmt.Errorf("invalid model %q: %w", ref, err)
	}
	m, err := hub.NewModelStore().Resolve(r)
	if err != nil {
		return fmt.Errorf("model %q is not installed (vortelio pull %s)", ref, ref)
	}
	if m.Type != "llm" {
		return fmt.Errorf("%s is a %s model; vortelio code needs a language model", ref, m.Type)
	}
	s.local = m
	s.runner = nil
	s.cloudProvider, s.cloudModel = "", ""
	return nil
}

func (s *codeSession) modelRef() string {
	if s.cloudProvider != "" {
		return "cloud/" + s.cloudProvider + "/" + s.cloudModel
	}
	if s.local != nil {
		return s.local.Name + ":" + s.local.Tag
	}
	return ""
}

func (s *codeSession) modelLabel() string {
	if s.cloudProvider != "" {
		p, _ := cloud.FindProvider(s.cloudProvider)
		return s.cloudModel + " (" + p.Name + ")"
	}
	if s.local != nil {
		return s.local.Name + ":" + s.local.Tag
	}
	return "no model"
}

func (s *codeSession) pickInitialModel() error {
	candidates := []string{s.opts.model, s.settings.Model}
	for i, ref := range candidates {
		if ref == "" {
			continue
		}
		if err := s.setModelRef(ref); err != nil {
			if i == 0 {
				return err // explicit --model must work
			}
			s.warns = append(s.warns, "saved model unavailable: "+err.Error())
			continue
		}
		return nil
	}
	if m := pickDefaultLLM(hub.NewModelStore()); m != nil {
		s.local = m
		return nil
	}
	if cl := server.CloudModelsForCLI(); len(cl) > 0 {
		s.cloudProvider, s.cloudModel = cl[0].Provider, cl[0].Model
		return nil
	}
	// Only keys for providers without a free plan: use that provider's default.
	for _, p := range cloud.AllProviders() {
		if cloud.Configured(p.ID) && p.DefaultModel != "" {
			s.cloudProvider, s.cloudModel = p.ID, p.DefaultModel
			return nil
		}
	}
	return errors.New("no language model installed and no cloud API key configured.\n  Install one:  vortelio pull qwen3.5:4b\n  or add a cloud key in the web UI (vortelio gui → Settings → Add cloud model)")
}

func pickDefaultLLM(store *hub.ModelStore) *hub.Model {
	models, err := store.List()
	if err != nil {
		return nil
	}
	var first *hub.Model
	for _, m := range models {
		if m.Type != "llm" {
			continue
		}
		if first == nil {
			first = m
		}
		if runtime.ModelSupportsTools(m.Name + ":" + m.Tag) {
			return m
		}
	}
	return first
}

func (s *codeSession) contextLimit() int {
	if s.cloudProvider != "" {
		return cloudContextTokens
	}
	if s.settings.ContextTokens > 0 {
		return s.settings.ContextTokens
	}
	return defaultContextTokens
}

func (s *codeSession) loadLocal() error {
	if s.local == nil {
		return nil
	}
	stop := s.spin("Loading " + s.modelLabel())
	// The runtime prints its own progress lines; the spinner replaces them in
	// the interactive UI (they would break the in-place redraw).
	var restore func()
	if s.t != nil && s.t.tty {
		if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			orig := os.Stdout
			os.Stdout = null
			restore = func() { os.Stdout = orig; null.Close() }
		}
	}
	r, err := runtime.GlobalModelManager.GetOrLoadWithContext(s.local, s.hw, 30*time.Minute, s.contextLimit())
	if restore != nil {
		restore()
	}
	stop()
	if err != nil {
		return fmt.Errorf("could not load %s: %w", s.modelLabel(), err)
	}
	s.runner = r
	if !runtime.ModelSupportsTools(s.local.Name + ":" + s.local.Tag) {
		s.ui.println("%s⚠ %s may not support tool calling; file edits and commands need a tool-capable model (Qwen 3.x, Gemma 4, gpt-oss, Llama 3.1+…).%s", cYell, s.modelLabel(), cReset)
	}
	return nil
}

// spin shows a one-line spinner until the returned stop func is called.
func (s *codeSession) spin(label string) func() {
	if s.t == nil || !s.t.tty {
		return func() {}
	}
	done := make(chan struct{})
	w := os.Stdout // captured: os.Stdout may be silenced while the spinner runs
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		for i := 0; ; i++ {
			fmt.Fprintf(w, "\r\033[K%s%s %s… %s(%ds)%s", cAccent, spinFrames[i%len(spinFrames)], label, cGray, int(time.Since(start).Seconds()), cReset)
			select {
			case <-done:
				fmt.Fprint(w, "\r\033[K")
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// ── sessions ─────────────────────────────────────────────────────────────────

func (s *codeSession) initSession() error {
	var loaded *savedSession
	switch {
	case s.opts.resume != "":
		ss, err := loadSession(s.opts.resume)
		if err != nil {
			return err
		}
		loaded = ss
	case s.opts.cont:
		if list := listSessions(s.workdir); len(list) > 0 {
			loaded = list[0]
		} else {
			s.warns = append(s.warns, "no previous session in this directory; starting a new one")
		}
	case s.opts.resumePick:
		loaded = s.pickSession()
	}
	if loaded != nil {
		s.adoptSession(loaded)
		return nil
	}
	s.sess = &savedSession{ID: newSessionID(), Dir: s.workdir, Created: time.Now()}
	return nil
}

func (s *codeSession) adoptSession(ss *savedSession) {
	s.sess = ss
	s.messages = nil
	s.summary = ""
	for _, m := range ss.Messages {
		if m.Role == "summary" {
			s.summary = m.Content
			continue
		}
		s.messages = append(s.messages, m)
	}
	if ss.Dir != "" && !samePath(ss.Dir, s.workdir) {
		if fi, err := os.Stat(ss.Dir); err == nil && fi.IsDir() {
			s.workdir = ss.Dir
			s.instr = loadInstructions(s.workdir)
			s.customCmds = loadCustomCommands(s.workdir)
		}
	}
	if s.opts.model == "" && ss.Model != "" {
		if err := s.setModelRef(ss.Model); err != nil {
			s.warns = append(s.warns, "session model unavailable: "+err.Error())
		}
	}
}

func (s *codeSession) pickSession() *savedSession {
	list := listSessions(s.workdir)
	if len(list) == 0 {
		list = listSessions("")
	}
	if len(list) == 0 {
		s.warns = append(s.warns, "no saved sessions")
		return nil
	}
	if len(list) > 50 {
		list = list[:50]
	}
	items := make([]string, len(list))
	for i, ss := range list {
		items[i] = fmt.Sprintf("%s  %s%s · %d msgs · %s%s", ss.Updated.Format("Jan 02 15:04"), oneLine(ss.Title, 50), cGray, len(ss.Messages), filepath.Base(ss.Dir), cReset)
	}
	idx := selectList(s.t, "Resume a session", items, 0)
	if idx < 0 {
		return nil
	}
	return list[idx]
}

func (s *codeSession) saveSession() {
	if s.sess == nil {
		return
	}
	s.sess.Model = s.modelRef()
	s.sess.Dir = s.workdir
	msgs := make([]chatMsg, 0, len(s.messages)+1)
	if s.summary != "" {
		msgs = append(msgs, chatMsg{Role: "summary", Content: s.summary})
	}
	msgs = append(msgs, s.messages...)
	s.sess.Messages = msgs
	if s.sess.Title == "" {
		for _, m := range s.messages {
			if m.Role == "user" {
				s.sess.Title = oneLine(m.Content, 80)
				break
			}
		}
	}
	if err := saveSession(s.sess); err != nil && s.ui != nil {
		s.ui.println("%s⚠ could not save session: %v%s", cYell, err, cReset)
	}
}

// ── system prompt & context ──────────────────────────────────────────────────

func (s *codeSession) systemPrompt() string {
	var b strings.Builder
	b.WriteString("You are Vortelio Code, an interactive coding agent running in the user's terminal. " +
		"You help with software engineering tasks in the working directory: reading and changing code, running commands, debugging, explaining.\n\n")

	b.WriteString("# Environment\n")
	fmt.Fprintf(&b, "- Working directory: %s (relative tool paths resolve here)\n", s.workdir)
	fmt.Fprintf(&b, "- Platform: %s/%s\n", goruntime.GOOS, goruntime.GOARCH)
	if goruntime.GOOS == "windows" {
		b.WriteString("- run_shell uses Windows PowerShell 5.1: chain with `;` (not `&&`), env vars are $env:NAME\n")
	} else {
		b.WriteString("- run_shell uses POSIX sh\n")
	}
	if branch, dirty := gitState(s.workdir); branch != "" {
		st := "clean"
		if dirty > 0 {
			st = fmt.Sprintf("%d uncommitted changes", dirty)
		}
		fmt.Fprintf(&b, "- Git: branch %s (%s)\n", branch, st)
	} else {
		b.WriteString("- Git: not a repository\n")
	}
	fmt.Fprintf(&b, "- Date: %s\n", time.Now().Format("2006-01-02"))
	if tree := topLevelListing(s.workdir, 40); tree != "" {
		b.WriteString("- Top-level entries: " + tree + "\n")
	}

	b.WriteString(`
# How to work
- Look before acting: use glob_search, grep_search and read_file to find and read the real code. Never guess file contents, paths or APIs.
- Read a file before editing it. Prefer edit_file (small exact replacements, unique old_string) over rewriting whole files. Match the existing style, naming and indentation.
- For tasks with several steps, track them with todo_write and keep it updated.
- After changing code, verify it when possible: build, run the tests or the program with run_shell, then fix what fails.
- Do what was asked, no more: no unrelated refactors, features or new files. Do not create docs or README files unless asked.
- Never commit, push, delete data or run destructive commands (rm -rf, git reset --hard, force push) unless the user explicitly asks.
- If a tool call is denied or blocked, do not retry it: adapt, or ask the user.
- If you need a decision only the user can make, use ask_user with 2-5 options.
- Paths in tool calls: prefer paths relative to the working directory.
`)
	switch s.mode {
	case "plan":
		b.WriteString("\n# PLAN MODE (active)\nYou are in read-only plan mode. Investigate with the read-only tools, then present a concise, numbered implementation plan (files to change and how) and stop. Do not try to edit files or run commands — those tools are blocked until the user switches mode.\n")
	case "auto":
		b.WriteString("\n# Auto mode\nEdits and commands run without confirmation. Work autonomously until the task is fully done and verified, then summarize.\n")
	}
	b.WriteString(`
# Communication
- Reply in the user's language. Be concise and direct: no preamble, no filler, no restating the question.
- Use Markdown sparingly (short lists, fenced code). Refer to code as path:line.
- When you finish a task, give a brief summary of what changed and how you verified it.
`)
	if len(s.instr) > 0 {
		b.WriteString("\n# Project instructions\nFollow these instructions from the user's instruction files; they override the defaults above.\n")
		for _, f := range s.instr {
			fmt.Fprintf(&b, "\n## %s\n%s\n", f.Path, strings.TrimSpace(f.Content))
		}
	}
	if s.summary != "" {
		b.WriteString("\n# Earlier in this session (summary)\n" + s.summary + "\n")
	}
	out := b.String()
	if len(s.skills) > 0 {
		out = server.ApplySkills(out, s.skills)
	}
	return out
}

func gitState(dir string) (string, int) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", 0
	}
	st, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	n := 0
	for _, l := range strings.Split(string(st), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return strings.TrimSpace(string(out)), n
}

func topLevelListing(dir string, max int) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if n == ".git" || isNoiseDirName(n) {
			continue
		}
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > max {
		names = append(names[:max], fmt.Sprintf("… (+%d)", len(names)-max))
	}
	return strings.Join(names, ", ")
}

// estimateTokens approximates the prompt size (chars/3.5).
func estimateTokens(chars int) int { return chars * 2 / 7 }

func (s *codeSession) contextUsed() int {
	n := len(s.systemPrompt()) + s.toolsChars + len(s.pendingContext)
	for _, m := range s.messages {
		n += len(m.Content) + 8
	}
	return estimateTokens(n)
}

func (s *codeSession) toolResultLimit() int {
	// Room for one large result: ~ a quarter of the window, in bytes.
	lim := s.contextLimit() // tokens ≈ bytes/4 → ctx/4 tokens = ctx bytes
	if lim > 60000 {
		lim = 60000
	}
	if lim < 6000 {
		lim = 6000
	}
	return lim
}

// ── the agent turn ───────────────────────────────────────────────────────────

func (s *codeSession) newProvider(ctx context.Context) interface {
	Tools() []runtime.ToolDef
	Execute(name, args string) (string, error)
} {
	return server.NewCLIToolProvider(server.CLIHarness{
		WorkDir:      s.workdir,
		Mode:         s.mode,
		Autonomous:   s.mode == "auto",
		MCP:          s.mcpOn,
		Media:        s.media,
		Emit:         s.onEvent,
		Approve:      s.approve,
		Ask:          s.askUser,
		Policy:       s.policy,
		OnFileChange: s.onFileChange,
		State:        s.state,
		Ctx:          ctx,
	})
}

func (s *codeSession) maxTurns() int {
	if s.opts.maxTurns > 0 {
		return s.opts.maxTurns
	}
	if s.settings.MaxTurns > 0 {
		return s.settings.MaxTurns
	}
	return defaultMaxTurns
}

// runTurn sends one user message and streams the agent's work.
func (s *codeSession) runTurn(userText string) (string, error) {
	if s.local == nil && s.cloudProvider == "" {
		s.ui.println("%s✗ no model selected — use /model%s", cRed, cReset)
		return "", errors.New("no model")
	}
	if s.local != nil && s.runner == nil {
		if err := s.loadLocal(); err != nil {
			s.ui.println("%s✗ %v%s", cRed, err, cReset)
			return "", err
		}
	}
	content := s.expandFileRefs(userText)
	if s.pendingContext != "" {
		content = s.pendingContext + "\n\n" + content
		s.pendingContext = ""
	}

	// Compact before the prompt would overflow the context window.
	if boolOr(s.settings.AutoCompact, true) && len(s.messages) >= 4 &&
		s.contextUsed()+estimateTokens(len(content)) > s.contextLimit()*3/4 {
		s.ui.println("%s✻ Context almost full — compacting the conversation…%s", cGray, cReset)
		if err := s.compact(""); err != nil {
			s.ui.println("%s⚠ compaction failed: %v%s", cYell, err, cReset)
		}
	}
	s.messages = append(s.messages, chatMsg{Role: "user", Content: content})

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel, s.cancelled = cancel, false
	s.turnCPs, s.turnSeen = nil, map[string]bool{}
	s.mu.Unlock()
	defer cancel()

	prov := s.newProvider(ctx)
	if s.toolsChars == 0 {
		if b, err := json.Marshal(prov.Tools()); err == nil {
			s.toolsChars = len(b)
		}
	}
	sys := s.systemPrompt()

	tty := s.t != nil && s.t.tty
	s.ui.beginTurn(tty)
	stopWatch := func() {}
	if tty {
		stopWatch = s.t.watch(func() {
			s.mu.Lock()
			s.cancelled = true
			s.mu.Unlock()
			cancel()
		})
	}

	var answer strings.Builder
	onContent := func(tok string) {
		answer.WriteString(tok)
		s.ui.content(tok)
	}
	var err error
	start := time.Now()
	if s.cloudProvider != "" {
		split := &thinkTagSplitter{content: onContent, think: s.ui.thinking}
		hist := make([]map[string]string, 0, len(s.messages))
		for _, m := range s.messages {
			hist = append(hist, map[string]string{"role": m.Role, "content": m.Content})
		}
		_, err = server.CLICloudTurn(ctx, s.cloudProvider, s.cloudModel, sys, prov, hist, s.maxTurns(), s.toolResultLimit(), split.feed, s.onEvent)
		split.flush()
	} else {
		msgs := []map[string]interface{}{{"role": "system", "content": sys}}
		for _, m := range s.messages {
			msgs = append(msgs, map[string]interface{}{"role": m.Role, "content": m.Content})
		}
		split := &thinkTagSplitter{content: onContent, think: s.ui.thinking}
		sopts := runtime.StreamOpts{
			Messages:        msgs,
			ToolsEnabled:    true,
			ToolProvider:    prov,
			ThinkEmit:       s.ui.thinking,
			MaxToolRounds:   s.maxTurns(),
			Ctx:             ctx,
			ToolResultLimit: s.toolResultLimit(),
		}
		// No repetition penalty: code legitimately repeats tokens (braces,
		// indentation, identifiers) and the chat default of 1.1 corrupts it.
		sopts.Options.RepeatPenalty = 1.0
		err = s.runner.StreamWithOpts(sopts, split.feed, s.onEvent)
		split.flush()
	}
	stopWatch()
	s.ui.endTurn()

	s.mu.Lock()
	cancelled := s.cancelled
	if len(s.turnCPs) > 0 {
		s.undo = append(s.undo, s.turnCPs)
	}
	s.turnCPs = nil
	s.cancel = nil
	s.mu.Unlock()

	text := strings.TrimSpace(answer.String())
	if cancelled || errors.Is(err, context.Canceled) {
		s.ui.println("  %s⎿  Interrupted — tell Vortelio what to do instead.%s", cYell, cReset)
		text += "\n\n[interrupted by the user]"
		err = nil
	} else if err != nil {
		s.ui.println("%s✗ %v%s", cRed, err, cReset)
	}

	// Keep a compact log of the actions in the history, so later turns know
	// what was already read, changed and run without replaying the outputs.
	hist := text
	if len(s.ui.toolLog) > 0 {
		hist = "[Actions: " + strings.Join(s.ui.toolLog, "; ") + "]\n\n" + text
	}
	if strings.TrimSpace(hist) == "" {
		hist = "(no answer)"
	}
	s.messages = append(s.messages, chatMsg{Role: "assistant", Content: hist})
	s.lastUsage = s.contextUsed()
	if !s.opts.print {
		s.ui.println("%s✻ %s · ~%s tokens out · context %d%%%s", cGray,
			time.Since(start).Round(100*time.Millisecond), humanCount(s.ui.tokens()),
			min(100, s.lastUsage*100/s.contextLimit()), cReset)
	}
	s.saveSession()
	return text, err
}

// thinkTagSplitter routes <think>…</think> blocks in the content stream (cloud
// reasoning, or models that inline their reasoning) to the reasoning view.
type thinkTagSplitter struct {
	content func(string)
	think   func(string)
	buf     string
	in      bool
}

func (t *thinkTagSplitter) feed(tok string) {
	t.buf += tok
	for t.buf != "" {
		tag := "<think>"
		if t.in {
			tag = "</think>"
		}
		if i := strings.Index(t.buf, tag); i >= 0 {
			t.emit(t.buf[:i])
			t.buf = t.buf[i+len(tag):]
			t.in = !t.in
			continue
		}
		// Keep a possible partial tag at the end.
		keep := 0
		for k := len(tag) - 1; k > 0; k-- {
			if strings.HasSuffix(t.buf, tag[:k]) {
				keep = k
				break
			}
		}
		t.emit(t.buf[:len(t.buf)-keep])
		t.buf = t.buf[len(t.buf)-keep:]
		return
	}
}

func (t *thinkTagSplitter) emit(s string) {
	if s == "" {
		return
	}
	if t.in {
		t.think(s)
	} else {
		t.content(s)
	}
}

func (t *thinkTagSplitter) flush() {
	t.emit(t.buf)
	t.buf = ""
}

// onEvent renders tool events coming from the harness.
func (s *codeSession) onEvent(ev string, data interface{}) {
	b, _ := json.Marshal(data)
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	switch ev {
	case "tool_call":
		s.ui.toolCall(str("name"), str("arguments"))
	case "tool_result":
		s.ui.toolResult(str("name"), str("result"), str("error"))
	case "file_diff":
		add, _ := m["added"].(float64)
		del, _ := m["removed"].(float64)
		created, _ := m["created"].(bool)
		s.ui.fileDiff(str("path"), str("diff"), int(add), int(del), created)
	case "todo_update":
		var t struct {
			Todos []todoView `json:"todos"`
		}
		json.Unmarshal(b, &t)
		s.ui.todos(t.Todos)
	case "media_generated":
		s.ui.mediaGenerated(str("path"))
	}
}

// policy applies permission rules, then the mode, for each tool call.
func (s *codeSession) policy(tool, args string) string {
	if d := s.settings.Permissions.decide(tool, args); d == "deny" {
		return server.PolicyDeny
	} else if d == "allow" && s.mode != "plan" {
		return server.PolicyAllow
	}
	if s.mode != "plan" {
		for _, r := range s.sessAllow {
			if pr, ok := parseRule(r); ok && pr.matches(tool, args) {
				return server.PolicyAllow
			}
		}
	}
	// The mode can change mid-turn (approval "don't ask again"): honor it live.
	switch s.mode {
	case "auto":
		return server.PolicyAllow
	case "edits":
		if tool == "write_file" || tool == "edit_file" {
			return server.PolicyAllow
		}
	}
	return ""
}

func (s *codeSession) onFileChange(path string, before []byte, existed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(path)
	if s.turnSeen[key] {
		return
	}
	s.turnSeen[key] = true
	s.turnCPs = append(s.turnCPs, fileCheckpoint{path: path, before: before, existed: existed})
}

// approve asks the user to confirm an edit or command (ask/edits modes).
func (s *codeSession) approve(tool, summary, args string) bool {
	if s.opts.print || s.t == nil {
		return false
	}
	s.ui.pause()
	s.t.beginPrompt()
	defer func() {
		s.t.endPrompt()
		s.ui.resume()
	}()

	head, diff, _ := strings.Cut(summary, "\n")
	fmt.Printf("%s╭─%s %s%s%s\n", cYell, cReset, cBold, head, cReset)
	if diff != "" {
		s.ui.renderDiff(diff, 40)
	}
	var cmd string
	if tool == "run_shell" {
		var a struct {
			Command string `json:"command"`
		}
		json.Unmarshal([]byte(args), &a)
		cmd = a.Command
		fmt.Printf("     %s%s%s\n", cCode, cmd, cReset)
	}
	var items []string
	switch tool {
	case "write_file", "edit_file":
		items = []string{"Yes", "Yes, and don't ask again for file edits (accept-edits mode)", "No, and tell Vortelio what to do differently (esc)"}
	case "run_shell":
		items = []string{"Yes", fmt.Sprintf("Yes, and don't ask again for `%s` in this project", strings.TrimSuffix(strings.TrimPrefix(suggestShellRule(cmd), "run_shell("), ")")), "No, and tell Vortelio what to do differently (esc)"}
	default:
		items = []string{"Yes", "Yes, and don't ask again this session (auto mode)", "No, and tell Vortelio what to do differently (esc)"}
	}
	q := "Do you want to proceed?"
	switch tool {
	case "edit_file":
		q = "Do you want to make this edit?"
	case "write_file":
		q = "Do you want to write this file?"
	case "run_shell":
		q = "Run this command?"
	}
	idx := selectList(s.t, q, items, 0)
	switch idx {
	case 0:
		if diff != "" {
			s.markDiffShown(head)
		}
		return true
	case 1:
		switch tool {
		case "write_file", "edit_file":
			s.setMode("edits", true)
		case "run_shell":
			rule := suggestShellRule(cmd)
			if rule != "" {
				s.sessAllow = append(s.sessAllow, rule)
				if err := updateProjectSettings(s.workdir, func(p *codeSettings) {
					p.Permissions.Allow = appendUnique(p.Permissions.Allow, rule)
				}); err == nil {
					s.settings.Permissions.Allow = appendUnique(s.settings.Permissions.Allow, rule)
					fmt.Printf("  %sSaved rule %s to %s%s\n", cGray, rule, projectSettingsPath(s.workdir), cReset)
				}
			}
		default:
			s.setMode("auto", true)
		}
		if diff != "" {
			s.markDiffShown(head)
		}
		return true
	default:
		s.mu.Lock()
		s.cancelled = true
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		return false
	}
}

func (s *codeSession) markDiffShown(head string) {
	if _, p, ok := strings.Cut(head, ": "); ok {
		s.ui.mu.Lock()
		if s.ui.diffShown == nil {
			s.ui.diffShown = map[string]bool{}
		}
		s.ui.diffShown[strings.TrimSpace(p)] = true
		s.ui.mu.Unlock()
	}
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// askUser answers the ask_user tool.
func (s *codeSession) askUser(question string, options []string) string {
	if s.opts.print || s.t == nil {
		return "No user is available (non-interactive run). Make a reasonable choice yourself and state the assumption."
	}
	s.ui.pause()
	s.t.beginPrompt()
	defer func() {
		s.t.endPrompt()
		s.ui.resume()
	}()
	fmt.Printf("%s?%s %s%s%s\n", cAccent, cReset, cBold, question, cReset)
	if len(options) > 0 {
		items := append(append([]string{}, options...), "Type something else…")
		idx := selectList(s.t, "", items, 0)
		if idx >= 0 && idx < len(options) {
			return "The user chose: " + options[idx]
		}
		if idx < 0 {
			return "The user dismissed the question without answering."
		}
	}
	ans, ok := promptLine(s.t, "  > ")
	if !ok || ans == "" {
		return "The user did not answer."
	}
	return "The user answered: " + ans
}

// ── modes ────────────────────────────────────────────────────────────────────

func nextMode(m string) string {
	switch m {
	case "ask":
		return "edits"
	case "edits":
		return "auto"
	case "auto":
		return "plan"
	default:
		return "ask"
	}
}

func modeLabel(m string) string {
	switch m {
	case "plan":
		return cCyan + "⏸ plan mode (read-only)" + cReset
	case "edits":
		return cMag + "⏵⏵ accept edits" + cReset
	case "auto":
		return cYell + "⏵⏵ auto (no prompts)" + cReset
	default:
		return cGray + "ask before edits" + cReset
	}
}

// setMode changes the mode for this session only (the default comes from the
// "mode" setting: /config set mode <m>).
func (s *codeSession) setMode(m string, announce bool) {
	s.mode = m
	if announce {
		fmt.Printf("  %sMode: %s\n", cReset, modeLabel(m))
	}
}

func (s *codeSession) footer() string {
	pct := 0
	if lim := s.contextLimit(); lim > 0 {
		pct = min(100, s.contextUsed()*100/lim)
	}
	ctx := fmt.Sprintf("%sctx %d%%%s", cGray, pct, cReset)
	if pct >= 75 {
		ctx = fmt.Sprintf("%sctx %d%% (/compact)%s", cYell, pct, cReset)
	}
	return modeLabel(s.mode) + cGray + " · " + s.modelLabel() + " · " + cReset + ctx
}

// ── @file references, !commands, # notes ─────────────────────────────────────

var fileRefRE = regexp.MustCompile(`(^|\s)@([^\s"']+)`)

func (s *codeSession) expandFileRefs(line string) string {
	var extras []string
	for _, m := range fileRefRE.FindAllStringSubmatch(line, -1) {
		p := m[2]
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(s.workdir, p)
		}
		fi, err := os.Stat(full)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			entries, _ := os.ReadDir(full)
			var names []string
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			extras = append(extras, fmt.Sprintf("<directory path=%q>\n%s\n</directory>", p, strings.Join(names, "\n")))
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		limit := s.toolResultLimit()
		if len(data) > limit {
			data = append(data[:limit], []byte("\n… (truncated; use read_file with offset for the rest)")...)
		}
		s.state.MarkRead(full)
		extras = append(extras, fmt.Sprintf("<file path=%q>\n%s\n</file>", p, string(data)))
		if s.ui != nil {
			s.ui.println("  %s⎿  attached %s (%d bytes)%s", cGray, p, len(data), cReset)
		}
	}
	if len(extras) == 0 {
		return line
	}
	return line + "\n\n" + strings.Join(extras, "\n\n")
}

// runBang runs a shell command typed by the user (!cmd). Its output is shown
// and added to the context of the next prompt.
func (s *codeSession) runBang(command string) {
	if command == "" {
		return
	}
	var c *exec.Cmd
	if goruntime.GOOS == "windows" {
		c = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; "+command)
	} else {
		c = exec.Command("sh", "-c", command)
	}
	c.Dir = s.workdir
	out, err := c.CombinedOutput()
	res := strings.TrimRight(string(out), "\r\n")
	for _, l := range strings.Split(res, "\n") {
		fmt.Printf("  %s%s%s\n", cGray, strings.TrimRight(l, "\r"), cReset)
	}
	if err != nil {
		fmt.Printf("  %s%v%s\n", cRed, err, cReset)
	}
	if len(res) > 8000 {
		res = res[:8000] + "\n… (truncated)"
	}
	s.pendingContext += fmt.Sprintf("<shell-command>\n$ %s\n%s\n</shell-command>", command, res)
}

// addMemory appends a note to the project's AGENTS.md (# note).
func (s *codeSession) addMemory(note string) {
	if note == "" {
		return
	}
	p := filepath.Join(s.workdir, "AGENTS.md")
	var b strings.Builder
	if data, err := os.ReadFile(p); err == nil {
		b.Write(data)
		if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
	} else {
		b.WriteString("# AGENTS.md\n\nInstructions for AI coding agents working in this project.\n\n")
	}
	b.WriteString("- " + note + "\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
		return
	}
	s.instr = loadInstructions(s.workdir)
	fmt.Printf("  %s⎿  Saved to %s%s\n", cGray, p, cReset)
}

// ── compaction ───────────────────────────────────────────────────────────────

// compact replaces the conversation with a dense summary to free context.
func (s *codeSession) compact(focus string) error {
	if len(s.messages) == 0 {
		return errors.New("nothing to compact")
	}
	var tr strings.Builder
	if s.summary != "" {
		tr.WriteString("Earlier summary:\n" + s.summary + "\n\n")
	}
	for _, m := range s.messages {
		fmt.Fprintf(&tr, "### %s\n%s\n\n", m.Role, m.Content)
	}
	text := tr.String()
	maxChars := s.contextLimit() * 2 // leave room for the answer
	if len(text) > maxChars {
		text = "…(older part omitted)\n" + text[len(text)-maxChars:]
	}
	instr := "Summarize this coding session so the work can continue without the full transcript. " +
		"Keep: the user's goals and requests, decisions and constraints, files read or changed (with what changed), " +
		"commands run and their outcome, errors still open, and the next steps. Be dense and factual, use short bullet points. " +
		"Write in the user's language."
	if focus != "" {
		instr += " Focus especially on: " + focus
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.spin("Compacting conversation")
	var out strings.Builder
	var err error
	if s.cloudProvider != "" {
		_, err = server.CLICloudTurn(ctx, s.cloudProvider, s.cloudModel, instr, nil,
			[]map[string]string{{"role": "user", "content": text}}, 1, 0, func(t string) { out.WriteString(t) }, nil)
	} else {
		if s.runner == nil {
			if err := s.loadLocal(); err != nil {
				stop()
				return err
			}
		}
		sopts := runtime.StreamOpts{
			Messages: []map[string]interface{}{{"role": "system", "content": instr}, {"role": "user", "content": text}},
			Ctx:      ctx,
		}
		split := &thinkTagSplitter{content: func(t string) { out.WriteString(t) }, think: func(string) {}}
		err = s.runner.StreamWithOpts(sopts, split.feed, nil)
		split.flush()
	}
	stop()
	if err != nil {
		return err
	}
	sum := strings.TrimSpace(regexp.MustCompile(`(?s)<think>.*?</think>`).ReplaceAllString(out.String(), ""))
	if sum == "" {
		return errors.New("the model returned an empty summary")
	}
	before := s.contextUsed()
	s.summary = sum
	s.messages = nil
	s.saveSession()
	if s.ui != nil {
		s.ui.println("  %s⎿  Compacted: ~%s → ~%s tokens%s", cGray, humanCount(before), humanCount(s.contextUsed()), cReset)
	}
	return nil
}

// ── undo ─────────────────────────────────────────────────────────────────────

func (s *codeSession) undoLast() {
	if len(s.undo) == 0 {
		fmt.Printf("  %sNothing to undo.%s\n", cGray, cReset)
		return
	}
	cps := s.undo[len(s.undo)-1]
	s.undo = s.undo[:len(s.undo)-1]
	var names []string
	for i := len(cps) - 1; i >= 0; i-- {
		cp := cps[i]
		rel, _ := filepath.Rel(s.workdir, cp.path)
		if cp.existed {
			if err := os.WriteFile(cp.path, cp.before, 0o644); err != nil {
				fmt.Printf("  %s✗ %s: %v%s\n", cRed, rel, err, cReset)
				continue
			}
			fmt.Printf("  %s⎿  restored %s%s\n", cGray, rel, cReset)
		} else {
			if err := os.Remove(cp.path); err != nil && !os.IsNotExist(err) {
				fmt.Printf("  %s✗ %s: %v%s\n", cRed, rel, err, cReset)
				continue
			}
			fmt.Printf("  %s⎿  removed %s%s\n", cGray, rel, cReset)
		}
		names = append(names, filepath.ToSlash(rel))
	}
	s.pendingContext += "[The user undid the file changes of the previous turn: " + strings.Join(names, ", ") + ". These files are back to their earlier content.]"
}

// ── print mode ───────────────────────────────────────────────────────────────

func (s *codeSession) runPrint() error {
	prompt := s.opts.prompt
	if !isTTY(os.Stdin) {
		// Piped input is appended to the prompt. With a prompt argument, don't
		// wait on a stdin that is open but never written to (IDE/CI shells).
		ch := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(os.Stdin)
			ch <- data
		}()
		var data []byte
		if prompt == "" {
			data = <-ch
		} else {
			select {
			case data = <-ch:
			case <-time.After(300 * time.Millisecond):
			}
		}
		if in := strings.TrimSpace(string(data)); in != "" {
			if prompt != "" {
				prompt += "\n\n" + in
			} else {
				prompt = in
			}
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("no prompt: vortelio code -p \"your request\"")
	}
	out := io.Writer(os.Stdout)
	if s.opts.outputFormat == "json" {
		out = io.Discard
	}
	s.ui = newCodeUI(out, os.Stderr, false, false)
	s.ui.quietTools = true
	s.ui.plain = true
	// Keep stdout for the answer only: progress printed by the runtime (model
	// loading, engine download) goes to stderr.
	realStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = realStdout }()
	if err := s.pickInitialModel(); err != nil {
		return err
	}
	if err := s.initSession(); err != nil {
		return err
	}
	for _, w := range s.warns {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	start := time.Now()
	text, err := s.runTurn(prompt)
	if s.opts.outputFormat == "json" {
		res := map[string]interface{}{
			"type":        "result",
			"is_error":    err != nil,
			"result":      text,
			"session_id":  s.sess.ID,
			"model":       s.modelRef(),
			"duration_ms": time.Since(start).Milliseconds(),
			"actions":     append([]string{}, s.ui.toolLog...),
		}
		if err != nil {
			res["error"] = err.Error()
		}
		enc := json.NewEncoder(realStdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
	} else {
		fmt.Fprintln(realStdout)
	}
	return err
}

// ── banner ───────────────────────────────────────────────────────────────────

func (s *codeSession) printBanner() {
	w := min(termWidth()-2, 72)
	line := strings.Repeat("─", w-2)
	fmt.Printf("%s╭%s╮%s\n", cAccent, line, cReset)
	title := fmt.Sprintf("✻ Vortelio Code  v%s", version.Version)
	fmt.Printf("%s│%s %s%s%s%s%s│%s\n", cAccent, cReset, cBold, title, cReset, strings.Repeat(" ", max(0, w-3-len([]rune(title)))), cAccent, cReset)
	fmt.Printf("%s╰%s╯%s\n", cAccent, line, cReset)
	fmt.Printf("  %scwd%s    %s\n", cGray, cReset, s.workdir)
	where := "local"
	if s.cloudProvider != "" {
		where = "cloud"
	}
	fmt.Printf("  %smodel%s  %s %s(%s)%s\n", cGray, cReset, s.modelLabel(), cGray, where, cReset)
	fmt.Printf("  %smode%s   %s\n", cGray, cReset, modeLabel(s.mode))
	if len(s.instr) > 0 {
		var names []string
		for _, f := range s.instr {
			names = append(names, shortPath(f.Path, s.workdir))
		}
		fmt.Printf("  %smemory%s %s\n", cGray, cReset, strings.Join(names, ", "))
	} else {
		fmt.Printf("  %smemory%s %snone — run /init to create AGENTS.md%s\n", cGray, cReset, cGray, cReset)
	}
	if s.sess != nil && len(s.messages) > 0 {
		fmt.Printf("  %sresumed%s %s (%d messages)\n", cGray, cReset, oneLine(s.sess.Title, 50), len(s.messages))
	}
	for _, w := range s.warns {
		fmt.Printf("  %s⚠ %s%s\n", cYell, w, cReset)
	}
	fmt.Printf("\n  %s/help for commands · ? for shortcuts · esc to interrupt%s\n", cGray, cReset)
}

func shortPath(p, workdir string) string {
	if rel, err := filepath.Rel(workdir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// ── prompt history (↑/↓ across sessions) ─────────────────────────────────────

func promptHistoryPath() string {
	return filepath.Join(filepath.Dir(sessionsDir()), "code_history")
}

func loadPromptHistory() []string {
	data, err := os.ReadFile(promptHistoryPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" {
			continue
		}
		if u, err := strconv.Unquote(l); err == nil {
			out = append(out, u)
		}
	}
	if len(out) > 500 {
		out = out[len(out)-500:]
	}
	return out
}

func appendPromptHistory(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	os.MkdirAll(filepath.Dir(promptHistoryPath()), 0o755)
	f, err := os.OpenFile(promptHistoryPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strconv.Quote(line) + "\n")
}
