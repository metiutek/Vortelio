package commands

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// readLine reads a line of input with a framed box and live autocomplete for
// '/' commands and '@' file references. Returns (line, exitRequested).
func (s *codeSession) readLine() (string, bool) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Non-interactive (piped): plain read.
		in, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", true
		}
		return in, false
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		in, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return in, false
	}
	defer term.Restore(fd, old)

	r := bufio.NewReader(os.Stdin)
	var buf []rune
	suggIdx := 0
	prevLines := 0
	histPos := len(s.history) // == len → editing a fresh line
	var stash []rune          // in-progress line saved when browsing history

	render := func() {
		kind, frag, sugg := s.suggestions(string(buf))
		if suggIdx >= len(sugg) {
			suggIdx = 0
		}
		w := termWidth() - 4
		if w < 20 {
			w = 20
		}
		if w > 100 {
			w = 100
		}

		var lines []string
		lines = append(lines, cDim+"╭"+strings.Repeat("─", w)+"╮"+cReset)
		shown := string(buf)
		// keep the tail visible if too long
		if len([]rune(shown)) > w-4 {
			rs := []rune(shown)
			shown = "…" + string(rs[len(rs)-(w-5):])
		}
		pad := w - 3 - len([]rune(shown))
		if pad < 0 {
			pad = 0
		}
		lines = append(lines, cDim+"│"+cReset+" "+cCyan+"›"+cReset+" "+shown+cInv+" "+cReset+strings.Repeat(" ", pad)+cDim+"│"+cReset)
		lines = append(lines, cDim+"╰"+strings.Repeat("─", w)+"╯"+cReset)
		// Status line: current mode + key hints. Always visible so ←/→ mode
		// switching has a discoverable, live indicator.
		lines = append(lines, "  "+cDim+"mode "+cReset+cYell+s.mode+cReset+cDim+"  ·  ←/→ mode · / comandi · @ file"+cReset)

		if kind != 0 && len(sugg) > 0 {
			// Scrolling viewport that follows suggIdx so the highlighted entry
			// is always visible, even past the first page. Cap the visible rows
			// to the terminal height (box 3 + status line + up/down markers +
			// hint = 7 rows of overhead) so the whole block never spills past the
			// bottom of the screen — when it did, the terminal scrolled and the
			// in-place redraw lost the highlighted row.
			visible := termHeight() - 7
			if visible < 3 {
				visible = 3
			}
			if visible > 10 {
				visible = 10
			}
			start, end := windowBounds(suggIdx, len(sugg), visible)
			if start > 0 {
				lines = append(lines, "  "+cDim+"↑ altri "+fmt.Sprintf("%d", start)+cReset)
			}
			for i := start; i < end; i++ {
				sg := sugg[i]
				if i == suggIdx {
					lines = append(lines, "  "+cInv+" "+sg+" "+cReset)
				} else {
					lines = append(lines, "  "+cDim+sg+cReset)
				}
			}
			if end < len(sugg) {
				lines = append(lines, "  "+cDim+"↓ altri "+fmt.Sprintf("%d", len(sugg)-end)+cReset)
			}
			hint := "Tab/→ completa · ↑↓ scegli · Invio invia"
			lines = append(lines, "  "+cDim+hint+cReset)
		}
		_ = frag

		// Clear previous render and draw.
		if prevLines > 0 {
			fmt.Printf("\033[%dA", prevLines-1)
		}
		fmt.Print("\r\033[J")
		fmt.Print(strings.Join(lines, "\r\n"))
		prevLines = len(lines)
	}

	// clearRegion erases the whole framed input + suggestions block.
	clearRegion := func() {
		if prevLines > 0 {
			fmt.Printf("\033[%dA", prevLines-1)
		}
		fmt.Print("\r\033[J")
		prevLines = 0
	}

	complete := func() {
		kind, frag, sugg := s.suggestions(string(buf))
		if kind == 0 || len(sugg) == 0 {
			return
		}
		if suggIdx >= len(sugg) {
			suggIdx = 0
		}
		sel := sugg[suggIdx]
		if kind == '/' {
			buf = []rune(strings.Fields(sel)[0] + " ")
		} else { // '@'
			// replace the current @token with @<sel>
			line := string(buf)
			at := strings.LastIndex(line, "@")
			if at >= 0 {
				// keep dir prefix already typed before the fragment
				base := line[:at+1]
				if d := strings.LastIndexAny(frag, "/\\"); d >= 0 {
					base += frag[:d+1]
				}
				buf = []rune(base + sel)
			}
		}
		suggIdx = 0
	}

	render()
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return "", true
		}
		switch ch {
		case 3: // Ctrl+C
			clearRegion()
			return "", true
		case 4: // Ctrl+D
			if len(buf) == 0 {
				clearRegion()
				return "", true
			}
		case '\r', '\n':
			kind, _, sugg := s.suggestions(string(buf))
			if kind == '/' && len(sugg) > 0 {
				// Enter on a highlighted command → run it directly.
				if suggIdx >= len(sugg) {
					suggIdx = 0
				}
				cmd := strings.Fields(sugg[suggIdx])[0]
				clearRegion()
				fmt.Printf("  %s›%s %s\r\n", cCyan, cReset, cmd)
				return cmd, false
			}
			if kind == '@' && len(sugg) > 0 {
				// Enter on a file suggestion → insert it and keep editing.
				complete()
				render()
				continue
			}
			clearRegion()
			line := string(buf)
			// Echo the submitted question as a highlighted bar so it stands out
			// clearly from the model's output that follows.
			fmt.Printf("  %s › %s %s\r\n", cQBg+cQFg+cBold, line, cReset)
			return line, false
		case '\t':
			complete()
			render()
		case 127, 8: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
			}
			suggIdx = 0
			histPos = len(s.history)
			render()
		case 27: // ESC — maybe an arrow
			b1, _, _ := r.ReadRune()
			if b1 == '[' {
				b2, _, _ := r.ReadRune()
				switch b2 {
				case 'A': // up → menu prev, else previous history entry
					if k, _, sg := s.suggestions(string(buf)); k != 0 && len(sg) > 0 {
						if suggIdx > 0 {
							suggIdx--
						}
					} else if histPos > 0 {
						if histPos == len(s.history) {
							stash = append([]rune(nil), buf...)
						}
						histPos--
						buf = []rune(s.history[histPos])
						suggIdx = 0
					}
					render()
				case 'B': // down → menu next, else newer history / back to draft
					if k, _, sg := s.suggestions(string(buf)); k != 0 && len(sg) > 0 {
						if suggIdx < len(sg)-1 {
							suggIdx++
						}
					} else if histPos < len(s.history) {
						histPos++
						if histPos == len(s.history) {
							buf = append([]rune(nil), stash...)
						} else {
							buf = []rune(s.history[histPos])
						}
						suggIdx = 0
					}
					render()
				case 'C': // right → complete suggestion, else cycle mode forward
					if k, _, sg := s.suggestions(string(buf)); k != 0 && len(sg) > 0 {
						complete()
					} else {
						s.cycleMode(1)
					}
					render()
				case 'D': // left → cycle mode backward (when no suggestion menu)
					if k, _, sg := s.suggestions(string(buf)); k == 0 || len(sg) == 0 {
						s.cycleMode(-1)
					}
					render()
				}
			}
		default:
			if ch >= 32 {
				buf = append(buf, ch)
				suggIdx = 0
				histPos = len(s.history)
				render()
			}
		}
	}
}

