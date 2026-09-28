package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// ── Prompt editor ────────────────────────────────────────────────────────────
//
//   ╭──────────────────────────────────────────╮
//   │ > fix the failing test in parser_test.go │
//   ╰──────────────────────────────────────────╯
//     ⏵⏵ ask · qwen3.5:4b · ctx 12%        ? for shortcuts
//
// Enter submits, Ctrl+J / Alt+Enter / "\"+Enter insert a line break, ↑/↓
// browse history, Tab completes /commands and @files, Shift+Tab cycles the
// permission mode, Esc clears, Ctrl+C clears (twice on empty exits), Ctrl+D
// on empty exits, Ctrl+L clears the screen, Ctrl+O toggles reasoning display.

type editorResult int

const (
	edSubmit editorResult = iota
	edExit
)

type suggestion struct {
	insert string // text that replaces the token
	label  string
	desc   string
}

type lineEditor struct {
	t       *terminal
	history []string

	// callbacks
	status    func() string                        // footer left part
	complete  func(buf string) (int, []suggestion) // token start (rune index) + items
	cycleMode func()
	toggleThk func() string // returns a notice

	buf       []rune
	cur       int
	histPos   int
	stash     []rune
	sel       int
	menuOff   bool
	prevLines int
	curLine   int // rendered line index of the cursor row (for redraw)
	ctrlC     bool
	notice    string
	showHelp  bool
}

func (e *lineEditor) read() (string, editorResult) {
	e.buf, e.cur, e.sel = nil, 0, 0
	e.histPos = len(e.history)
	e.stash = nil
	e.menuOff = false
	e.prevLines = 0
	e.ctrlC = false
	e.showHelp = false
	if !e.t.tty {
		return e.readPlain()
	}
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")
	e.render()
	for {
		k := e.t.ReadKey()
		if k.kind != kCtrlC {
			e.ctrlC = false
		}
		e.notice = ""
		switch k.kind {
		case kEOF:
			e.clear()
			return "", edExit
		case kCtrlD:
			if len(e.buf) == 0 {
				e.clear()
				return "", edExit
			}
			e.deleteAt(e.cur)
		case kCtrlC:
			if len(e.buf) > 0 {
				e.buf, e.cur = nil, 0
			} else if e.ctrlC {
				e.clear()
				return "", edExit
			} else {
				e.ctrlC = true
				e.notice = "Press Ctrl+C again to exit"
			}
		case kEsc:
			if start, items := e.menu(); start >= 0 && len(items) > 0 && !e.menuOff {
				e.menuOff = true
			} else if e.showHelp {
				e.showHelp = false
			} else {
				e.buf, e.cur = nil, 0
			}
		case kEnter:
			start, items := e.menu()
			if start >= 0 && len(items) > 0 && !e.menuOff {
				it := items[e.sel%len(items)]
				e.applySuggestion(start, it)
				// A /command is run right away; a file keeps editing.
				if strings.HasPrefix(it.insert, "/") {
					return e.submit()
				}
				break
			}
			if e.cur > 0 && e.buf[e.cur-1] == '\\' {
				e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
				e.cur--
				e.insert("\n")
				break
			}
			if strings.TrimSpace(string(e.buf)) == "" {
				break
			}
			return e.submit()
		case kNewline:
			e.insert("\n")
		case kTab:
			if start, items := e.menu(); start >= 0 && len(items) > 0 {
				e.applySuggestion(start, items[e.sel%len(items)])
			}
		case kShiftTab:
			if e.cycleMode != nil {
				e.cycleMode()
			}
		case kCtrlO:
			if e.toggleThk != nil {
				e.notice = e.toggleThk()
			}
		case kCtrlL:
			fmt.Print("\033[H\033[2J")
			e.prevLines = 0
		case kBackspace:
			if e.cur > 0 {
				e.cur--
				e.deleteAt(e.cur)
			}
		case kDelete:
			e.deleteAt(e.cur)
		case kCtrlU:
			ls := e.lineStart()
			e.buf = append(e.buf[:ls], e.buf[e.cur:]...)
			e.cur = ls
		case kCtrlK:
			le := e.lineEnd()
			e.buf = append(e.buf[:e.cur], e.buf[le:]...)
		case kCtrlW:
			i := e.cur
			for i > 0 && unicode.IsSpace(e.buf[i-1]) {
				i--
			}
			for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
				i--
			}
			e.buf = append(e.buf[:i], e.buf[e.cur:]...)
			e.cur = i
		case kLeft:
			if e.cur > 0 {
				e.cur--
			}
		case kRight:
			if e.cur < len(e.buf) {
				e.cur++
			}
		case kWordLeft:
			for e.cur > 0 && unicode.IsSpace(e.buf[e.cur-1]) {
				e.cur--
			}
			for e.cur > 0 && !unicode.IsSpace(e.buf[e.cur-1]) {
				e.cur--
			}
		case kWordRight:
			for e.cur < len(e.buf) && unicode.IsSpace(e.buf[e.cur]) {
				e.cur++
			}
			for e.cur < len(e.buf) && !unicode.IsSpace(e.buf[e.cur]) {
				e.cur++
			}
		case kHome:
			e.cur = e.lineStart()
		case kEnd:
			e.cur = e.lineEnd()
		case kUp:
			if start, items := e.menu(); start >= 0 && len(items) > 0 && !e.menuOff {
				e.sel = (e.sel - 1 + len(items)) % len(items)
			} else if e.lineStart() > 0 {
				e.moveVert(-1)
			} else {
				e.historyPrev()
			}
		case kDown:
			if start, items := e.menu(); start >= 0 && len(items) > 0 && !e.menuOff {
				e.sel = (e.sel + 1) % len(items)
			} else if e.lineEnd() < len(e.buf) {
				e.moveVert(1)
			} else {
				e.historyNext()
			}
		case kPaste:
			e.insert(k.text)
		case kRune:
			if k.r == 0 {
				break
			}
			if k.r == '?' && len(e.buf) == 0 {
				e.showHelp = !e.showHelp
				break
			}
			e.insert(string(k.r))
		}
		if k.kind != kUp && k.kind != kDown && k.kind != kEsc && k.kind != kTab {
			e.sel = 0
			if k.kind == kRune || k.kind == kBackspace || k.kind == kPaste {
				e.menuOff = false
			}
		}
		if e.t.pending() && k.kind != kEnter {
			continue // more keys queued: redraw once after the burst
		}
		e.render()
	}
}

