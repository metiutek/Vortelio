package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ANSI styles.
const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cItal   = "\033[3m"
	cBold   = "\033[1m"
	cCyan   = "\033[36m"
	cGreen  = "\033[32m"
	cYell   = "\033[33m"
	cRed    = "\033[31m"
	cMag    = "\033[35m"
	cBlue   = "\033[34m"
	cInv    = "\033[7m"
	cGray   = "\033[90m"
	cCode   = "\033[38;5;180m"
	cAccent = "\033[38;5;111m"
	cAddBg  = "\033[38;5;114m"
	cDelBg  = "\033[38;5;174m"
)

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// codeUI renders one agent turn: streamed answer (light Markdown), reasoning,
// tool calls/results, diffs, todo lists and a live status line. All writes are
// serialized through mu so the spinner never interleaves with content.
type codeUI struct {
	mu           sync.Mutex
	out          io.Writer // answer text
	log          io.Writer // tool activity (stderr in print mode)
	color        bool
	showThinking bool
	quietTools   bool // print mode: one line per tool
	plain        bool // print mode: answer text written raw (no styling)

	// turn state
	start       time.Time
	outBytes    int
	phase       string
	spin        int
	atLineStart bool
	statusShown bool
	spinnerOn   bool
	stopSpin    chan struct{}
	inThink     bool
	inAnswer    bool
	md          mdStream
	diffShown   map[string]bool
	lastTool    string
	lastArg     string
	toolLog     []string // compact log of the turn's actions (kept in history)
}

func newCodeUI(out, log io.Writer, color, showThinking bool) *codeUI {
	return &codeUI{out: out, log: log, color: color, showThinking: showThinking, atLineStart: true}
}

func (u *codeUI) c(style string) string {
	if !u.color {
		return ""
	}
	return style
}

// ── turn lifecycle ───────────────────────────────────────────────────────────

func (u *codeUI) beginTurn(spinner bool) {
	u.mu.Lock()
	u.start = time.Now()
	u.outBytes = 0
	u.phase = "Thinking"
	u.atLineStart = true
	u.inThink, u.inAnswer = false, false
	u.md = mdStream{}
	u.diffShown = map[string]bool{}
	u.toolLog = nil
	u.spinnerOn = spinner && u.color
	u.mu.Unlock()
	if u.spinnerOn {
		u.stopSpin = make(chan struct{})
		go func(stop chan struct{}) {
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					u.mu.Lock()
					u.drawStatus()
					u.mu.Unlock()
				}
			}
		}(u.stopSpin)
	}
}

