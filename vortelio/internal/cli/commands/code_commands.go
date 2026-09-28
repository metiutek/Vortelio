package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/hub"
	"github.com/vortelio/vortelio/internal/runtime"
	"github.com/vortelio/vortelio/internal/server"
	"github.com/vortelio/vortelio/internal/version"
)

type slashCommand struct {
	name    string
	aliases []string
	args    string
	desc    string
}

var slashCommands = []slashCommand{
	{"help", []string{"?"}, "", "show commands and shortcuts"},
	{"model", []string{"m"}, "[ref]", "switch model (local or cloud)"},
	{"mode", nil, "[plan|ask|edits|auto]", "permission mode (shift+tab cycles)"},
	{"init", nil, "", "create or update AGENTS.md for this project"},
	{"clear", []string{"new", "reset"}, "", "start a new conversation"},
	{"compact", nil, "[focus]", "summarize the conversation to free context"},
	{"context", []string{"cost", "usage"}, "", "show context window usage"},
	{"resume", []string{"sessions"}, "[id]", "resume a previous session"},
	{"undo", []string{"rewind"}, "", "revert the file changes of the last turn"},
	{"diff", nil, "", "show uncommitted changes (git diff)"},
	{"todos", nil, "", "show the current task list"},
	{"memory", nil, "", "show the instruction files in use"},
	{"permissions", []string{"allowed-tools"}, "[allow|deny|remove <rule>]", "manage permission rules"},
	{"config", []string{"settings"}, "[set <key> <value>]", "show or change settings"},
	{"thinking", nil, "[on|off]", "show or hide the model's reasoning"},
	{"status", nil, "", "session, model and environment info"},
	{"skills", nil, "", "enable or disable skills"},
	{"mcp", nil, "[on|off]", "expose connected MCP tools"},
	{"media", nil, "[on|off]", "expose image/audio/video/3D generation tools"},
	{"cd", nil, "<dir>", "change the working directory"},
	{"export", nil, "[file]", "save the conversation as Markdown"},
	{"exit", []string{"quit", "q"}, "", "exit"},
}

