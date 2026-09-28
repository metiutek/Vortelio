package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestCoding(t *testing.T, mode string, approve func(tool, summary, args string) bool) (*codingProvider, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &AgenticConfig{Mode: mode, WorkingDir: dir, ApproveFunc: approve}
	return newCodingProvider(cfg, nil), dir
}

func args(m map[string]interface{}) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFileNumberedAndPaged(t *testing.T) {
	c, dir := newTestCoding(t, ModeAuto, nil)
	write(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
	out, err := c.Execute("read_file", args(map[string]interface{}{"path": "a.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "     1\tone\n") || !strings.Contains(out, "     4\tfour\n") {
		t.Fatalf("unexpected read output:\n%s", out)
	}
	out, _ = c.Execute("read_file", args(map[string]interface{}{"path": "a.txt", "offset": 2, "limit": 2}))
	if !strings.Contains(out, "     2\ttwo") || strings.Contains(out, "one") || !strings.Contains(out, "lines 2-3 of 4") {
		t.Fatalf("paging broken:\n%s", out)
	}
}

func TestEditRequiresReadAndUniqueMatch(t *testing.T) {
	c, dir := newTestCoding(t, ModeAuto, nil)
	write(t, dir, "x.go", "a := 1\nb := 1\n")
	edit := args(map[string]interface{}{"path": "x.go", "old_string": "1", "new_string": "2"})
	if _, err := c.Execute("edit_file", edit); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("edit without read must fail, got %v", err)
	}
	c.Execute("read_file", args(map[string]interface{}{"path": "x.go"}))
	if _, err := c.Execute("edit_file", edit); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("ambiguous edit must fail, got %v", err)
	}
	if _, err := c.Execute("edit_file", args(map[string]interface{}{"path": "x.go", "old_string": "b := 1", "new_string": "b := 2"})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute("edit_file", args(map[string]interface{}{"path": "x.go", "old_string": "1", "new_string": "3", "replace_all": true})); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "x.go"))
	if string(data) != "a := 3\nb := 2\n" {
		t.Fatalf("got %q", data)
	}
}

func TestEditCRLFAndLegacyNames(t *testing.T) {
	c, dir := newTestCoding(t, ModeAuto, nil)
	write(t, dir, "w.txt", "line1\r\nline2\r\n")
	c.Execute("read_file", args(map[string]interface{}{"path": "w.txt"}))
	if _, err := c.Execute("edit_file", args(map[string]interface{}{"path": "w.txt", "old_text": "line1\nline2", "new_text": "A\nB"})); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "w.txt"))
	if string(data) != "A\r\nB\r\n" {
		t.Fatalf("got %q", data)
	}
}

func TestWriteExistingNeedsRead(t *testing.T) {
	c, dir := newTestCoding(t, ModeAuto, nil)
	write(t, dir, "e.txt", "old")
	if _, err := c.Execute("write_file", args(map[string]interface{}{"path": "e.txt", "content": "new"})); err == nil {
		t.Fatal("overwrite without read must fail")
	}
	if _, err := c.Execute("write_file", args(map[string]interface{}{"path": "sub/new.txt", "content": "hi"})); err != nil {
		t.Fatalf("creating a new file must work: %v", err)
	}
}

func TestPathOutsideWorkspace(t *testing.T) {
	c, _ := newTestCoding(t, ModeAuto, nil)
	if _, err := c.Execute("read_file", args(map[string]interface{}{"path": "../outside.txt"})); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected containment error, got %v", err)
	}
}

func TestModes(t *testing.T) {
	asked := 0
	approve := func(tool, summary, a string) bool { asked++; return false }

	// plan: blocked, never asks
	c, dir := newTestCoding(t, ModePlan, approve)
	if _, err := c.Execute("write_file", args(map[string]interface{}{"path": "p.txt", "content": "x"})); err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("plan must block writes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p.txt")); err == nil {
		t.Fatal("file written in plan mode")
	}
	// ask: asks, denial blocks
	c, dir = newTestCoding(t, ModeAsk, approve)
	if _, err := c.Execute("write_file", args(map[string]interface{}{"path": "q.txt", "content": "x"})); err == nil || asked != 1 {
		t.Fatalf("ask mode must ask and respect denial (asked=%d err=%v)", asked, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "q.txt")); err == nil {
		t.Fatal("denied write happened")
	}
	// edits: writes free, shell asks
	asked = 0
	c, dir = newTestCoding(t, ModeEdits, approve)
	if _, err := c.Execute("write_file", args(map[string]interface{}{"path": "r.txt", "content": "x"})); err != nil || asked != 0 {
		t.Fatalf("edits mode must write without asking (asked=%d err=%v)", asked, err)
	}
	if _, err := c.Execute("run_shell", args(map[string]interface{}{"command": "echo hi"})); err == nil || asked != 1 {
		t.Fatalf("edits mode must ask for shell (asked=%d err=%v)", asked, err)
	}
	_ = dir
}

func TestPolicyDenyBeatsAuto(t *testing.T) {
	dir := t.TempDir()
	cfg := &AgenticConfig{Mode: ModeAuto, WorkingDir: dir, Policy: func(tool, a string) string {
		if tool == "run_shell" && strings.Contains(a, "rm ") {
			return PolicyDeny
		}
		return ""
	}}
	c := newCodingProvider(cfg, nil)
	if _, err := c.Execute("run_shell", args(map[string]interface{}{"command": "rm -rf x"})); err == nil || !strings.Contains(err.Error(), "permission rule") {
		t.Fatalf("deny rule must win in auto mode: %v", err)
	}
}