func (u *codeUI) endTurn() {
	if u.stopSpin != nil {
		close(u.stopSpin)
		u.stopSpin = nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	u.closeThink()
	u.flushMD()
	if !u.atLineStart {
		fmt.Fprint(u.out, "\n")
		u.atLineStart = true
	}
	u.spinnerOn = false
}

func (u *codeUI) elapsed() time.Duration { return time.Since(u.start) }

// tokens is an estimate of generated tokens (bytes/4).
func (u *codeUI) tokens() int { return u.outBytes / 4 }

func (u *codeUI) drawStatus() {
	if !u.spinnerOn || !u.atLineStart {
		return
	}
	frame := spinFrames[u.spin%len(spinFrames)]
	u.spin++
	secs := int(time.Since(u.start).Seconds())
	fmt.Fprintf(u.out, "\r\033[K%s%s %s…%s %s(%ds · ↓ %s tokens · esc to interrupt)%s",
		u.c(cAccent), frame, u.phase, u.c(cReset), u.c(cGray), secs, humanCount(u.tokens()), u.c(cReset))
	u.statusShown = true
}

func (u *codeUI) eraseStatus() {
	if u.statusShown {
		fmt.Fprint(u.out, "\r\033[K")
		u.statusShown = false
	}
}

// pause hides the status line while something else (a prompt) owns the screen.
func (u *codeUI) pause() {
	u.mu.Lock()
	u.eraseStatus()
	u.closeThink()
	u.flushMD()
	if !u.atLineStart {
		fmt.Fprint(u.out, "\n")
		u.atLineStart = true
	}
	u.spinnerOn = false
	u.mu.Unlock()
}

func (u *codeUI) resume() {
	u.mu.Lock()
	u.spinnerOn = u.stopSpin != nil && u.color
	u.mu.Unlock()
}

func humanCount(n int) string {
	if n >= 1000 {
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	}
	return strconv.Itoa(n)
}

// ── streamed text ────────────────────────────────────────────────────────────

func (u *codeUI) thinking(tok string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.outBytes += len(tok)
	u.phase = "Thinking"
	if !u.showThinking {
		return
	}
	u.eraseStatus()
	if !u.inThink {
		u.flushMD()
		if !u.atLineStart {
			fmt.Fprint(u.out, "\n")
		}
		fmt.Fprintf(u.out, "%s✻ Thinking…%s\n%s%s  ", u.c(cGray+cItal), u.c(cReset), u.c(cGray), u.c(cItal))
		u.inThink = true
		u.atLineStart = false
	}
	tok = strings.ReplaceAll(tok, "\r", "")
	tok = strings.ReplaceAll(tok, "\n", "\n  ")
	fmt.Fprint(u.out, tok)
	u.atLineStart = false
}

func (u *codeUI) closeThink() {
	if u.inThink {
		fmt.Fprint(u.out, u.c(cReset)+"\n")
		u.inThink = false
		u.atLineStart = true
	}
}

func (u *codeUI) content(tok string) {
	if tok == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.outBytes += len(tok)
	u.phase = "Writing"
	if u.plain {
		fmt.Fprint(u.out, tok)
		return
	}
	u.eraseStatus()
	u.closeThink()
	if !u.inAnswer {
		if !u.atLineStart {
			fmt.Fprint(u.out, "\n")
		}
		fmt.Fprint(u.out, u.c(cReset)+"● ")
		u.inAnswer = true
		u.md.lineStart = true
		u.md.indent = "  "
	}
	u.md.color = u.color
	s := u.md.feed(tok)
	fmt.Fprint(u.out, s)
	u.atLineStart = strings.HasSuffix(s, "\n") || (s == "" && u.atLineStart)
}

func (u *codeUI) flushMD() {
	if s := u.md.flush(); s != "" {
		fmt.Fprint(u.out, s)
		u.atLineStart = strings.HasSuffix(s, "\n")
	}
	if u.inAnswer {
		fmt.Fprint(u.out, u.c(cReset))
	}
}

// endAnswerBlock closes the current answer paragraph (before a tool call).
func (u *codeUI) endAnswerBlock() {
	u.closeThink()
	u.flushMD()
	if !u.atLineStart {
		fmt.Fprint(u.out, "\n")
		u.atLineStart = true
	}
	u.inAnswer = false
}

// ── Markdown-lite streaming ──────────────────────────────────────────────────

// mdStream styles Markdown as it streams: fenced code blocks, headings, bold
// and inline code. It holds back only the few characters needed to decide.
type mdStream struct {
	color     bool
	lineStart bool
	pending   string // held at line start / after a lone marker
	inFence   bool
	inHead    bool
	bold      bool
	code      bool
	indent    string
}

func (m *mdStream) st(s string) string {
	if !m.color {
		return ""
	}
	return s
}

func (m *mdStream) feed(tok string) string {
	var b strings.Builder
	m.pending += tok
	for m.pending != "" {
		if m.lineStart {
			trim := strings.TrimLeft(m.pending, " \t")
			nl := strings.IndexByte(m.pending, '\n')
			// A line starting with ` or # may become a fence or a heading: wait
			// for a few more characters before deciding.
			if nl < 0 && len(trim) < 3 && (trim == "" || trim[0] == '`' || trim[0] == '#') {
				break
			}
			m.lineStart = false
			if strings.HasPrefix(trim, "```") {
				// Fence line: print it whole (dim) once complete.
				if nl < 0 {
					m.lineStart = true
					break
				}
				line := m.pending[:nl]
				m.pending = m.pending[nl+1:]
				m.inFence = !m.inFence
				b.WriteString(m.st(cGray) + strings.TrimSpace(line) + m.st(cReset) + "\n" + m.indent)
				m.lineStart = true
				continue
			}
			if m.inFence {
				b.WriteString(m.st(cCode))
			} else if strings.HasPrefix(trim, "#") {
				m.inHead = true
				m.pending = strings.TrimLeft(trim, "#")
				m.pending = strings.TrimLeft(m.pending, " ")
				b.WriteString(m.st(cBold + cAccent))
			}
		}
		r, size := utf8.DecodeRuneInString(m.pending)
		if r == utf8.RuneError && size <= 1 && len(m.pending) < 4 && !utf8.FullRuneInString(m.pending) {
			break // incomplete UTF-8 sequence
		}
		switch {
		case r == '\n':
			b.WriteString(m.st(cReset) + "\n" + m.indent)
			m.inHead, m.bold, m.code = false, false, false
			m.lineStart = true
			m.pending = m.pending[size:]
			continue
		case m.inFence:
			b.WriteString(string(r))
		case r == '`':
			m.code = !m.code
			if m.code {
				b.WriteString(m.st(cCode))
			} else {
				b.WriteString(m.st(cReset) + m.restore())
			}
		case r == '*' && len(m.pending) < 2:
			return b.String() // need the next char to tell ** from *
		case r == '*' && m.pending[1] == '*' && !m.code:
			m.bold = !m.bold
			if m.bold {
				b.WriteString(m.st(cBold))
			} else {
				b.WriteString(m.st(cReset) + m.restore())
			}
			m.pending = m.pending[2:]
			continue
		default:
			b.WriteString(string(r))
		}
		m.pending = m.pending[size:]
	}
	return b.String()
}

func (m *mdStream) restore() string {
	s := ""
	if m.inHead {
		s += m.st(cBold + cAccent)
	}
	if m.bold {
		s += m.st(cBold)
	}
	return s
}

func (m *mdStream) flush() string {
	s := m.pending
	m.pending = ""
	if s == "" {
		return ""
	}
	return s + m.st(cReset)
}

// ── tools ────────────────────────────────────────────────────────────────────

// toolTitle renders a tool call as Name(arg) like other coding agents.
func toolTitle(name, argsJSON string) (string, string) {
	var a map[string]interface{}
	json.Unmarshal([]byte(argsJSON), &a)
	str := func(k string) string {
		if v, ok := a[k]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	switch name {
	case "read_file":
		arg := str("path")
		if off := str("offset"); off != "" && off != "0" {
			arg += ":" + off
		}
		return "Read", arg
	case "write_file":
		return "Write", str("path")
	case "edit_file":
		return "Update", str("path")
	case "list_directory":
		p := str("path")
		if p == "" {
			p = "."
		}
		return "List", p
	case "glob_search":
		return "Glob", str("pattern")
	case "grep_search":
		p := str("pattern")
		if p == "" {
			p = str("query")
		}
		return "Grep", p
	case "run_shell":
		return "Bash", str("command")
	case "web_search":
		return "Search", str("query")
	case "fetch_url":
		return "Fetch", str("url")
	case "todo_write":
		return "Todos", ""
	case "ask_user":
		return "Ask", str("question")
	}
	return name, compactArgs(a)
}

func compactArgs(a map[string]interface{}) string {
	var parts []string
	for k, v := range a {
		s := fmt.Sprint(v)
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > max {
		rs := []rune(s)
		return string(rs[:max]) + "…"
	}
	return s
}

func (u *codeUI) toolCall(name, args string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	u.endAnswerBlock()
	title, arg := toolTitle(name, args)
	u.phase = "Running " + title
	u.lastTool = name
	u.lastArg = oneLine(arg, 80)
	if name == "todo_write" || name == "ask_user" {
		return // rendered by their own events/prompts
	}
	w := termWidth() - len(title) - 6
	if w < 20 {
		w = 20
	}
	if arg == "" {
		fmt.Fprintf(u.log, "%s●%s %s%s%s\n", u.c(cGreen), u.c(cReset), u.c(cBold), title, u.c(cReset))
	} else {
		fmt.Fprintf(u.log, "%s●%s %s%s%s(%s)\n", u.c(cGreen), u.c(cReset), u.c(cBold), title, u.c(cReset), oneLine(arg, w))
	}
	u.atLineStart = true
	u.outBytes += len(args)
}

var exitCodeRE = regexp.MustCompile(`\n?\[exit code (-?\d+)\]$`)

func (u *codeUI) toolResult(name, result, errStr string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	title, _ := toolTitle(name, "{}")
	u.phase = "Thinking"
	if errStr != "" {
		u.logAction(name, "error: "+oneLine(errStr, 80))
		fmt.Fprintf(u.log, "  %s⎿  %s%s\n", u.c(cRed), oneLine(errStr, termWidth()-8), u.c(cReset))
		return
	}
	var summary string
	var body []string
	switch name {
	case "read_file":
		n := strings.Count(result, "\n")
		if strings.Contains(result, "(lines ") {
			n--
		}
		summary = fmt.Sprintf("Read %d lines", n)
	case "write_file", "edit_file":
		summary = oneLine(result, 100)
	case "list_directory":
		var r struct {
			Entries []string `json:"entries"`
		}
		json.Unmarshal([]byte(result), &r)
		summary = fmt.Sprintf("Listed %d entries", len(r.Entries))
	case "glob_search":
		if result == "No files found" {
			summary = result
		} else {
			summary = fmt.Sprintf("Found %d files", strings.Count(result, "\n")+1)
		}
	case "grep_search":
		if result == "No matches found" {
			summary = result
		} else {
			summary = fmt.Sprintf("Found %d matches", strings.Count(result, "\n")+1)
		}
	case "run_shell":
		code := "0"
		if m := exitCodeRE.FindStringSubmatch(result); m != nil {
			code = m[1]
			result = exitCodeRE.ReplaceAllString(result, "")
		}
		lines := strings.Split(strings.TrimRight(result, "\n"), "\n")
		max := 6
		for i, l := range lines {
			if i >= max {
				body = append(body, fmt.Sprintf("… +%d lines", len(lines)-max))
				break
			}
			body = append(body, oneLine(l, termWidth()-8))
		}
		if code != "0" {
			summary = "exit code " + code
		}
	case "todo_write", "ask_user":
		return
	default:
		summary = oneLine(result, 100)
	}
	u.logAction(name, summary)
	if u.quietTools && name != "run_shell" {
		return
	}
	prefix := "  ⎿  "
	if summary != "" {
		col := u.c(cGray)
		if name == "run_shell" {
			col = u.c(cRed)
		}
		fmt.Fprintf(u.log, "%s%s%s%s\n", prefix, col, summary, u.c(cReset))
		prefix = "     "
	}
	for _, l := range body {
		fmt.Fprintf(u.log, "%s%s%s%s\n", prefix, u.c(cGray), l, u.c(cReset))
		prefix = "     "
	}
	_ = title
}

// logAction records a compact line for the conversation history, so the model
// remembers what it already did in earlier turns without replaying outputs.
func (u *codeUI) logAction(name, summary string) {
	entry := name
	if name == u.lastTool && u.lastArg != "" {
		entry += "(" + u.lastArg + ")"
	}
	if summary != "" {
		entry += " → " + summary
	}
	u.toolLog = append(u.toolLog, entry)
}

func (u *codeUI) fileDiff(path, diff string, add, del int, created bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	if u.quietTools {
		return
	}
	if u.diffShown[path] {
		delete(u.diffShown, path)
		return
	}
	u.renderDiff(diff, 30)
}

// renderDiff prints a unified diff with line numbers and colors (max lines).
func (u *codeUI) renderDiff(diff string, max int) {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	oldNo, newNo := 0, 0
	shown := 0
	hunkRE := regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	for i, l := range lines {
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			continue
		}
		if m := hunkRE.FindStringSubmatch(l); m != nil {
			oldNo, _ = strconv.Atoi(m[1])
			newNo, _ = strconv.Atoi(m[2])
			if shown > 0 {
				fmt.Fprintf(u.log, "     %s⋯%s\n", u.c(cGray), u.c(cReset))
			}
			continue
		}
		if shown >= max {
			rest := 0
			for _, x := range lines[i:] {
				if strings.HasPrefix(x, "+") || strings.HasPrefix(x, "-") {
					rest++
				}
			}
			if rest > 0 {
				fmt.Fprintf(u.log, "     %s… +%d changed lines%s\n", u.c(cGray), rest, u.c(cReset))
			}
			return
		}
		if l == "" {
			continue
		}
		text := oneLine(l[1:], termWidth()-14)
		switch l[0] {
		case '+':
			fmt.Fprintf(u.log, "     %s%4d +%s %s%s\n", u.c(cAddBg), newNo, "", text, u.c(cReset))
			newNo++
		case '-':
			fmt.Fprintf(u.log, "     %s%4d -%s %s%s\n", u.c(cDelBg), oldNo, "", text, u.c(cReset))
			oldNo++
		default:
			fmt.Fprintf(u.log, "     %s%4d  %s %s\n", u.c(cGray), newNo, u.c(cReset), text)
			oldNo++
			newNo++
		}
		shown++
	}
}

type todoView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

func (u *codeUI) todos(list []todoView) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	u.endAnswerBlock()
	fmt.Fprintf(u.log, "%s●%s %sUpdate Todos%s\n", u.c(cGreen), u.c(cReset), u.c(cBold), u.c(cReset))
	for i, t := range list {
		prefix := "     "
		if i == 0 {
			prefix = "  ⎿  "
		}
		switch t.Status {
		case "completed":
			fmt.Fprintf(u.log, "%s%s☒ %s%s\n", prefix, u.c(cGray), t.Content, u.c(cReset))
		case "in_progress":
			fmt.Fprintf(u.log, "%s%s☐ %s%s\n", prefix, u.c(cBold+cAccent), t.Content, u.c(cReset))
		default:
			fmt.Fprintf(u.log, "%s☐ %s\n", prefix, t.Content)
		}
	}
	u.atLineStart = true
}

func (u *codeUI) mediaGenerated(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	fmt.Fprintf(u.log, "  ⎿  %s🎨 %s%s\n", u.c(cMag), path, u.c(cReset))
}

// println prints a line outside of a turn (or between streamed blocks).
func (u *codeUI) println(format string, a ...interface{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.eraseStatus()
	if !u.atLineStart {
		fmt.Fprint(u.out, "\n")
		u.atLineStart = true
	}
	fmt.Fprintf(u.log, format+"\n", a...)
}

// ── pickers & prompts ────────────────────────────────────────────────────────

// selectList shows an arrow-navigable list; returns the index or -1 (Esc).
func selectList(t *terminal, title string, items []string, start int) int {
	if len(items) == 0 {
		return -1
	}
	idx := start
	if idx < 0 || idx >= len(items) {
		idx = 0
	}
	prev := 0
	draw := func() {
		maxRows := termHeight() - 5
		if maxRows < 3 {
			maxRows = 3
		}
		s, e := windowBounds(idx, len(items), maxRows)
		var lines []string
		lines = append(lines, cBold+title+cReset)
		if s > 0 {
			lines = append(lines, cGray+"   ↑ "+strconv.Itoa(s)+" more"+cReset)
		}
		for i := s; i < e; i++ {
			if i == idx {
				lines = append(lines, cAccent+" ❯ "+items[i]+cReset)
			} else {
				lines = append(lines, "   "+items[i])
			}
		}
		if e < len(items) {
			lines = append(lines, cGray+"   ↓ "+strconv.Itoa(len(items)-e)+" more"+cReset)
		}
		lines = append(lines, cGray+"   ↑↓ select · enter confirm · esc cancel"+cReset)
		if t.tty {
			if prev > 0 {
				fmt.Printf("\033[%dA", prev-1)
			}
			fmt.Print("\r\033[J")
		}
		fmt.Print(strings.Join(lines, "\n"))
		prev = len(lines)
	}
	if t.tty {
		fmt.Print("\033[?25l")
		defer fmt.Print("\033[?25h")
	}
	draw()
	for {
		k := t.ReadKey()
		switch k.kind {
		case kEOF:
			fmt.Print("\n")
			return -1
		case kEsc, kCtrlC:
			fmt.Print("\n")
			return -1
		case kEnter:
			fmt.Print("\n")
			return idx
		case kUp:
			if idx > 0 {
				idx--
			} else {
				idx = len(items) - 1
			}
		case kDown, kTab:
			if idx < len(items)-1 {
				idx++
			} else {
				idx = 0
			}
		case kRune:
			if k.r >= '1' && k.r <= '9' && int(k.r-'1') < len(items) {
				idx = int(k.r - '1')
				draw()
				fmt.Print("\n")
				return idx
			}
			if k.r == 'k' && idx > 0 {
				idx--
			} else if k.r == 'j' && idx < len(items)-1 {
				idx++
			}
		}
		draw()
	}
}

// promptLine reads one line of free text with basic editing.
func promptLine(t *terminal, prompt string) (string, bool) {
	fmt.Print(prompt)
	var buf []rune
	for {
		k := t.ReadKey()
		switch k.kind {
		case kEOF, kEsc, kCtrlC:
			fmt.Print("\n")
			return "", false
		case kEnter:
			fmt.Print("\n")
			return strings.TrimSpace(string(buf)), true
		case kBackspace:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				if t.tty {
					fmt.Print("\b \b")
				}
			}
		case kPaste:
			s := strings.ReplaceAll(k.text, "\n", " ")
			buf = append(buf, []rune(s)...)
			if t.tty {
				fmt.Print(s)
			}
		case kRune:
			if k.r != 0 {
				buf = append(buf, k.r)
				if t.tty {
					fmt.Print(string(k.r))
				}
			}
		}
	}
}

func windowBounds(idx, total, max int) (int, int) {
	if max <= 0 || total <= max {
		return 0, total
	}
	start := idx - max/2
	if start < 0 {
		start = 0
	}
	if start+max > total {
		start = total - max
	}
	return start, start + max
}

// stdoutIsColor reports whether ANSI styling should be used on stdout.
func stdoutIsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTTY(os.Stdout)
}