func findSlash(name string) (slashCommand, bool) {
	for _, c := range slashCommands {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return slashCommand{}, false
}

// completions feeds the prompt editor: /commands at the start, @files anywhere.
func (s *codeSession) completions(buf string) (int, []suggestion) {
	if strings.HasPrefix(buf, "/") && !strings.ContainsAny(buf, " \n") {
		pre := strings.ToLower(buf[1:])
		var out []suggestion
		for _, c := range slashCommands {
			if strings.HasPrefix(c.name, pre) {
				ins := "/" + c.name
				if c.args != "" && strings.HasPrefix(c.args, "<") {
					ins += " "
				}
				out = append(out, suggestion{insert: ins, label: "/" + c.name, desc: c.desc})
			}
		}
		var names []string
		for n := range s.customCmds {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if strings.HasPrefix(n, pre) {
				d := s.customCmds[n].Description
				if d == "" {
					d = "custom command"
				}
				out = append(out, suggestion{insert: "/" + n, label: "/" + n, desc: d + " (custom)"})
			}
		}
		return 0, out
	}
	return fileSuggestions(s.workdir, buf)
}

// handleCommand runs a /command. Returns true to exit.
func (s *codeSession) handleCommand(line string) bool {
	name, rest, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	name = strings.ToLower(strings.TrimSpace(name))
	rest = strings.TrimSpace(rest)
	arg1, _, _ := strings.Cut(rest, " ")

	if cc, ok := s.customCmds[name]; ok {
		if _, builtin := findSlash(name); !builtin {
			s.runTurn(cc.expand(rest))
			return false
		}
	}
	cmd, ok := findSlash(name)
	if !ok {
		fmt.Printf("  %sUnknown command /%s — /help lists them.%s\n", cGray, name, cReset)
		return false
	}
	switch cmd.name {
	case "exit":
		return true
	case "help":
		s.printHelp()
	case "model":
		s.cmdModel(rest)
	case "mode":
		if rest == "" {
			fmt.Printf("  Mode: %s  %s(/mode plan|ask|edits|auto · shift+tab)%s\n", modeLabel(s.mode), cGray, cReset)
			return false
		}
		if !validMode(arg1) {
			fmt.Printf("  %sUnknown mode %q — use plan, ask, edits or auto.%s\n", cRed, arg1, cReset)
			return false
		}
		s.setMode(arg1, true)
	case "init":
		s.runTurn(initPrompt(s.workdir))
		s.instr = loadInstructions(s.workdir)
	case "clear":
		s.saveSession()
		s.messages, s.summary, s.pendingContext = nil, "", ""
		s.state = server.NewCodingState()
		s.undo = nil
		s.sess = &savedSession{ID: newSessionID(), Dir: s.workdir, Created: time.Now()}
		if s.t != nil && s.t.tty {
			fmt.Print("\033[H\033[2J")
		}
		fmt.Printf("  %sNew conversation.%s\n", cGray, cReset)
	case "compact":
		if err := s.compact(rest); err != nil {
			fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
		}
	case "context":
		s.printContext()
	case "resume":
		var ss *savedSession
		if rest != "" {
			var err error
			if ss, err = loadSession(arg1); err != nil {
				fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
				return false
			}
		} else {
			ss = s.pickSession()
		}
		if ss != nil {
			s.saveSession()
			s.adoptSession(ss)
			fmt.Printf("  %sResumed %q (%d messages, %s).%s\n", cGray, oneLine(ss.Title, 60), len(s.messages), s.modelLabel(), cReset)
			s.replayTail(6)
		}
	case "undo":
		s.undoLast()
	case "diff":
		s.showDiff()
	case "todos":
		s.state.Lock()
		todos := append([]server.TodoItem(nil), s.state.Todos...)
		s.state.Unlock()
		if len(todos) == 0 {
			fmt.Printf("  %sNo tasks.%s\n", cGray, cReset)
			return false
		}
		var v []todoView
		for _, t := range todos {
			v = append(v, todoView{Content: t.Content, Status: t.Status})
		}
		s.ui.todos(v)
	case "memory":
		s.instr = loadInstructions(s.workdir)
		if len(s.instr) == 0 {
			fmt.Printf("  %sNo instruction files. Run /init to create AGENTS.md, or type # note to add one.%s\n", cGray, cReset)
		}
		for _, f := range s.instr {
			fmt.Printf("  %s  %s(%d bytes)%s\n", f.Path, cGray, len(f.Content), cReset)
		}
		fmt.Printf("  %sLoaded in this order: ~/.vortelio/AGENTS.md, then AGENTS.md / VORTELIO.md / CLAUDE.md from the repo root down to the working directory.%s\n", cGray, cReset)
	case "permissions":
		s.cmdPermissions(rest)
	case "config":
		s.cmdConfig(rest)
	case "thinking":
		switch strings.ToLower(arg1) {
		case "on":
			s.showThinking = true
		case "off":
			s.showThinking = false
		case "":
			s.showThinking = !s.showThinking
		default:
			fmt.Printf("  %sUse /thinking on|off%s\n", cGray, cReset)
			return false
		}
		s.ui.showThinking = s.showThinking
		v := s.showThinking
		_ = updateGlobalSettings(func(g *codeSettings) { g.ShowThinking = &v })
		fmt.Printf("  %sReasoning display: %s%s\n", cGray, onOff(v), cReset)
	case "status":
		s.printStatus()
	case "skills":
		s.chooseSkills()
	case "mcp":
		s.mcpOn = toggleArg(arg1, s.mcpOn)
		v := s.mcpOn
		_ = updateGlobalSettings(func(g *codeSettings) { g.MCP = &v })
		fmt.Printf("  %sMCP tools: %s%s\n", cGray, onOff(v), cReset)
	case "media":
		s.media = toggleArg(arg1, s.media)
		v := s.media
		_ = updateGlobalSettings(func(g *codeSettings) { g.MediaTools = &v })
		fmt.Printf("  %sMedia tools: %s%s\n", cGray, onOff(v), cReset)
	case "cd":
		if rest == "" {
			fmt.Printf("  %s%s%s\n", cGray, s.workdir, cReset)
			return false
		}
		d := rest
		if !filepath.IsAbs(d) {
			d = filepath.Join(s.workdir, d)
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			fmt.Printf("  %s✗ not a directory: %s%s\n", cRed, d, cReset)
			return false
		}
		s.workdir = filepath.Clean(d)
		s.instr = loadInstructions(s.workdir)
		s.customCmds = loadCustomCommands(s.workdir)
		s.settings, s.warns = effectiveSettings(s.workdir)
		s.state = server.NewCodingState()
		fmt.Printf("  %sWorking directory: %s%s\n", cGray, s.workdir, cReset)
	case "export":
		s.export(rest)
	}
	return false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func toggleArg(arg string, cur bool) bool {
	switch strings.ToLower(arg) {
	case "on", "true", "1":
		return true
	case "off", "false", "0":
		return false
	}
	return !cur
}

func (s *codeSession) printHelp() {
	fmt.Printf("\n  %sCommands%s\n", cBold, cReset)
	for _, c := range slashCommands {
		n := "/" + c.name
		if c.args != "" {
			n += " " + c.args
		}
		fmt.Printf("  %s%-36s%s %s\n", cAccent, n, cReset, c.desc)
	}
	if len(s.customCmds) > 0 {
		fmt.Printf("\n  %sCustom commands%s\n", cBold, cReset)
		var names []string
		for n := range s.customCmds {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := s.customCmds[n]
			fmt.Printf("  %s%-36s%s %s %s(%s)%s\n", cAccent, "/"+n, cReset, c.Description, cGray, shortPath(c.Path, s.workdir), cReset)
		}
	}
	fmt.Printf("\n  %sInput%s\n", cBold, cReset)
	for _, l := range shortcutHelp() {
		fmt.Println(l)
	}
}

// ── /model ───────────────────────────────────────────────────────────────────

func (s *codeSession) cmdModel(ref string) {
	if ref == "" {
		ref = s.pickModel()
		if ref == "" {
			return
		}
	}
	prevLocal, prevProv, prevModel := s.local, s.cloudProvider, s.cloudModel
	if err := s.setModelRef(ref); err != nil {
		fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
		return
	}
	if s.local != nil {
		if err := s.loadLocal(); err != nil {
			fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
			s.local, s.cloudProvider, s.cloudModel = prevLocal, prevProv, prevModel
			return
		}
	}
	r := s.modelRef()
	_ = updateGlobalSettings(func(g *codeSettings) { g.Model = r })
	fmt.Printf("  %sModel: %s%s\n", cGray, s.modelLabel(), cReset)
}

func (s *codeSession) pickModel() string {
	models, _ := hub.NewModelStore().List()
	var refs, items []string
	start := 0
	cur := s.modelRef()
	for _, m := range models {
		if m.Type != "llm" {
			continue
		}
		ref := m.Name + ":" + m.Tag
		tl := ""
		if runtime.ModelSupportsTools(ref) {
			tl = cGray + " · tools" + cReset
		}
		if ref == cur {
			start = len(items)
		}
		refs = append(refs, ref)
		items = append(items, "💻 "+ref+tl)
	}
	for _, c := range server.CloudModelsForCLI() {
		ref := "cloud/" + c.Provider + "/" + c.Model
		if ref == cur {
			start = len(items)
		}
		refs = append(refs, ref)
		items = append(items, "☁  "+c.Label+cGray+" · "+c.ProviderName+cReset)
	}
	if len(items) == 0 {
		fmt.Printf("  %sNo models. Install one with: vortelio pull qwen3.5:4b%s\n", cGray, cReset)
		return ""
	}
	idx := selectList(s.t, "Select a model", items, start)
	if idx < 0 {
		return ""
	}
	return refs[idx]
}

// ── /permissions ─────────────────────────────────────────────────────────────

func (s *codeSession) cmdPermissions(rest string) {
	verb, rule, _ := strings.Cut(rest, " ")
	rule = strings.TrimSpace(rule)
	switch strings.ToLower(verb) {
	case "":
		g, _ := readSettings(globalSettingsPath())
		p, _ := readSettings(projectSettingsPath(s.workdir))
		show := func(title string, list []string, src string) {
			for _, r := range list {
				fmt.Printf("  %-6s %s%s  %s(%s)%s\n", title, cAccent, r, cGray, src, cReset)
			}
		}
		fmt.Printf("  Mode: %s\n", modeLabel(s.mode))
		show("deny", g.Permissions.Deny, "global")
		show("deny", p.Permissions.Deny, "project")
		show("allow", g.Permissions.Allow, "global")
		show("allow", p.Permissions.Allow, "project")
		for _, r := range s.sessAllow {
			fmt.Printf("  %-6s %s%s  %s(this session)%s\n", "allow", cAccent, r, cGray, cReset)
		}
		fmt.Printf("  %sRules: Tool or Tool(pattern), e.g. run_shell(go test*), edit_file(src/**), Bash(npm run *). Deny wins.%s\n", cGray, cReset)
		fmt.Printf("  %s/permissions allow <rule> · /permissions deny <rule> · /permissions remove <rule>  (saved in %s)%s\n", cGray, shortPath(projectSettingsPath(s.workdir), s.workdir), cReset)
	case "allow", "deny":
		if _, ok := parseRule(rule); !ok || rule == "" {
			fmt.Printf("  %s✗ invalid rule %q%s\n", cRed, rule, cReset)
			return
		}
		err := updateProjectSettings(s.workdir, func(p *codeSettings) {
			if verb == "allow" {
				p.Permissions.Allow = appendUnique(p.Permissions.Allow, rule)
			} else {
				p.Permissions.Deny = appendUnique(p.Permissions.Deny, rule)
			}
		})
		if err != nil {
			fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
			return
		}
		s.settings, _ = effectiveSettings(s.workdir)
		fmt.Printf("  %sAdded %s rule %s%s\n", cGray, verb, rule, cReset)
	case "remove", "rm":
		removed := false
		for _, path := range []string{projectSettingsPath(s.workdir), globalSettingsPath()} {
			st, err := readSettings(path)
			if err != nil {
				continue
			}
			na, nd := without(st.Permissions.Allow, rule), without(st.Permissions.Deny, rule)
			if len(na) != len(st.Permissions.Allow) || len(nd) != len(st.Permissions.Deny) {
				st.Permissions.Allow, st.Permissions.Deny = na, nd
				writeSettings(path, st)
				removed = true
			}
		}
		s.sessAllow = without(s.sessAllow, rule)
		s.settings, _ = effectiveSettings(s.workdir)
		if removed {
			fmt.Printf("  %sRemoved %s%s\n", cGray, rule, cReset)
		} else {
			fmt.Printf("  %sRule not found: %s%s\n", cGray, rule, cReset)
		}
	default:
		fmt.Printf("  %sUse /permissions [allow|deny|remove <rule>]%s\n", cGray, cReset)
	}
}

func without(list []string, v string) []string {
	var out []string
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// ── /config ──────────────────────────────────────────────────────────────────

var configKeys = []string{"model", "mode", "show_thinking", "auto_compact", "context_tokens", "max_turns", "media_tools", "mcp"}

func (s *codeSession) cmdConfig(rest string) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		data, _ := json.MarshalIndent(s.settings, "  ", "  ")
		fmt.Printf("  %sEffective settings%s\n  %s\n", cBold, cReset, string(data))
		fmt.Printf("  %sglobal:  %s%s\n", cGray, globalSettingsPath(), cReset)
		fmt.Printf("  %sproject: %s%s\n", cGray, projectSettingsPath(s.workdir), cReset)
		fmt.Printf("  %s/config set <key> <value> [--project]   keys: %s%s\n", cGray, strings.Join(configKeys, ", "), cReset)
		return
	}
	if fields[0] != "set" || len(fields) < 3 {
		fmt.Printf("  %sUse /config set <key> <value> [--project]%s\n", cGray, cReset)
		return
	}
	key, val := fields[1], fields[2]
	project := len(fields) > 3 && fields[3] == "--project"
	apply := func(c *codeSettings) error {
		b := func() (*bool, error) {
			v, err := strconv.ParseBool(map[string]string{"on": "true", "off": "false"}[val] + val)
			if err != nil {
				v, err = strconv.ParseBool(val)
			}
			if err != nil {
				return nil, fmt.Errorf("%s must be true/false", key)
			}
			return &v, nil
		}
		switch key {
		case "model":
			c.Model = val
		case "mode":
			if !validMode(val) {
				return fmt.Errorf("mode must be plan, ask, edits or auto")
			}
			c.Mode = val
		case "show_thinking", "auto_compact", "media_tools", "mcp":
			p, err := b()
			if err != nil {
				return err
			}
			switch key {
			case "show_thinking":
				c.ShowThinking = p
			case "auto_compact":
				c.AutoCompact = p
			case "media_tools":
				c.MediaTools = p
			case "mcp":
				c.MCP = p
			}
		case "context_tokens", "max_turns":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return fmt.Errorf("%s must be a positive number", key)
			}
			if key == "context_tokens" {
				if n < 4096 {
					return fmt.Errorf("context_tokens must be at least 4096")
				}
				c.ContextTokens = n
			} else {
				c.MaxTurns = n
			}
		default:
			return fmt.Errorf("unknown key %q (keys: %s)", key, strings.Join(configKeys, ", "))
		}
		return nil
	}
	var applyErr error
	fn := func(c *codeSettings) { applyErr = apply(c) }
	var err error
	if project {
		err = updateProjectSettings(s.workdir, fn)
	} else {
		err = updateGlobalSettings(fn)
	}
	if applyErr != nil {
		fmt.Printf("  %s✗ %v%s\n", cRed, applyErr, cReset)
		return
	}
	if err != nil {
		fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
		return
	}
	// Apply live.
	prevCtx := s.contextLimit()
	s.settings, s.warns = effectiveSettings(s.workdir)
	switch key {
	case "model":
		s.cmdModel(val)
	case "mode":
		s.mode = s.settings.Mode
	case "show_thinking":
		s.showThinking = boolOr(s.settings.ShowThinking, true)
		s.ui.showThinking = s.showThinking
	case "media_tools":
		s.media = boolOr(s.settings.MediaTools, false)
	case "mcp":
		s.mcpOn = boolOr(s.settings.MCP, false)
	case "context_tokens":
		if s.local != nil && s.contextLimit() != prevCtx {
			s.runner = nil // reload with the new window on the next prompt
		}
	}
	fmt.Printf("  %sSet %s = %s (%s)%s\n", cGray, key, val, map[bool]string{true: "project", false: "global"}[project], cReset)
}

