package runtime

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestThinkSplitterKeepsCharacters(t *testing.T) {
	var out []string
	ts := &thinkSplitter{emit: func(s string) { out = append(out, s) }, thinkEmit: func(string) {}}
	in := []string{"code—the AI can ", "analyze — and ", "è più veloce…"}
	for _, c := range in {
		ts.feed(c)
	}
	ts.flush()
	for _, o := range out {
		if !utf8.ValidString(o) {
			t.Fatalf("emitted a broken character: %q", o)
		}
	}
	if got := strings.Join(out, ""); got != strings.Join(in, "") {
		t.Errorf("got %q", got)
	}
}
