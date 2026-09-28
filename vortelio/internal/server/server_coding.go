package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	rt "github.com/vortelio/vortelio/internal/runtime"
)

// ── Coding tool provider ───────────────────────────────────────────────────────
//
// Workspace file and shell tools shared by the Developer GUI and `vortelio code`.
// Semantics follow the common coding-agent conventions (Claude Code, Codex,
// OpenCode): numbered reads with offset/limit, exact-match edits that must be
// unique unless replace_all, read-before-edit, regex grep, real ** globs, a
// shell with a timeout, and a todo list.

// Permission modes.
const (
	ModePlan  = "plan"  // read-only: no edits, no commands
	ModeAsk   = "ask"   // confirm edits and commands
	ModeEdits = "edits" // file edits run freely, commands are confirmed
	ModeAuto  = "auto"  // everything runs without confirmation
)

// Policy decisions returned by a PolicyFunc.
const (
	PolicyAllow = "allow"
	PolicyDeny  = "deny"
)

// PolicyFunc lets the caller apply allow/deny rules to a tool call before the
// mode is consulted. It returns PolicyAllow, PolicyDeny or "" (no rule).
type PolicyFunc func(tool, args string) string

// CodingState is the per-session state of the coding tools: which files the
// model has read (edits require a prior read) and the current todo list.
type CodingState struct {
	mu    sync.Mutex
	read  map[string]bool
	Todos []TodoItem
}

// TodoItem is one entry of the agent's task list (todo_write).
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

func NewCodingState() *CodingState { return &CodingState{read: map[string]bool{}} }

func (s *CodingState) markRead(path string) {
	s.mu.Lock()
	s.read[strings.ToLower(path)] = true
	s.mu.Unlock()
}

func (s *CodingState) wasRead(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read[strings.ToLower(path)]
}

type codingProvider struct {
	mode     string
	root     string
	emit     rt.ToolEventEmitter
	approve  func(tool, summary, args string) bool // synchronous approval (CLI); nil = HTTP flow
	policy   PolicyFunc
	onChange func(path string, before []byte, existed bool)
	state    *CodingState
	ctx      context.Context
	counter  int
	mu       sync.Mutex
}

func newCodingProvider(cfg *AgenticConfig, emit rt.ToolEventEmitter) *codingProvider {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeAsk
	}
	root := cfg.WorkingDir
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	st := cfg.State
	if st == nil {
		st = NewCodingState()
	}
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return &codingProvider{mode: mode, root: root, emit: emit, approve: cfg.ApproveFunc,
		policy: cfg.Policy, onChange: cfg.OnFileChange, state: st, ctx: ctx}
}

func (c *codingProvider) Tools() []rt.ToolDef {
	return []rt.ToolDef{
		toolDef("read_file", "Read a text file. Returns lines prefixed with their line number (cat -n style). Reads up to 2000 lines from offset; use offset/limit to page through larger files. You must read a file before editing or overwriting it.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute."},"offset":{"type":"integer","description":"1-based line to start from. Optional."},"limit":{"type":"integer","description":"Number of lines to read (default 2000). Optional."}},"required":["path"]}`),
		toolDef("write_file", "Create a file or overwrite it entirely. Prefer edit_file for changes to existing files. An existing file must be read first. Set append=true to add to the end (build very large files in chunks).",
			`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"append":{"type":"boolean","description":"Append instead of overwrite. Default false."}},"required":["path","content"]}`),
		toolDef("edit_file", "Replace exact text in a file. old_string must match the file exactly (including indentation) and be unique unless replace_all is true; include surrounding lines to make it unique. The file must be read first. With an empty old_string on a missing file, creates it with new_string.",
			`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string","description":"Exact text to replace."},"new_string":{"type":"string","description":"Replacement text."},"replace_all":{"type":"boolean","description":"Replace every occurrence. Default false."}},"required":["path","old_string","new_string"]}`),
		toolDef("list_directory", "List the files and folders in a directory of the workspace.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Directory path. Defaults to the workspace root."}},"required":[]}`),
		toolDef("glob_search", "Find files by glob pattern, e.g. **/*.go or src/**/*.tsx. Returns paths sorted by most recently modified.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob relative to path (supports **, *, ?, {a,b})."},"path":{"type":"string","description":"Directory to search in. Defaults to the workspace root."}},"required":["pattern"]}`),
		toolDef("grep_search", "Search file contents with a regular expression (RE2 syntax). Returns file:line: text for each match.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for."},"path":{"type":"string","description":"File or directory to search. Defaults to the workspace root."},"include":{"type":"string","description":"Only search files matching this glob, e.g. *.go or **/*.{ts,tsx}."},"case_insensitive":{"type":"boolean"}},"required":["pattern"]}`),
		toolDef("run_shell", "Run a shell command in the workspace root and return its output and exit code. "+shellDescription()+" Use for builds, tests, git and other CLI tools; use the file tools instead of shell commands to read, search or edit files.",
			`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer","description":"Timeout in milliseconds (default 120000, max 600000)."},"description":{"type":"string","description":"Short description of what the command does."}},"required":["command"]}`),
		toolDef("todo_write", "Create or update the task list for the current work. Use it for multi-step tasks: send the full list every time, keep exactly one item in_progress, mark items completed as soon as they are done.",
			`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"]}}},"required":["todos"]}`),
	}
}