// ── /context, /status ────────────────────────────────────────────────────────

func (s *codeSession) printContext() {
	lim := s.contextLimit()
	sys := estimateTokens(len(s.systemPrompt()))
	tools := estimateTokens(s.toolsChars)
	msgs := 0
	for _, m := range s.messages {
		msgs += len(m.Content)
	}
	msgT := estimateTokens(msgs)
	used := sys + tools + msgT
	pct := used * 100 / lim
	barW := 40
	fill := min(barW, pct*barW/100)
	col := cGreen
	if pct >= 75 {
		col = cYell
	}
	fmt.Printf("\n  %sContext%s  %s%s%s%s  ~%s / %s tokens (%d%%)\n", cBold, cReset, col, strings.Repeat("█", fill), cGray+strings.Repeat("░", barW-fill), cReset, humanCount(used), humanCount(lim), pct)
	fmt.Printf("  %ssystem prompt + instructions  ~%s%s\n", cGray, humanCount(sys), cReset)
	fmt.Printf("  %stool definitions              ~%s%s\n", cGray, humanCount(tools), cReset)
	fmt.Printf("  %sconversation (%d messages)     ~%s%s\n", cGray, len(s.messages), humanCount(msgT), cReset)
	if s.summary != "" {
		fmt.Printf("  %s(includes a compacted summary of earlier turns)%s\n", cGray, cReset)
	}
	if s.local != nil {
		fmt.Printf("  %sLocal window: context_tokens=%d (change with /config set context_tokens N)%s\n", cGray, lim, cReset)
	}
}