// promptLineRaw reads one line of free text in raw mode, echoing characters and
// handling backspace/Enter itself. Using raw mode makes input reliable even when
// the terminal was left in a non-cooked state mid tool-loop (where a plain
// ReadString would not echo and Enter would not submit).
func promptLineRaw(prompt string) string {
	fd := int(os.Stdin.Fd())
	fmt.Print(prompt)
	if !term.IsTerminal(fd) {
		in, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(in)
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		in, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(in)
	}
	defer term.Restore(fd, old)
	r := bufio.NewReader(os.Stdin)
	var buf []rune
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		}
		switch ch {
		case 3: // Ctrl+C
			fmt.Print("\r\n")
			return ""
		case '\r', '\n':
			fmt.Print("\r\n")
			return strings.TrimSpace(string(buf))
		case 127, 8: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}
		default:
			if ch >= 32 {
				buf = append(buf, ch)
				fmt.Print(string(ch)) // echo
			}
		}
	}
	return strings.TrimSpace(string(buf))
}

// cycleMode advances the session mode (ask · plan · auto) by dir (+1/-1),
// keeps the autonomous flag in sync with "auto", and persists the choice.
func (s *codeSession) cycleMode(dir int) {
	order := []string{"ask", "plan", "auto"}
	i := 0
	for k, m := range order {
		if m == s.mode {
			i = k
		}
	}
	i = (i + dir + len(order)) % len(order)
	s.mode = order[i]
	s.autonomous = s.mode == "auto"
	s.savePrefs()
}