func shellDescription() string {
	if runtime.GOOS == "windows" {
		return "The shell is Windows PowerShell 5.1: use PowerShell syntax (`;` to chain, `$env:VAR`, no `&&`)."
	}
	return "The shell is POSIX sh."
}

func toolDef(name, desc, schema string) rt.ToolDef {
	return rt.ToolDef{Type: "function", Function: rt.ToolFuncDef{
		Name: name, Description: desc, Parameters: json.RawMessage(schema),
	}}
}

// mutating reports whether a tool changes files or runs commands.
func mutating(name string) bool {
	switch name {
	case "write_file", "edit_file", "run_shell":
		return true
	}
	return false
}

func (c *codingProvider) Execute(name, argsJSON string) (string, error) {
	if err := c.ctx.Err(); err != nil {
		return "", err
	}
	switch name {
	case "read_file":
		if err := c.permit(name, argsJSON, "", ""); err != nil {
			return "", err
		}
		return c.readFile(argsJSON)
	case "list_directory":
		if err := c.permit(name, argsJSON, "", ""); err != nil {
			return "", err
		}
		return c.listDir(argsJSON)
	case "glob_search":
		if err := c.permit(name, argsJSON, "", ""); err != nil {
			return "", err
		}
		return c.glob(argsJSON)
	case "grep_search":
		if err := c.permit(name, argsJSON, "", ""); err != nil {
			return "", err
		}
		return c.grep(argsJSON)
	case "write_file":
		return c.writeFile(argsJSON)
	case "edit_file":
		return c.editFile(argsJSON)
	case "run_shell":
		return c.runShell(argsJSON)
	case "todo_write":
		return c.todoWrite(argsJSON)
	default:
		return "", fmt.Errorf("unknown coding tool: %s", name)
	}
}

// permit applies policy rules, then the permission mode. summary/diff describe
// the action for the approval prompt.
func (c *codingProvider) permit(name, argsJSON, summary, diff string) error {
	if c.policy != nil {
		switch c.policy(name, argsJSON) {
		case PolicyDeny:
			return fmt.Errorf("denied by a permission rule for %s. Do not retry it; choose another approach or ask the user", name)
		case PolicyAllow:
			return nil
		}
	}
	if !mutating(name) {
		return nil
	}
	switch c.mode {
	case ModePlan:
		return fmt.Errorf("blocked: plan mode is read-only (no edits, no commands). Present your plan to the user instead; they can switch mode to apply it")
	case ModeAuto:
		return nil
	case ModeEdits:
		if name != "run_shell" {
			return nil
		}
	}
	if summary == "" {
		summary = riskSummary(name, argsJSON)
	}
	if !c.requestApproval(name, summary, diff, argsJSON) {
		return errors.New("denied by user. Do not retry the same action; ask the user how to proceed if needed")
	}
	return nil
}