// promptSecret is promptLine that echoes • instead of the typed characters.
func promptSecret(t *terminal, prompt string) (string, bool) {
	fmt.Print(prompt)
	var buf []rune
	for {
		k := t.ReadKey()
		switch k.kind {
		case kEOF, kEsc, kCtrlC:
			fmt.Print("\n")
			return "", false
		case kEnter:
			fmt.Print("\n")
			return strings.TrimSpace(string(buf)), true
		case kBackspace:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				if t.tty {
					fmt.Print("\b \b")
				}
			}
		case kPaste:
			s := strings.ReplaceAll(strings.ReplaceAll(k.text, "\r", ""), "\n", "")
			buf = append(buf, []rune(s)...)
			if t.tty {
				fmt.Print(strings.Repeat("•", len([]rune(s))))
			}
		case kRune:
			if k.r != 0 {
				buf = append(buf, k.r)
				if t.tty {
					fmt.Print("•")
				}
			}
		}
	}
}

// searchList is selectList with type-to-filter, for long lists (e.g. the 300+
// models OpenRouter serves). Returns the index into items, or -1.
func searchList(t *terminal, title string, items []string, start int) int {
	if len(items) == 0 {
		return -1
	}
	plain := make([]string, len(items))
	for i, it := range items {
		plain[i] = strings.ToLower(stripANSI(it))
	}
	var query []rune
	var view []int // indexes into items matching query
	filter := func() {
		view = view[:0]
		q := strings.ToLower(string(query))
		for i := range items {
			if q == "" || strings.Contains(plain[i], q) {
				view = append(view, i)
			}
		}
	}
	filter()
	idx := 0
	for vi, i := range view {
		if i == start {
			idx = vi
		}
	}
	prev := 0
	draw := func() {
		maxRows := termHeight() - 6
		if maxRows < 3 {
			maxRows = 3
		}
		s, e := windowBounds(idx, len(view), maxRows)
		lines := []string{cBold + title + cReset + cGray + "  filter: " + cReset + string(query) + cGray + "▏" + cReset}
		if s > 0 {
			lines = append(lines, cGray+"   ↑ "+strconv.Itoa(s)+" more"+cReset)
		}
		for vi := s; vi < e; vi++ {
			if vi == idx {
				lines = append(lines, cAccent+" ❯ "+items[view[vi]]+cReset)
			} else {
				lines = append(lines, "   "+items[view[vi]])
			}
		}
		if len(view) == 0 {
			lines = append(lines, cGray+"   (no match)"+cReset)
		}
		if e < len(view) {
			lines = append(lines, cGray+"   ↓ "+strconv.Itoa(len(view)-e)+" more"+cReset)
		}
		lines = append(lines, cGray+"   type to filter · ↑↓ select · enter confirm · esc cancel"+cReset)
		if t.tty {
			if prev > 0 {
				fmt.Printf("\033[%dA", prev-1)
			}
			fmt.Print("\r\033[J")
		}
		fmt.Print(strings.Join(lines, "\n"))
		prev = len(lines)
	}
	if t.tty {
		fmt.Print("\033[?25l")
		defer fmt.Print("\033[?25h")
	}
	draw()
	for {
		k := t.ReadKey()
		switch k.kind {
		case kEOF, kEsc, kCtrlC:
			fmt.Print("\n")
			return -1
		case kEnter:
			fmt.Print("\n")
			if len(view) == 0 {
				return -1
			}
			return view[idx]
		case kUp:
			if idx > 0 {
				idx--
			} else if len(view) > 0 {
				idx = len(view) - 1
			}
		case kDown, kTab:
			if idx < len(view)-1 {
				idx++
			} else {
				idx = 0
			}
		case kBackspace:
			if len(query) > 0 {
				query = query[:len(query)-1]
				filter()
				idx = 0
			}
		case kPaste:
			query = append(query, []rune(strings.TrimSpace(k.text))...)
			filter()
			idx = 0
		case kRune:
			if k.r != 0 {
				query = append(query, k.r)
				filter()
				idx = 0
			}
		}
		draw()
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
