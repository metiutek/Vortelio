package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vortelio/vortelio/internal/server"
)

func TestParseCodeArgs(t *testing.T) {
	o, err := parseCodeArgs([]string{"-p", "fix", "the", "bug", "-m", "cloud/ollamacloud/gpt-oss:120b", "--mode", "edits", "-C", "x", "--max-turns", "7", "--output-format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.print || o.prompt != "fix the bug" || o.model != "cloud/ollamacloud/gpt-oss:120b" || o.mode != "edits" || o.dir != "x" || o.maxTurns != 7 || o.outputFormat != "json" {
		t.Fatalf("%+v", o)
	}
	o, _ = parseCodeArgs([]string{"--yes", "-c"})
	if o.mode != "auto" || !o.cont {
		t.Fatalf("%+v", o)
	}
	o, _ = parseCodeArgs([]string{"-r", "20260928-101010-abcdef12"})
	if o.resume != "20260928-101010-abcdef12" || o.resumePick {
		t.Fatalf("%+v", o)
	}
	o, _ = parseCodeArgs([]string{"--resume"})
	if !o.resumePick {
		t.Fatalf("%+v", o)
	}
	for _, bad := range [][]string{{"--mode", "yolo"}, {"--bogus"}, {"-m"}, {"--max-turns", "0"}} {
		if _, err := parseCodeArgs(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestPermissionRules(t *testing.T) {
	p := codePermissions{
		Allow: []string{"run_shell(go test*)", "Bash(npm run test:*)", "Edit(src/**)", "read_file"},
		Deny:  []string{"run_shell(go test ./secret*)", "Write"},
	}
	cases := []struct {
		tool, args, want string
	}{
		{"run_shell", `{"command":"go test ./..."}`, "allow"},
		{"run_shell", `{"command":"go test ./secret/x"}`, "deny"},
		{"run_shell", `{"command":"npm run test:unit"}`, "allow"},
		{"run_shell", `{"command":"go build"}`, ""},
		{"edit_file", `{"path":"src/a/b.go"}`, "allow"},
		{"edit_file", `{"path":"docs/x.md"}`, ""},
		{"write_file", `{"path":"src/a.go"}`, "deny"},
		{"read_file", `{"path":"anything"}`, "allow"},
	}
	for _, c := range cases {
		if got := p.decide(c.tool, c.args); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.tool, c.args, got, c.want)
		}
	}
	if r := suggestShellRule("go test ./pkg -run X"); r != "run_shell(go test*)" {
		t.Fatalf("suggest: %s", r)
	}
	if r := suggestShellRule("ls -la"); r != "run_shell(ls*)" {
		t.Fatalf("suggest: %s", r)
	}
}

func withHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("VORTELIO_HOME", h)
	return h
}

func TestSettingsMerge(t *testing.T) {
	withHome(t)
	proj := t.TempDir()
	tr := true
	writeSettings(globalSettingsPath(), codeSettings{Model: "a:1", Mode: "edits", ShowThinking: &tr, Permissions: codePermissions{Allow: []string{"read_file"}}})
	f := false
	writeSettings(projectSettingsPath(proj), codeSettings{Mode: "plan", ShowThinking: &f, ContextTokens: 8192, Permissions: codePermissions{Deny: []string{"run_shell"}}})
	e, warns := effectiveSettings(proj)
	if len(warns) != 0 {
		t.Fatal(warns)
	}
	if e.Model != "a:1" || e.Mode != "plan" || boolOr(e.ShowThinking, true) || e.ContextTokens != 8192 ||
		len(e.Permissions.Allow) != 1 || len(e.Permissions.Deny) != 1 {
		t.Fatalf("%+v", e)
	}
	os.WriteFile(projectSettingsPath(proj), []byte("{bad json"), 0o644)
	if _, warns := effectiveSettings(proj); len(warns) == 0 {
		t.Fatal("broken project settings must warn")
	}
}

func TestLegacyPrefsMigration(t *testing.T) {
	h := withHome(t)
	os.WriteFile(filepath.Join(h, "code_session.json"), []byte(`{"cloud_provider":"ollamacloud","cloud_model":"gpt-oss:20b","mode":"auto"}`), 0o644)
	e, _ := effectiveSettings(t.TempDir())
	if e.Model != "cloud/ollamacloud/gpt-oss:20b" || e.Mode != "ask" {
		t.Fatalf("%+v", e)
	}
}

func TestCustomCommands(t *testing.T) {
	withHome(t)
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".vortelio", "commands"), 0o755)
	os.WriteFile(filepath.Join(proj, ".vortelio", "commands", "Review.md"), []byte("---\ndescription: Review a file\n---\nReview $1 carefully. Notes: $ARGUMENTS\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".vortelio", "commands", "plain.md"), []byte("Explain the build."), 0o644)
	cmds := loadCustomCommands(proj)
	r, ok := cmds["review"]
	if !ok || r.Description != "Review a file" {
		t.Fatalf("%+v", cmds)
	}
	if got := r.expand("main.go be strict"); got != "Review main.go carefully. Notes: main.go be strict\n" {
		t.Fatalf("%q", got)
	}
	if got := cmds["plain"].expand("for windows"); got != "Explain the build.\n\nfor windows" {
		t.Fatalf("%q", got)
	}
}

func TestInstructionFiles(t *testing.T) {
	h := withHome(t)
	os.WriteFile(filepath.Join(h, "AGENTS.md"), []byte("global rule"), 0o644)
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "CLAUDE.md"), []byte("project rule"), 0o644)
	os.WriteFile(filepath.Join(proj, "PROJECT.md"), []byte("legacy"), 0o644)
	files := loadInstructions(proj)
	if len(files) != 2 || files[0].Content != "global rule" || files[1].Content != "project rule" {
		t.Fatalf("%+v", files)
	}
}