func (s *codeSession) printStatus() {
	kv := func(k, v string) { fmt.Printf("  %s%-14s%s %s\n", cGray, k, cReset, v) }
	fmt.Println()
	kv("version", version.Version)
	kv("session", s.sess.ID)
	kv("directory", s.workdir)
	if b, dirty := gitState(s.workdir); b != "" {
		kv("git", fmt.Sprintf("%s (%d changed files)", b, dirty))
	}
	kv("model", s.modelLabel())
	if s.local != nil {
		kv("backend", s.hw.String())
	}
	kv("mode", modeLabel(s.mode))
	kv("reasoning", onOff(s.showThinking))
	kv("mcp / media", onOff(s.mcpOn)+" / "+onOff(s.media))
	kv("skills", strings.Join(s.skills, ", "))
	var names []string
	for _, f := range s.instr {
		names = append(names, shortPath(f.Path, s.workdir))
	}
	kv("instructions", strings.Join(names, ", "))
	kv("settings", globalSettingsPath()+" + "+shortPath(projectSettingsPath(s.workdir), s.workdir))
	kv("messages", strconv.Itoa(len(s.messages)))
}

// ── /skills ──────────────────────────────────────────────────────────────────

func (s *codeSession) chooseSkills() {
	all := server.ListSkillInfos()
	if len(all) == 0 {
		fmt.Printf("  %sNo skills.%s\n", cGray, cReset)
		return
	}
	sel := 0
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
		sel = selectList(s.t, "Skills (enter toggles · esc closes)", items, sel)
		if sel < 0 {
			return
		}
		id := all[sel].ID
		if on[id] {
			s.skills = without(s.skills, id)
		} else {
			s.skills = append(s.skills, id)
		}
	}
}