func riskSummary(name, argsJSON string) string {
	var m map[string]interface{}
	json.Unmarshal([]byte(argsJSON), &m)
	switch name {
	case "run_shell":
		return fmt.Sprintf("Run command: %v", m["command"])
	case "write_file":
		return fmt.Sprintf("Write file: %v", m["path"])
	case "edit_file":
		return fmt.Sprintf("Edit file: %v", m["path"])
	}
	return name
}

// requestApproval asks the user. The CLI gets summary+diff in one string (first
// line = summary); the GUI gets them as separate fields.
func (c *codingProvider) requestApproval(tool, summary, diff, argsJSON string) bool {
	if c.approve != nil {
		full := summary
		if diff != "" {
			full += "\n" + diff
		}
		return c.approve(tool, full, argsJSON)
	}
	c.mu.Lock()
	c.counter++
	id := fmt.Sprintf("appr_%d_%d", time.Now().UnixNano(), c.counter)
	c.mu.Unlock()

	ch := registerApproval(id)
	if c.emit != nil {
		ev := map[string]interface{}{
			"id": id, "tool": tool, "summary": summary, "arguments": json.RawMessage(argsJSON),
		}
		if diff != "" {
			ev["diff"] = diff
		}
		c.emit("approval_request", ev)
	}
	select {
	case ok := <-ch:
		return ok
	case <-time.After(5 * time.Minute):
		resolveApproval(id, false)
		return false
	case <-c.ctx.Done():
		resolveApproval(id, false)
		return false
	}
}

// resolvePath maps a tool-supplied path into the workspace, enforcing containment
// when a root is configured.
func (c *codingProvider) resolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		if c.root != "" {
			return c.root, nil
		}
		return filepath.Abs(".")
	}
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		base := c.root
		if base == "" {
			base = "."
		}
		full = filepath.Clean(filepath.Join(base, p))
	}
	if c.root != "" {
		rel, err := filepath.Rel(c.root, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q is outside the workspace root %s", p, c.root)
		}
	}
	return full, nil
}

func (c *codingProvider) rel(full string) string {
	if c.root != "" {
		if r, err := filepath.Rel(c.root, full); err == nil {
			return filepath.ToSlash(r)
		}
	}
	return full
}

const (
	readDefaultLines = 2000
	readMaxLineLen   = 2000
)

func (c *codingProvider) readFile(argsJSON string) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Offset    int    `json:"offset"`
		Limit     int    `json:"limit"`
		LineStart int    `json:"line_start"` // legacy
		LineEnd   int    `json:"line_end"`   // legacy
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	full, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_directory", a.Path)
	}
	if fi.Size() > 20<<20 {
		return "", fmt.Errorf("%s is too large (%d bytes); use grep_search or read_file with offset/limit", a.Path, fi.Size())
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if isBinaryData(data) {
		return "", fmt.Errorf("%s is a binary file", a.Path)
	}
	c.state.markRead(full)

	if a.Offset == 0 && a.LineStart > 0 {
		a.Offset = a.LineStart
		if a.LineEnd >= a.LineStart {
			a.Limit = a.LineEnd - a.LineStart + 1
		}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "(empty file)", nil
	}
	start := a.Offset
	if start < 1 {
		start = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = readDefaultLines
	}
	if start > len(lines) {
		return "", fmt.Errorf("offset %d is past the end of the file (%d lines)", start, len(lines))
	}
	end := start + limit - 1
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		ln := lines[i-1]
		if len(ln) > readMaxLineLen {
			ln = ln[:readMaxLineLen] + "… [line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, ln)
	}
	if start > 1 || end < len(lines) {
		fmt.Fprintf(&b, "(lines %d-%d of %d; use offset/limit to read more)\n", start, end, len(lines))
	}
	return b.String(), nil
}

func isBinaryData(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func (c *codingProvider) listDir(argsJSON string) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	full, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", err
	}
	var dirs, files []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name()+"/")
		} else {
			files = append(files, e.Name())
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	all := append(dirs, files...)
	if len(all) > 500 {
		all = append(all[:500], fmt.Sprintf("… (%d more)", len(all)-500))
	}
	b, _ := json.Marshal(map[string]interface{}{"path": c.rel(full), "entries": all})
	return string(b), nil
}