func TestSessionsSaveList(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	s1 := &savedSession{ID: newSessionID(), Dir: dir, Messages: []chatMsg{{"user", "hi"}}}
	saveSession(s1)
	time.Sleep(10 * time.Millisecond)
	s2 := &savedSession{ID: newSessionID() + "x", Dir: t.TempDir(), Messages: []chatMsg{{"user", "other"}}}
	saveSession(s2)
	if l := listSessions(dir); len(l) != 1 || l[0].ID != s1.ID {
		t.Fatalf("%v", l)
	}
	if l := listSessions(""); len(l) != 2 || l[0].ID != s2.ID {
		t.Fatalf("newest first: %v", l)
	}
	if !looksLikeSessionID(s1.ID) {
		t.Fatal(s1.ID)
	}
}

func collectKeys(input string, n int) []key {
	t := newTestTerminal(strings.NewReader(input))
	var out []key
	for i := 0; i < n; i++ {
		out = append(out, t.ReadKey())
	}
	return out
}

func TestKeyDecoding(t *testing.T) {
	ks := collectKeys("a\x1b[A\x1b[B\x1b[Z\x1b[3~\x1b[1;5D\x1b[200~x\ny\x1b[201~\x03", 8)
	want := []keyKind{kRune, kUp, kDown, kShiftTab, kDelete, kWordLeft, kPaste, kCtrlC}
	for i, k := range ks {
		if k.kind != want[i] {
			t.Fatalf("key %d = %v, want %v (%+v)", i, k.kind, want[i], ks)
		}
	}
	if ks[6].text != "x\ny" {
		t.Fatalf("paste %q", ks[6].text)
	}
	// Lone ESC, then CRLF from a pipe = a single Enter, then EOF.
	ks = collectKeys("\x1b", 2)
	if ks[0].kind != kEsc || ks[1].kind != kEOF {
		t.Fatalf("%+v", ks)
	}
	ks = collectKeys("x\r\ny\n", 5)
	if ks[0].kind != kRune || ks[1].kind != kEnter || ks[2].r != 'y' || ks[3].kind != kEnter || ks[4].kind != kEOF {
		t.Fatalf("%+v", ks)
	}
}

