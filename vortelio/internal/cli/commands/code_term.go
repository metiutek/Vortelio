package commands

import (
	"bufio"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// ── Terminal input for `vortelio code` ───────────────────────────────────────
//
// The session keeps stdin in raw mode and decodes it into keys on a single
// goroutine. Every consumer (line editor, pickers, approval prompts, the
// Esc-to-interrupt watcher) reads from that one stream, so nothing races for
// stdin. Output processing stays on (see enableOutputProcessing), so plain
// "\n" still moves to the start of the next line.

type keyKind int

const (
	kRune keyKind = iota
	kEnter
	kNewline // Ctrl+J / Alt+Enter: insert a line break
	kTab
	kShiftTab
	kBackspace
	kDelete
	kUp
	kDown
	kLeft
	kRight
	kHome
	kEnd
	kWordLeft
	kWordRight
	kEsc
	kCtrlC
	kCtrlD
	kCtrlU
	kCtrlK
	kCtrlW
	kCtrlL
	kCtrlO
	kPaste
	kEOF
)

type key struct {
	kind keyKind
	r    rune
	text string // kPaste
}

type terminal struct {
	tty     bool
	restore func()
	keys    chan key

	// While a turn runs, a watcher owns keys; prompts opened during the turn
	// (approvals, ask_user) receive keys through promptKeys instead.
	routing    atomic.Bool
	promptOpen atomic.Bool
	promptKeys chan key
	mu         sync.Mutex
}

// openTerminal puts stdin in raw mode (when it is a TTY) and starts decoding.
func openTerminal() *terminal {
	fd := int(os.Stdin.Fd())
	t := &terminal{keys: make(chan key, 256), promptKeys: make(chan key, 256), restore: func() {}}
	if term.IsTerminal(fd) && term.IsTerminal(int(os.Stdout.Fd())) {
		if old, err := term.MakeRaw(fd); err == nil {
			t.tty = true
			enableOutputProcessing(fd)
			enableVTOutput()
			os.Stdout.WriteString("\033[?2004h") // bracketed paste
			t.restore = func() {
				os.Stdout.WriteString("\033[?2004l\033[?25h")
				term.Restore(fd, old)
			}
		}
	}
	go decodeKeys(bufio.NewReader(os.Stdin), t.keys, !t.tty)
	return t
}

// newTestTerminal decodes keys from r (tests and piped input).
func newTestTerminal(r io.Reader) *terminal {
	t := &terminal{keys: make(chan key, 256), promptKeys: make(chan key, 256), restore: func() {}}
	go decodeKeys(bufio.NewReader(r), t.keys, true)
	return t
}

func (t *terminal) Close() { t.restore() }

// ReadKey returns the next key for an interactive consumer.
func (t *terminal) ReadKey() key {
	if t.routing.Load() {
		return <-t.promptKeys
	}
	return <-t.keys
}

// pending reports whether more keys are already queued (used to coalesce
// redraws while keys arrive in bursts, e.g. fast typing or a paste).
func (t *terminal) pending() bool {
	if t.routing.Load() {
		return len(t.promptKeys) > 0
	}
	return len(t.keys) > 0
}

// beginPrompt/endPrompt bracket a prompt opened while a turn is running.
func (t *terminal) beginPrompt() { t.promptOpen.Store(true) }
func (t *terminal) endPrompt()   { t.promptOpen.Store(false) }

// watch routes keys during a turn: Esc / Ctrl+C call interrupt unless a prompt
// is open, in which case keys go to the prompt. Returns a stop function.
func (t *terminal) watch(interrupt func()) (stop func()) {
	done := make(chan struct{})
	t.routing.Store(true)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case k := <-t.keys:
				if t.promptOpen.Load() {
					t.promptKeys <- k
					continue
				}
				switch k.kind {
				case kEsc, kCtrlC:
					interrupt()
				case kEOF:
					interrupt()
					t.keys <- k // let the main loop see EOF
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
		t.routing.Store(false)
		// Drop keys typed while the model was working (and any prompt leftovers).
		for {
			select {
			case <-t.promptKeys:
			default:
				return
			}
		}
	}
}

// decodeKeys turns a byte stream into keys. lfIsEnter: piped input (no raw mode)
// ends lines with "\n", which must submit instead of inserting a line break.
func decodeKeys(r *bufio.Reader, out chan<- key, lfIsEnter bool) {
	runes := make(chan rune, 1024)
	go func() {
		for {
			ch, _, err := r.ReadRune()
			if err != nil {
				close(runes)
				return
			}
			runes <- ch
		}
	}()
	next := func(wait time.Duration) (rune, bool) {
		if wait <= 0 {
			ch, ok := <-runes
			return ch, ok
		}
		select {
		case ch, ok := <-runes:
			return ch, ok
		case <-time.After(wait):
			return 0, false
		}
	}
	for {
		ch, ok := next(0)
		if !ok {
			out <- key{kind: kEOF}
			return
		}
		switch ch {
		case 27:
			out <- decodeEscape(next)
		case '\r':
			// Windows/raw Enter. A following '\n' (CRLF from a pipe) is swallowed.
			if c2, ok := next(5 * time.Millisecond); ok && c2 != '\n' {
				out <- key{kind: kEnter}
				pushBack(runes, c2, out, lfIsEnter)
				continue
			}
			out <- key{kind: kEnter}
		case '\n':
			if lfIsEnter {
				out <- key{kind: kEnter}
			} else {
				out <- key{kind: kNewline}
			}
		default:
			out <- controlKey(ch, lfIsEnter)
		}
	}
}

// pushBack handles a rune read ahead by the CR check.
func pushBack(_ chan rune, ch rune, out chan<- key, lfIsEnter bool) {
	switch ch {
	case 27:
		out <- key{kind: kEsc}
	case '\n':
		if lfIsEnter {
			out <- key{kind: kEnter}
		} else {
			out <- key{kind: kNewline}
		}
	case '\r':
		out <- key{kind: kEnter}
	default:
		out <- controlKey(ch, lfIsEnter)
	}
}

func controlKey(ch rune, _ bool) key {
	switch ch {
	case '\t':
		return key{kind: kTab}
	case 127, 8:
		return key{kind: kBackspace}
	case 3:
		return key{kind: kCtrlC}
	case 4:
		return key{kind: kCtrlD}
	case 1:
		return key{kind: kHome}
	case 5:
		return key{kind: kEnd}
	case 21:
		return key{kind: kCtrlU}
	case 11:
		return key{kind: kCtrlK}
	case 23:
		return key{kind: kCtrlW}
	case 12:
		return key{kind: kCtrlL}
	case 15:
		return key{kind: kCtrlO}
	case 2:
		return key{kind: kLeft}
	case 6:
		return key{kind: kRight}
	case 16:
		return key{kind: kUp}
	case 14:
		return key{kind: kDown}
	}
	if ch < 32 {
		return key{kind: kRune, r: 0} // ignored by consumers
	}
	return key{kind: kRune, r: ch}
}

// decodeEscape parses the sequence after ESC. A lone ESC (nothing follows
// quickly) is the Esc key.
func decodeEscape(next func(time.Duration) (rune, bool)) key {
	c, ok := next(50 * time.Millisecond)
	if !ok {
		return key{kind: kEsc}
	}
	switch c {
	case '[':
		var params strings.Builder
		for {
			f, ok := next(50 * time.Millisecond)
			if !ok {
				return key{kind: kEsc}
			}
			if f >= 0x40 && f <= 0x7e {
				return csiKey(params.String(), f, next)
			}
			params.WriteRune(f)
		}
	case 'O':
		f, ok := next(50 * time.Millisecond)
		if !ok {
			return key{kind: kEsc}
		}
		switch f {
		case 'A':
			return key{kind: kUp}
		case 'B':
			return key{kind: kDown}
		case 'C':
			return key{kind: kRight}
		case 'D':
			return key{kind: kLeft}
		case 'H':
			return key{kind: kHome}
		case 'F':
			return key{kind: kEnd}
		}
		return key{kind: kRune}
	case '\r', '\n':
		return key{kind: kNewline} // Alt+Enter
	case 'b':
		return key{kind: kWordLeft}
	case 'f':
		return key{kind: kWordRight}
	case 127, 8:
		return key{kind: kCtrlW}
	case 27:
		return key{kind: kEsc}
	}
	return key{kind: kRune} // unknown Alt+key: ignore
}

func csiKey(params string, final rune, next func(time.Duration) (rune, bool)) key {
	mod := ""
	if i := strings.IndexByte(params, ';'); i >= 0 {
		mod = params[i+1:]
		params = params[:i]
	}
	word := mod == "5" || mod == "3" // Ctrl/Alt + arrow
	switch final {
	case 'A':
		return key{kind: kUp}
	case 'B':
		return key{kind: kDown}
	case 'C':
		if word {
			return key{kind: kWordRight}
		}
		return key{kind: kRight}
	case 'D':
		if word {
			return key{kind: kWordLeft}
		}
		return key{kind: kLeft}
	case 'H':
		return key{kind: kHome}
	case 'F':
		return key{kind: kEnd}
	case 'Z':
		return key{kind: kShiftTab}
	case '~':
		switch params {
		case "1", "7":
			return key{kind: kHome}
		case "4", "8":
			return key{kind: kEnd}
		case "3":
			return key{kind: kDelete}
		case "200":
			return readPaste(next)
		}
	}
	return key{kind: kRune}
}

// readPaste collects a bracketed paste until ESC[201~.
func readPaste(next func(time.Duration) (rune, bool)) key {
	var b strings.Builder
	const end = "\x1b[201~"
	for {
		c, ok := next(2 * time.Second)
		if !ok {
			break
		}
		b.WriteRune(c)
		if strings.HasSuffix(b.String(), end) {
			s := strings.TrimSuffix(b.String(), end)
			return key{kind: kPaste, text: strings.ReplaceAll(s, "\r\n", "\n")}
		}
	}
	return key{kind: kPaste, text: strings.ReplaceAll(b.String(), "\r", "\n")}
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 100
	}
	return w
}

func termHeight() int {
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h <= 0 {
		return 30
	}
	return h
}