// suggestions returns (kind, fragment, list) for the current input.
// kind is '/' for commands, '@' for files, 0 for none.
func (s *codeSession) suggestions(line string) (byte, string, []string) {
	if strings.HasPrefix(line, "/") && !strings.Contains(line, " ") {
		var vals []string
		for _, c := range slashCmds {
			if strings.HasPrefix(c.Cmd, line) {
				vals = append(vals, fmt.Sprintf("%-9s %s", c.Cmd, c.Desc))
			}
		}
		return '/', line, vals
	}
	at := strings.LastIndex(line, "@")
	if at >= 0 {
		after := line[at+1:]
		if strings.ContainsAny(after, " \t") {
			return 0, "", nil
		}
		dir := s.workdir
		frag := after
		if d := strings.LastIndexAny(after, "/\\"); d >= 0 {
			dir = s.workdir + string(os.PathSeparator) + after[:d]
			frag = after[d+1:]
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return '@', after, nil
		}
		var out []string
		fl := strings.ToLower(frag)
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") && fl == "" {
				continue
			}
			if fl == "" || strings.Contains(strings.ToLower(name), fl) {
				if e.IsDir() {
					name += "/"
				}
				out = append(out, name)
			}
		}
		sort.Strings(out)
		if len(out) > 20 {
			out = out[:20]
		}
		return '@', after, out
	}
	return 0, "", nil
}

// selectList shows an arrow-navigable list and returns the chosen index (or -1).
// start is the initially highlighted index.
func selectList(title string, items []string, start int) int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || len(items) == 0 {
		return -1
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1
	}
	defer term.Restore(fd, old)
	r := bufio.NewReader(os.Stdin)
	idx := start
	if idx < 0 || idx >= len(items) {
		idx = 0
	}
	prev := 0
	draw := func() {
		// Reserve rows for title + hint (+ scroll markers) and keep the
		// highlighted row inside the visible viewport.
		maxRows := termHeight() - 4
		if maxRows < 3 {
			maxRows = 3
		}
		start, end := windowBounds(idx, len(items), maxRows)
		var lines []string
		lines = append(lines, cBold+title+cReset)
		if start > 0 {
			lines = append(lines, cDim+"   ↑ altri "+fmt.Sprintf("%d", start)+cReset)
		}
		for i := start; i < end; i++ {
			it := items[i]
			if i == idx {
				lines = append(lines, cInv+" › "+it+" "+cReset)
			} else {
				lines = append(lines, "   "+it)
			}
		}
		if end < len(items) {
			lines = append(lines, cDim+"   ↓ altri "+fmt.Sprintf("%d", len(items)-end)+cReset)
		}
		lines = append(lines, cDim+"↑↓ scegli · Invio conferma · q annulla"+cReset)
		if prev > 0 {
			fmt.Printf("\033[%dA", prev-1)
		}
		fmt.Print("\r\033[J")
		fmt.Print(strings.Join(lines, "\r\n"))
		prev = len(lines)
	}
	draw()
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return -1
		}
		switch ch {
		case 3, 'q': // Ctrl+C / q
			fmt.Print("\r\n")
			return -1
		case '\r', '\n':
			fmt.Print("\r\n")
			return idx
		case 'k':
			if idx > 0 {
				idx--
			}
			draw()
		case 'j':
			if idx < len(items)-1 {
				idx++
			}
			draw()
		case 27:
			b1, _, _ := r.ReadRune()
			if b1 == '[' {
				b2, _, _ := r.ReadRune()
				if b2 == 'A' && idx > 0 {
					idx--
				} else if b2 == 'B' && idx < len(items)-1 {
					idx++
				}
				draw()
			}
		}
	}
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func termHeight() int {
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// windowBounds returns [start,end) of a scrolling viewport of length total that
// keeps idx visible within at most max rows. Used so long selection lists and
// autocomplete menus never push the highlighted row off-screen.
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