// readPlain is used when stdin is not a terminal (pipes, tests).
func (e *lineEditor) readPlain() (string, editorResult) {
	for {
		k := e.t.ReadKey()
		switch k.kind {
		case kEOF:
			if len(e.buf) > 0 {
				return e.submitPlain()
			}
			return "", edExit
		case kEnter:
			if strings.TrimSpace(string(e.buf)) == "" {
				e.buf = nil
				continue
			}
			return e.submitPlain()
		case kNewline, kPaste:
			e.buf = append(e.buf, []rune(k.text+map[bool]string{true: "\n"}[k.kind == kNewline])...)
		case kBackspace:
			if len(e.buf) > 0 {
				e.buf = e.buf[:len(e.buf)-1]
			}
		case kRune:
			if k.r != 0 {
				e.buf = append(e.buf, k.r)
			}
		}
	}
}

func (e *lineEditor) submitPlain() (string, editorResult) {
	line := string(e.buf)
	e.buf = nil
	e.remember(line)
	return line, edSubmit
}

func (e *lineEditor) submit() (string, editorResult) {
	line := string(e.buf)
	e.clear()
	// Echo the prompt above the conversation, like other coding agents.
	lines := strings.Split(line, "\n")
	for i, l := range lines {
		p := "> "
		if i > 0 {
			p = "  "
		}
		fmt.Printf("%s%s%s%s\n", cGray, p, l, cReset)
	}
	e.remember(line)
	return line, edSubmit
}

func (e *lineEditor) remember(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n == 0 || e.history[n-1] != line {
		e.history = append(e.history, line)
	}
}