// ── /diff ────────────────────────────────────────────────────────────────────

func (s *codeSession) showDiff() {
	if b, _ := gitState(s.workdir); b == "" {
		// Not a git repo: show this session's changes from the undo checkpoints.
		if len(s.undo) == 0 {
			fmt.Printf("  %sNot a git repository and no changes in this session.%s\n", cGray, cReset)
			return
		}
		seen := map[string]bool{}
		for _, turn := range s.undo {
			for _, cp := range turn {
				if seen[cp.path] {
					continue
				}
				seen[cp.path] = true
				now, _ := os.ReadFile(cp.path)
				rel, _ := filepath.Rel(s.workdir, cp.path)
				d, add, del := server.UnifiedDiff(filepath.ToSlash(rel), string(cp.before), string(now))
				fmt.Printf("%s●%s %s %s(+%d -%d)%s\n", cGreen, cReset, rel, cGray, add, del, cReset)
				s.ui.renderDiff(d, 200)
			}
		}
		return
	}
	stat, _ := exec.Command("git", "-C", s.workdir, "diff", "--stat", "HEAD").CombinedOutput()
	if strings.TrimSpace(string(stat)) == "" {
		fmt.Printf("  %sNo uncommitted changes.%s\n", cGray, cReset)
		return
	}
	fmt.Println(strings.TrimRight(string(stat), "\n"))
	out, _ := exec.Command("git", "-C", s.workdir, "diff", "HEAD").CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
			fmt.Println(cBold + l + cReset)
		case strings.HasPrefix(l, "+"):
			fmt.Println(cAddBg + l + cReset)
		case strings.HasPrefix(l, "-"):
			fmt.Println(cDelBg + l + cReset)
		case strings.HasPrefix(l, "@@"):
			fmt.Println(cAccent + l + cReset)
		default:
			fmt.Println(l)
		}
	}
}