func TestEditorPlain(t *testing.T) {
	tm := newTestTerminal(strings.NewReader("hello\n\n/mode plan\n"))
	ed := &lineEditor{t: tm}
	for _, want := range []string{"hello", "/mode plan"} {
		line, res := ed.read()
		if res != edSubmit || line != want {
			t.Fatalf("got %q %v, want %q", line, res, want)
		}
	}
	if _, res := ed.read(); res != edExit {
		t.Fatal("expected exit at EOF")
	}
	if len(ed.history) != 2 {
		t.Fatalf("history %v", ed.history)
	}
}

func TestThinkTagSplitter(t *testing.T) {
	var c, th strings.Builder
	sp := &thinkTagSplitter{content: func(s string) { c.WriteString(s) }, think: func(s string) { th.WriteString(s) }}
	for _, tok := range []string{"<thi", "nk>reason", "ing</th", "ink>ans", "wer"} {
		sp.feed(tok)
	}
	sp.flush()
	if c.String() != "answer" || th.String() != "reasoning" {
		t.Fatalf("content=%q think=%q", c.String(), th.String())
	}
}

func TestMarkdownStream(t *testing.T) {
	m := &mdStream{color: false, lineStart: true}
	var out strings.Builder
	for _, tok := range []string{"# Ti", "tle\nsome **bo", "ld** and `co", "de`\n``", "`go\nx := 1\n```\n2 * 3"} {
		out.WriteString(m.feed(tok))
	}
	out.WriteString(m.flush())
	got := out.String()
	want := "Title\nsome bold and code\n```go\nx := 1\n```\n2 * 3"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func testSession(t *testing.T) *codeSession {
	t.Helper()
	withHome(t)
	dir := t.TempDir()
	s := &codeSession{workdir: dir, mode: "ask", state: server.NewCodingState(), turnSeen: map[string]bool{}}
	s.settings, _ = effectiveSettings(dir)
	s.ui = newCodeUI(&bytes.Buffer{}, &bytes.Buffer{}, false, true)
	s.sess = &savedSession{ID: newSessionID(), Dir: dir}
	s.customCmds = map[string]customCommand{}
	return s
}

func TestSlashCommandsState(t *testing.T) {
	s := testSession(t)

	s.handleCommand("/mode plan")
	if s.mode != "plan" {
		t.Fatal(s.mode)
	}
	if g, _ := readSettings(globalSettingsPath()); g.Mode != "" {
		t.Fatalf("/mode must be session-only: %+v", g)
	}
	s.handleCommand("/mode bogus")
	if s.mode != "plan" {
		t.Fatal("invalid mode must be ignored")
	}

	s.handleCommand("/permissions allow run_shell(make*)")
	if p, _ := readSettings(projectSettingsPath(s.workdir)); len(p.Permissions.Allow) != 1 {
		t.Fatalf("%+v", p)
	}
	if s.settings.Permissions.decide("run_shell", `{"command":"make all"}`) != "allow" {
		t.Fatal("rule not live")
	}
	s.handleCommand("/permissions remove run_shell(make*)")
	if s.settings.Permissions.decide("run_shell", `{"command":"make all"}`) != "" {
		t.Fatal("rule not removed")
	}

	s.handleCommand("/config set context_tokens 32768")
	if s.contextLimit() != 32768 {
		t.Fatal(s.contextLimit())
	}
	s.handleCommand("/config set max_turns 5 --project")
	if s.maxTurns() != 5 {
		t.Fatal(s.maxTurns())
	}
	s.handleCommand("/config set context_tokens 100")
	if s.contextLimit() != 32768 {
		t.Fatal("too small context must be rejected")
	}

	s.showThinking = true
	s.handleCommand("/thinking off")
	if s.showThinking || s.ui.showThinking {
		t.Fatal("thinking not off")
	}

	s.handleCommand("/media on")
	s.handleCommand("/mcp on")
	if !s.media || !s.mcpOn {
		t.Fatal("toggles")
	}

	sub := filepath.Join(s.workdir, "sub")
	os.Mkdir(sub, 0o755)
	s.handleCommand("/cd sub")
	if s.workdir != sub {
		t.Fatal(s.workdir)
	}

	s.messages = []chatMsg{{"user", "x"}}
	s.summary = "old"
	s.handleCommand("/clear")
	if len(s.messages) != 0 || s.summary != "" {
		t.Fatal("clear")
	}
	if !s.handleCommand("/exit") || !s.handleCommand("/q") {
		t.Fatal("exit")
	}
	if s.handleCommand("/nope") {
		t.Fatal("unknown must not exit")
	}
}

func TestUndoRestoresFiles(t *testing.T) {
	s := testSession(t)
	existing := filepath.Join(s.workdir, "a.txt")
	created := filepath.Join(s.workdir, "b.txt")
	os.WriteFile(existing, []byte("before"), 0o644)
	s.onFileChange(existing, []byte("before"), true)
	s.onFileChange(existing, []byte("middle"), true) // second change in the same turn: first snapshot wins
	s.onFileChange(created, nil, false)
	os.WriteFile(existing, []byte("after"), 0o644)
	os.WriteFile(created, []byte("new"), 0o644)
	s.undo = append(s.undo, s.turnCPs)

	s.handleCommand("/undo")
	if b, _ := os.ReadFile(existing); string(b) != "before" {
		t.Fatalf("restored %q", b)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatal("created file must be removed")
	}
	if !strings.Contains(s.pendingContext, "undid") {
		t.Fatal("model must be told about the undo")
	}
}

func TestPolicyFollowsMode(t *testing.T) {
	s := testSession(t)
	edit := `{"path":"x.go"}`
	s.mode = "edits"
	if s.policy("edit_file", edit) != server.PolicyAllow || s.policy("run_shell", `{"command":"ls"}`) != "" {
		t.Fatal("edits mode")
	}
	s.mode = "plan"
	s.settings.Permissions.Allow = []string{"edit_file"}
	if s.policy("edit_file", edit) == server.PolicyAllow {
		t.Fatal("allow rules must not bypass plan mode")
	}
	s.mode = "auto"
	s.settings.Permissions.Deny = []string{"run_shell(rm*)"}
	if s.policy("run_shell", `{"command":"rm -rf /"}`) != server.PolicyDeny {
		t.Fatal("deny must win in auto")
	}
}

func TestMemoryNoteAndFileRefs(t *testing.T) {
	s := testSession(t)
	s.addMemory("use tabs")
	data, _ := os.ReadFile(filepath.Join(s.workdir, "AGENTS.md"))
	if !strings.Contains(string(data), "- use tabs") || len(s.instr) != 1 {
		t.Fatalf("%q %v", data, s.instr)
	}
	os.WriteFile(filepath.Join(s.workdir, "f.txt"), []byte("content!"), 0o644)
	out := s.expandFileRefs("look at @f.txt and email@example.com")
	if !strings.Contains(out, `<file path="f.txt">`) || !strings.Contains(out, "content!") || strings.Count(out, "<file") != 1 {
		t.Fatalf("%s", out)
	}
}

func TestCompletions(t *testing.T) {
	s := testSession(t)
	s.customCmds["deploy"] = customCommand{Name: "deploy", Description: "ship it"}
	start, items := s.completions("/de")
	if start != 0 || len(items) != 1 || items[0].insert != "/deploy" {
		t.Fatalf("%+v", items)
	}
	_, items = s.completions("/c")
	if len(items) < 4 {
		t.Fatalf("%+v", items)
	}
	os.WriteFile(filepath.Join(s.workdir, "main.go"), nil, 0o644)
	os.Mkdir(filepath.Join(s.workdir, "internal"), 0o755)
	start, items = s.completions("see @ma")
	if start != 4 || len(items) != 1 || items[0].insert != "@main.go" {
		t.Fatalf("%d %+v", start, items)
	}
}