// globToRegexp converts a glob (with **, *, ?, [..], {a,b}) matched against a
// slash-separated relative path into an anchored regexp.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(strings.TrimPrefix(glob, "./"))
	var b strings.Builder
	b.WriteString("^")
	inClass, braces := false, 0
	for i := 0; i < len(glob); i++ {
		ch := glob[i]
		switch {
		case inClass:
			if ch == ']' {
				inClass = false
			}
			b.WriteByte(ch)
		case ch == '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?") // **/ = zero or more directories
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case ch == '?':
			b.WriteString("[^/]")
		case ch == '[':
			inClass = true
			b.WriteByte('[')
		case ch == '{':
			braces++
			b.WriteString("(?:")
		case ch == '}' && braces > 0:
			braces--
			b.WriteString(")")
		case ch == ',' && braces > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// matchGlob reports whether rel (slash path) matches the glob. A glob without a
// slash also matches the base name anywhere ("*.go" finds files in subfolders).
func matchGlob(re *regexp.Regexp, glob, rel string) bool {
	if re.MatchString(rel) {
		return true
	}
	if !strings.Contains(glob, "/") {
		return re.MatchString(pathBase(rel))
	}
	return false
}

func pathBase(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

func (c *codingProvider) glob(argsJSON string) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	base, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	re, err := globToRegexp(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid glob: %w", err)
	}
	type hit struct {
		rel string
		mod time.Time
	}
	var hits []hit
	filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || c.ctx.Err() != nil {
			return nil
		}
		if d.IsDir() {
			if path != base && isNoiseDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		if matchGlob(re, a.Pattern, rel) {
			var mod time.Time
			if fi, err := d.Info(); err == nil {
				mod = fi.ModTime()
			}
			hits = append(hits, hit{rel, mod})
			if len(hits) >= 5000 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
	out := make([]string, 0, len(hits))
	for i, h := range hits {
		if i >= 200 {
			out = append(out, fmt.Sprintf("… (%d more; narrow the pattern)", len(hits)-200))
			break
		}
		out = append(out, h.rel)
	}
	if len(out) == 0 {
		return "No files found", nil
	}
	return strings.Join(out, "\n"), nil
}

func (c *codingProvider) grep(argsJSON string) (string, error) {
	var a struct {
		Pattern         string `json:"pattern"`
		Query           string `json:"query"` // legacy: literal substring
		Path            string `json:"path"`
		Include         string `json:"include"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	pat := a.Pattern
	if pat == "" && a.Query != "" {
		pat = regexp.QuoteMeta(a.Query)
	}
	if pat == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if a.CaseInsensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		// Models often pass a literal with regex metacharacters: search it literally.
		re = regexp.MustCompile(regexp.QuoteMeta(strings.TrimPrefix(pat, "(?i)")))
	}
	var incRE *regexp.Regexp
	if a.Include != "" {
		if incRE, err = globToRegexp(a.Include); err != nil {
			return "", fmt.Errorf("invalid include glob: %w", err)
		}
	}
	base, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	var hits []string
	total := 0
	searchFile := func(path string) {
		data, e := os.ReadFile(path)
		if e != nil || isBinaryData(data) {
			return
		}
		relName := c.rel(path)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				total++
				if len(hits) < 250 {
					line = strings.TrimRight(line, "\r")
					if len(line) > 300 {
						line = line[:300] + "…"
					}
					hits = append(hits, fmt.Sprintf("%s:%d: %s", relName, i+1, line))
				}
			}
		}
	}
	if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
		searchFile(base)
	} else {
		filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || c.ctx.Err() != nil {
				return nil
			}
			if d.IsDir() {
				if path != base && isNoiseDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if isBinaryName(path) {
				return nil
			}
			if incRE != nil {
				rel, _ := filepath.Rel(base, path)
				if !matchGlob(incRE, a.Include, filepath.ToSlash(rel)) {
					return nil
				}
			}
			if info, _ := d.Info(); info != nil && info.Size() > 4<<20 {
				return nil
			}
			searchFile(path)
			if total > 5000 {
				return filepath.SkipAll
			}
			return nil
		})
	}
	if len(hits) == 0 {
		return "No matches found", nil
	}
	out := strings.Join(hits, "\n")
	if total > len(hits) {
		out += fmt.Sprintf("\n… (%d matches in total; narrow the pattern or path)", total)
	}
	return out, nil
}

// checkWritable enforces read-before-overwrite on existing files, so the model
// never clobbers content it has not seen.
func (c *codingProvider) checkWritable(full, shown string) error {
	if _, err := os.Stat(full); err != nil {
		return nil // new file
	}
	if !c.state.wasRead(full) {
		return fmt.Errorf("%s already exists and has not been read in this session. Call read_file on it first, then retry", shown)
	}
	return nil
}

func (c *codingProvider) writeFile(argsJSON string) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	full, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	before, readErr := os.ReadFile(full)
	existed := readErr == nil
	if !a.Append {
		if err := c.checkWritable(full, a.Path); err != nil {
			return "", err
		}
	}
	after := a.Content
	if a.Append {
		after = string(before) + a.Content
	}
	diff, add, del := unifiedDiff(c.rel(full), string(before), after)
	summary := "Create file: " + c.rel(full)
	if existed {
		summary = "Overwrite file: " + c.rel(full)
		if a.Append {
			summary = "Append to file: " + c.rel(full)
		}
	}
	if err := c.permit("write_file", argsJSON, summary, diff); err != nil {
		return "", err
	}
	if c.onChange != nil {
		c.onChange(full, before, existed)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "", err
	}
	if a.Append {
		f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return "", err
		}
		_, err = f.WriteString(a.Content)
		f.Close()
		if err != nil {
			return "", err
		}
	} else if err := os.WriteFile(full, []byte(a.Content), 0644); err != nil {
		return "", err
	}
	c.state.markRead(full)
	c.emitDiff(full, diff, add, del, !existed)
	verb := "Wrote"
	switch {
	case a.Append:
		verb = "Appended to"
	case !existed:
		verb = "Created"
	}
	return fmt.Sprintf("%s %s (%d lines, +%d -%d)", verb, c.rel(full), strings.Count(after, "\n")+1, add, del), nil
}

func (c *codingProvider) editFile(argsJSON string) (string, error) {
	var a struct {
		Path       string  `json:"path"`
		OldString  *string `json:"old_string"`
		NewString  *string `json:"new_string"`
		OldText    *string `json:"old_text"` // legacy
		NewText    *string `json:"new_text"` // legacy
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	pick := func(p, q *string) (string, bool) {
		if p != nil {
			return *p, true
		}
		if q != nil {
			return *q, true
		}
		return "", false
	}
	oldS, okOld := pick(a.OldString, a.OldText)
	newS, okNew := pick(a.NewString, a.NewText)
	if !okOld || !okNew {
		return "", fmt.Errorf("old_string and new_string are required")
	}
	full, err := c.resolvePath(a.Path)
	if err != nil {
		return "", err
	}
	data, readErr := os.ReadFile(full)
	existed := readErr == nil

	var updated string
	switch {
	case !existed && oldS == "":
		updated = newS // create
	case !existed:
		return "", readErr
	default:
		if !c.state.wasRead(full) {
			return "", fmt.Errorf("read %s with read_file before editing it", a.Path)
		}
		if oldS == newS {
			return "", fmt.Errorf("old_string and new_string are identical; nothing to change")
		}
		if oldS == "" {
			return "", fmt.Errorf("old_string is empty; to rewrite the whole file use write_file")
		}
		content := string(data)
		// Files with CRLF line endings: models send LF.
		if strings.Contains(content, "\r\n") && !strings.Contains(oldS, "\r\n") {
			oldS = strings.ReplaceAll(oldS, "\n", "\r\n")
			newS = strings.ReplaceAll(newS, "\n", "\r\n")
		}
		n := strings.Count(content, oldS)
		switch {
		case n == 0:
			return "", fmt.Errorf("old_string not found in %s. It must match exactly, including whitespace and indentation; read the file again and copy the text", a.Path)
		case n > 1 && !a.ReplaceAll:
			return "", fmt.Errorf("old_string occurs %d times in %s. Add more surrounding context to make it unique, or set replace_all=true", n, a.Path)
		}
		if a.ReplaceAll {
			updated = strings.ReplaceAll(content, oldS, newS)
		} else {
			updated = strings.Replace(content, oldS, newS, 1)
		}
	}

	diff, add, del := unifiedDiff(c.rel(full), string(data), updated)
	if err := c.permit("edit_file", argsJSON, "Edit file: "+c.rel(full), diff); err != nil {
		return "", err
	}
	if c.onChange != nil {
		c.onChange(full, data, existed)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(updated), 0644); err != nil {
		return "", err
	}
	c.state.markRead(full)
	c.emitDiff(full, diff, add, del, !existed)
	return fmt.Sprintf("Edited %s (+%d -%d)", c.rel(full), add, del), nil
}

func (c *codingProvider) emitDiff(full, diff string, add, del int, created bool) {
	if c.emit == nil {
		return
	}
	c.emit("file_diff", map[string]interface{}{
		"path": c.rel(full), "diff": diff, "added": add, "removed": del, "created": created,
	})
}

const (
	shellDefaultTimeout = 2 * time.Minute
	shellMaxTimeout     = 10 * time.Minute
	shellMaxOutput      = 30000
)

func (c *codingProvider) runShell(argsJSON string) (string, error) {
	var a struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	if err := c.permit("run_shell", argsJSON, "Run command: "+a.Command, ""); err != nil {
		return "", err
	}
	timeout := shellDefaultTimeout
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
		if timeout > shellMaxTimeout {
			timeout = shellMaxTimeout
		}
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// UTF-8 output so non-ASCII text (and git/go output) is not mangled.
		script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; " + a.Command
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", a.Command)
	}
	rt.HideWindow(cmd)
	if c.root != "" {
		cmd.Dir = c.root
	}
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	res := string(out)
	if len(res) > shellMaxOutput {
		head, tail := res[:shellMaxOutput*2/3], res[len(res)-shellMaxOutput/3:]
		res = head + fmt.Sprintf("\n… [%d bytes omitted] …\n", len(res)-len(head)-len(tail)) + tail
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return res, fmt.Errorf("command timed out after %s", timeout)
		case c.ctx.Err() != nil:
			return res, c.ctx.Err()
		case errors.As(err, &ee):
			code = ee.ExitCode()
		default:
			return res, fmt.Errorf("could not run command: %v", err)
		}
	}
	if strings.TrimSpace(res) == "" {
		res = "(no output)"
	}
	return fmt.Sprintf("%s\n[exit code %d]", strings.TrimRight(res, "\n"), code), nil
}

func (c *codingProvider) todoWrite(argsJSON string) (string, error) {
	var a struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	done := 0
	for i := range a.Todos {
		switch a.Todos[i].Status {
		case "pending", "in_progress", "completed":
		default:
			a.Todos[i].Status = "pending"
		}
		if a.Todos[i].Status == "completed" {
			done++
		}
	}
	c.state.mu.Lock()
	c.state.Todos = a.Todos
	c.state.mu.Unlock()
	if c.emit != nil {
		c.emit("todo_update", map[string]interface{}{"todos": a.Todos})
	}
	return fmt.Sprintf("Todo list updated (%d/%d completed).", done, len(a.Todos)), nil
}

// MarkRead records that the user attached a file (@path), which counts as read.
func (s *CodingState) MarkRead(path string) { s.markRead(path) }

// Lock/Unlock guard Todos for readers outside the package.
func (s *CodingState) Lock()   { s.mu.Lock() }
func (s *CodingState) Unlock() { s.mu.Unlock() }

// UnifiedDiff is the diff used for approvals and file_diff events.
func UnifiedDiff(path, a, b string) (string, int, int) { return unifiedDiff(path, a, b) }