// ── /export ──────────────────────────────────────────────────────────────────

func (s *codeSession) export(path string) {
	if path == "" {
		path = "vortelio-session-" + time.Now().Format("20060102-150405") + ".md"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.workdir, path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Vortelio Code session %s\n\n- Directory: %s\n- Model: %s\n\n", s.sess.ID, s.workdir, s.modelLabel())
	if s.summary != "" {
		b.WriteString("## Earlier (summary)\n\n" + s.summary + "\n\n")
	}
	for _, m := range s.messages {
		role := "User"
		if m.Role == "assistant" {
			role = "Vortelio"
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", role, m.Content)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Printf("  %s✗ %v%s\n", cRed, err, cReset)
		return
	}
	fmt.Printf("  %sExported to %s%s\n", cGray, path, cReset)
}

// replayTail prints the last messages of a resumed session.
func (s *codeSession) replayTail(n int) {
	start := len(s.messages) - n
	if start < 0 {
		start = 0
	}
	for _, m := range s.messages[start:] {
		if m.Role == "user" {
			fmt.Printf("%s> %s%s\n", cGray, oneLine(m.Content, termWidth()-4), cReset)
		} else {
			fmt.Printf("● %s\n", oneLine(m.Content, termWidth()-4))
		}
	}
}

// ── /init ────────────────────────────────────────────────────────────────────

func initPrompt(workdir string) string {
	existing := ""
	for _, n := range []string{"AGENTS.md", "CLAUDE.md", "VORTELIO.md"} {
		if _, err := os.Stat(filepath.Join(workdir, n)); err == nil {
			existing = n
			break
		}
	}
	var b strings.Builder
	if existing == "AGENTS.md" {
		b.WriteString("Update AGENTS.md in the working directory. Read it first, compare it with the current state of the project and fix only what is outdated or missing; keep hand-written notes.\n\n")
	} else {
		b.WriteString("Create an AGENTS.md file in the working directory: the instructions that coding agents (you included) will read at the start of every session in this project.\n\n")
		if existing != "" {
			b.WriteString("An existing " + existing + " contains project instructions: read it and carry over what is still valid.\n\n")
		}
	}
	b.WriteString(`First explore the project with the tools: list the top level, read the README, the dependency manifests (go.mod, package.json, pyproject.toml, Cargo.toml…), the build/CI config and the main entry points. Do not invent anything.

Write a concise AGENTS.md (about 20-60 lines) with:
1. What the project is (1-2 sentences) and its stack.
2. Commands: install, build, run, test (single test too), lint/format — exactly as they work here.
3. Layout: the main directories and what lives where.
4. Code style and conventions actually used (naming, error handling, formatting, comments).
5. Gotchas: anything non-obvious an agent must know (generated files, env vars, platform quirks).

Skip generic advice. Then write the file with write_file (or edit_file when updating) and reply with a one-line summary.`)
	return b.String()
}