func TestPlanBlocksEvenWithApprove(t *testing.T) {
	// Regression: the builtin read/write tools used to shadow the coding tools,
	// so plan/ask modes were silently bypassed in the composite provider.
	dir := t.TempDir()
	prov := buildAgenticProvider(&AgenticConfig{Mode: ModePlan, WorkingDir: dir, Coding: true, Builtins: true, WebSearch: true}, nil)
	names := map[string]int{}
	for _, tl := range prov.Tools() {
		names[tl.Function.Name]++
	}
	for n, k := range names {
		if k > 1 {
			t.Fatalf("tool %s listed %d times", n, k)
		}
	}
	if _, err := prov.Execute("write_file", args(map[string]interface{}{"path": "z.txt", "content": "x"})); err == nil {
		t.Fatal("plan mode write went through the composite provider")
	}
}

func TestGlobAndGrep(t *testing.T) {
	c, dir := newTestCoding(t, ModeAuto, nil)
	write(t, dir, "main.go", "package main\nfunc Foo() {}\n")
	write(t, dir, "pkg/a/b.go", "package a\nfunc fooBar() {}\n")
	write(t, dir, "web/app.tsx", "export const Foo = 1\n")
	write(t, dir, "node_modules/x/y.go", "func Foo() {}\n")

	out, _ := c.Execute("glob_search", args(map[string]interface{}{"pattern": "**/*.go"}))
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "pkg/a/b.go") || strings.Contains(out, "node_modules") {
		t.Fatalf("glob **/*.go:\n%s", out)
	}
	out, _ = c.Execute("glob_search", args(map[string]interface{}{"pattern": "*.{go,tsx}"}))
	if !strings.Contains(out, "web/app.tsx") || !strings.Contains(out, "main.go") {
		t.Fatalf("glob braces:\n%s", out)
	}
	out, _ = c.Execute("grep_search", args(map[string]interface{}{"pattern": `func \w*[Ff]oo`, "include": "*.go"}))
	if !strings.Contains(out, "main.go:2:") || !strings.Contains(out, "pkg/a/b.go:2:") || strings.Contains(out, "app.tsx") {
		t.Fatalf("grep regex+include:\n%s", out)
	}
	out, _ = c.Execute("grep_search", args(map[string]interface{}{"pattern": "FOO", "case_insensitive": true, "path": "web"}))
	if !strings.Contains(out, "app.tsx:1:") {
		t.Fatalf("grep -i path:\n%s", out)
	}
	out, _ = c.Execute("grep_search", args(map[string]interface{}{"pattern": "Foo("}))
	if !strings.Contains(out, "main.go") {
		t.Fatalf("invalid regex must fall back to literal:\n%s", out)
	}
}

func TestShellExitCodeAndTimeout(t *testing.T) {
	c, _ := newTestCoding(t, ModeAuto, nil)
	out, err := c.Execute("run_shell", args(map[string]interface{}{"command": "exit 3"}))
	if err != nil || !strings.Contains(out, "[exit code 3]") {
		t.Fatalf("exit code: %q %v", out, err)
	}
	sleep := "sleep 5"
	if isWindows() {
		sleep = "Start-Sleep -Seconds 5"
	}
	_, err = c.Execute("run_shell", args(map[string]interface{}{"command": sleep, "timeout_ms": 1500}))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestTodoAndDiffEvents(t *testing.T) {
	var events []string
	dir := t.TempDir()
	c := newCodingProvider(&AgenticConfig{Mode: ModeAuto, WorkingDir: dir}, func(ev string, data interface{}) {
		events = append(events, ev)
	})
	c.Execute("todo_write", args(map[string]interface{}{"todos": []map[string]string{{"content": "a", "status": "in_progress"}, {"content": "b", "status": "weird"}}}))
	if len(c.state.Todos) != 2 || c.state.Todos[1].Status != "pending" {
		t.Fatalf("todos: %+v", c.state.Todos)
	}
	c.Execute("write_file", args(map[string]interface{}{"path": "n.txt", "content": "x\n"}))
	if strings.Join(events, ",") != "todo_update,file_diff" {
		t.Fatalf("events: %v", events)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\n"
	b := "l1\nl2\nl3\nL4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\n"
	d, add, del := unifiedDiff("f.txt", a, b)
	if add != 2 || del != 1 {
		t.Fatalf("add=%d del=%d\n%s", add, del, d)
	}
	for _, want := range []string{"--- a/f.txt", "@@ -1,7 +1,7 @@", "-l4\n+L4", "@@ -10,3 +10,4 @@", "+l13"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
	if d, _, _ := unifiedDiff("f", "same", "same"); d != "" {
		t.Fatal("identical texts must give empty diff")
	}
	d, add, _ = unifiedDiff("new.txt", "", "a\nb\n")
	if add != 2 || !strings.Contains(d, "@@ -1,0 +1,2 @@") {
		t.Fatalf("creation diff:\n%s", d)
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "c.go", true},
		{"src/**", "src/x/y", true},
		{"src/*.ts", "src/a/b.ts", false},
		{"*.{ts,tsx}", "a.tsx", true},
		{"file?.txt", "file1.txt", true},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s vs %s = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}