func (e *lineEditor) insert(s string) {
	rs := []rune(s)
	nb := make([]rune, 0, len(e.buf)+len(rs))
	nb = append(nb, e.buf[:e.cur]...)
	nb = append(nb, rs...)
	nb = append(nb, e.buf[e.cur:]...)
	e.buf = nb
	e.cur += len(rs)
}

func (e *lineEditor) deleteAt(i int) {
	if i >= 0 && i < len(e.buf) {
		e.buf = append(e.buf[:i], e.buf[i+1:]...)
	}
}

func (e *lineEditor) lineStart() int {
	i := e.cur
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

func (e *lineEditor) lineEnd() int {
	i := e.cur
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

func (e *lineEditor) moveVert(dir int) {
	col := e.cur - e.lineStart()
	if dir < 0 {
		prevEnd := e.lineStart() - 1
		prevStart := prevEnd
		for prevStart > 0 && e.buf[prevStart-1] != '\n' {
			prevStart--
		}
		e.cur = prevStart + min(col, prevEnd-prevStart)
	} else {
		nextStart := e.lineEnd() + 1
		nextEnd := nextStart
		for nextEnd < len(e.buf) && e.buf[nextEnd] != '\n' {
			nextEnd++
		}
		e.cur = nextStart + min(col, nextEnd-nextStart)
	}
}

func (e *lineEditor) historyPrev() {
	if e.histPos == 0 {
		return
	}
	if e.histPos == len(e.history) {
		e.stash = append([]rune(nil), e.buf...)
	}
	e.histPos--
	e.buf = []rune(e.history[e.histPos])
	e.cur = len(e.buf)
}

func (e *lineEditor) historyNext() {
	if e.histPos >= len(e.history) {
		return
	}
	e.histPos++
	if e.histPos == len(e.history) {
		e.buf = append([]rune(nil), e.stash...)
	} else {
		e.buf = []rune(e.history[e.histPos])
	}
	e.cur = len(e.buf)
}

func (e *lineEditor) menu() (int, []suggestion) {
	if e.complete == nil || e.cur != len(e.buf) {
		return -1, nil
	}
	return e.complete(string(e.buf))
}

func (e *lineEditor) applySuggestion(start int, s suggestion) {
	e.buf = append(append([]rune{}, e.buf[:start]...), []rune(s.insert)...)
	e.cur = len(e.buf)
	e.sel = 0
}

func (e *lineEditor) clear() {
	if e.prevLines > 0 {
		if e.curLine > 0 {
			fmt.Printf("\033[%dA", e.curLine)
		}
		fmt.Print("\r\033[J")
	}
	e.prevLines = 0
	e.curLine = 0
}

// render redraws the prompt box, footer and completion menu in place.
func (e *lineEditor) render() {
	w := termWidth() - 2
	if w > 120 {
		w = 120
	}
	if w < 24 {
		w = 24
	}
	inner := w - 4 // "│ " + text + " │"

	border := cGray
	prompt := "> "
	text := string(e.buf)
	switch {
	case strings.HasPrefix(text, "!"):
		border, prompt = cMag, "! "
	case strings.HasPrefix(text, "#"):
		border, prompt = cBlue, "# "
	}

	// Wrap the buffer into rows, tracking where the cursor lands.
	type cell struct {
		r   rune
		cur bool
	}
	var rows [][]cell
	var row []cell
	width := inner - 2
	for i := 0; i <= len(e.buf); i++ {
		isCur := i == e.cur
		if i == len(e.buf) {
			row = append(row, cell{' ', isCur})
			break
		}
		r := e.buf[i]
		if r == '\n' {
			row = append(row, cell{' ', isCur})
			rows = append(rows, row)
			row = nil
			continue
		}
		row = append(row, cell{r, isCur})
		if len(row) >= width {
			rows = append(rows, row)
			row = nil
		}
	}
	rows = append(rows, row)

	var lines []string
	lines = append(lines, border+"╭"+strings.Repeat("─", w-2)+"╮"+cReset)
	for ri, rw := range rows {
		var b strings.Builder
		n := 0
		for _, c := range rw {
			if c.cur {
				b.WriteString(cInv + string(c.r) + cReset)
			} else {
				b.WriteRune(c.r)
			}
			n++
		}
		p := "  "
		if ri == 0 {
			p = prompt
		}
		pad := inner - 2 - n
		if pad < 0 {
			pad = 0
		}
		lines = append(lines, border+"│"+cReset+" "+p+b.String()+strings.Repeat(" ", pad)+border+"│"+cReset)
	}
	lines = append(lines, border+"╰"+strings.Repeat("─", w-2)+"╯"+cReset)

	footer := ""
	if e.status != nil {
		footer = e.status()
	}
	right := "? for shortcuts"
	if e.notice != "" {
		right = cYell + e.notice + cReset
	}
	lines = append(lines, "  "+footer+cGray+"   "+right+cReset)

	if e.showHelp {
		lines = append(lines, shortcutHelp()...)
	} else if start, items := e.menu(); start >= 0 && len(items) > 0 && !e.menuOff {
		max := termHeight() - len(lines) - 2
		if max > 10 {
			max = 10
		}
		if max < 3 {
			max = 3
		}
		sel := e.sel % len(items)
		s, en := windowBounds(sel, len(items), max)
		for i := s; i < en; i++ {
			it := items[i]
			label := fmt.Sprintf("%-22s", it.label)
			if i == sel {
				lines = append(lines, "  "+cAccent+"❯ "+label+cReset+" "+cGray+it.desc+cReset)
			} else {
				lines = append(lines, "    "+label+" "+cGray+it.desc+cReset)
			}
		}
		if en < len(items) {
			lines = append(lines, cGray+fmt.Sprintf("    … %d more", len(items)-en)+cReset)
		}
	}

	e.clear()
	fmt.Print(strings.Join(lines, "\n"))
	e.prevLines = len(lines)
	e.curLine = len(lines) - 1
}

func shortcutHelp() []string {
	g := func(k, d string) string { return fmt.Sprintf("  %s%-18s%s %s", cAccent, k, cReset, d) }
	return []string{
		g("/", "commands"),
		g("@path", "attach a file"),
		g("!command", "run a shell command"),
		g("# note", "save a note to AGENTS.md"),
		g("shift+tab", "switch mode (ask · edits · auto · plan)"),
		g("esc", "interrupt the agent / clear input"),
		g("ctrl+j, \\+enter", "new line"),
		g("↑ ↓", "history"),
		g("ctrl+o", "show/hide reasoning"),
		g("ctrl+l", "clear screen"),
		g("ctrl+c ×2, ctrl+d", "exit"),
	}
}

// ── completion ───────────────────────────────────────────────────────────────

// fileSuggestions completes the @path token at the end of line.
func fileSuggestions(workdir, line string) (int, []suggestion) {
	rs := []rune(line)
	i := len(rs)
	for i > 0 && !unicode.IsSpace(rs[i-1]) {
		i--
	}
	tok := string(rs[i:])
	if !strings.HasPrefix(tok, "@") {
		return -1, nil
	}
	frag := tok[1:]
	dir := workdir
	namePart := frag
	prefix := ""
	if d := strings.LastIndexAny(frag, "/\\"); d >= 0 {
		prefix = frag[:d+1]
		dir = filepath.Join(workdir, frag[:d])
		namePart = frag[d+1:]
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1, nil
	}
	low := strings.ToLower(namePart)
	var out []suggestion
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(namePart, ".") {
			continue
		}
		if e.IsDir() && isNoiseDirName(name) {
			continue
		}
		if low != "" && !strings.Contains(strings.ToLower(name), low) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		out = append(out, suggestion{insert: "@" + prefix + name, label: prefix + name})
	}
	sort.Slice(out, func(a, b int) bool {
		ap := strings.HasPrefix(strings.ToLower(out[a].label[len(prefix):]), low)
		bp := strings.HasPrefix(strings.ToLower(out[b].label[len(prefix):]), low)
		if ap != bp {
			return ap
		}
		return out[a].label < out[b].label
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return i, out
}

func isNoiseDirName(n string) bool {
	switch n {
	case "node_modules", "__pycache__", ".git", ".venv", "venv", "dist", "build", "target", ".next":
		return true
	}
	return false
}
