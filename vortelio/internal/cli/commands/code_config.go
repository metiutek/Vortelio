package commands

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/config"
)

// ── Settings ─────────────────────────────────────────────────────────────────
//
// Global:  ~/.vortelio/code_settings.json
// Project: <workdir>/.vortelio/settings.json (overrides global; permission
//          rules from both files apply)

type codePermissions struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type codeSettings struct {
	Model         string          `json:"model,omitempty"`          // "qwen3.5:4b" or "cloud/<provider>/<model>"
	Mode          string          `json:"mode,omitempty"`           // plan | ask | edits | auto
	ShowThinking  *bool           `json:"show_thinking,omitempty"`  // stream the model's reasoning (default true)
	AutoCompact   *bool           `json:"auto_compact,omitempty"`   // summarize old context when it gets full (default true)
	ContextTokens int             `json:"context_tokens,omitempty"` // local model context window (default 16384)
	MaxTurns      int             `json:"max_turns,omitempty"`      // tool rounds per request (default 30)
	MediaTools    *bool           `json:"media_tools,omitempty"`    // expose image/audio/video/3D generation (default false)
	MCP           *bool           `json:"mcp,omitempty"`            // expose connected MCP tools (default false)
	Permissions   codePermissions `json:"permissions,omitempty"`
}

func globalSettingsPath() string { return filepath.Join(config.HomeDir(), "code_settings.json") }

func projectSettingsPath(dir string) string {
	return filepath.Join(dir, ".vortelio", "settings.json")
}

func readSettings(path string) (codeSettings, error) {
	var s codeSettings
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func writeSettings(path string, s codeSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// effectiveSettings merges global and project settings. Errors in either file
// are returned as warnings (the session still starts).
func effectiveSettings(dir string) (codeSettings, []string) {
	var warns []string
	g, err := readSettings(globalSettingsPath())
	if err != nil {
		warns = append(warns, err.Error())
	}
	migrateLegacyPrefs(&g)
	p, err := readSettings(projectSettingsPath(dir))
	if err != nil {
		warns = append(warns, err.Error())
	}
	e := g
	if p.Model != "" {
		e.Model = p.Model
	}
	if p.Mode != "" {
		e.Mode = p.Mode
	}
	if p.ShowThinking != nil {
		e.ShowThinking = p.ShowThinking
	}
	if p.AutoCompact != nil {
		e.AutoCompact = p.AutoCompact
	}
	if p.ContextTokens > 0 {
		e.ContextTokens = p.ContextTokens
	}
	if p.MaxTurns > 0 {
		e.MaxTurns = p.MaxTurns
	}
	if p.MediaTools != nil {
		e.MediaTools = p.MediaTools
	}
	if p.MCP != nil {
		e.MCP = p.MCP
	}
	e.Permissions.Allow = append(append([]string{}, g.Permissions.Allow...), p.Permissions.Allow...)
	e.Permissions.Deny = append(append([]string{}, g.Permissions.Deny...), p.Permissions.Deny...)
	if !validMode(e.Mode) {
		if e.Mode != "" {
			warns = append(warns, fmt.Sprintf("unknown mode %q in settings (use plan, ask, edits or auto)", e.Mode))
		}
		e.Mode = "ask"
	}
	return e, warns
}

// migrateLegacyPrefs reads the pre-0.3.88 ~/.vortelio/code_session.json once.
func migrateLegacyPrefs(g *codeSettings) {
	if g.Model != "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(config.HomeDir(), "code_session.json"))
	if err != nil {
		return
	}
	var old struct {
		ModelName     string `json:"model_name"`
		ModelTag      string `json:"model_tag"`
		CloudProvider string `json:"cloud_provider"`
		CloudModel    string `json:"cloud_model"`
	}
	if json.Unmarshal(data, &old) != nil {
		return
	}
	switch {
	case old.CloudProvider != "" && old.CloudModel != "":
		g.Model = "cloud/" + old.CloudProvider + "/" + old.CloudModel
	case old.ModelName != "":
		g.Model = old.ModelName + ":" + old.ModelTag
	}
}

// updateGlobalSettings applies fn to the global settings file and saves it.
func updateGlobalSettings(fn func(*codeSettings)) error {
	s, err := readSettings(globalSettingsPath())
	if err != nil {
		return err
	}
	fn(&s)
	return writeSettings(globalSettingsPath(), s)
}

func updateProjectSettings(dir string, fn func(*codeSettings)) error {
	s, err := readSettings(projectSettingsPath(dir))
	if err != nil {
		return err
	}
	fn(&s)
	return writeSettings(projectSettingsPath(dir), s)
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func validMode(m string) bool {
	switch m {
	case "plan", "ask", "edits", "auto":
		return true
	}
	return false
}

// ── Permission rules ─────────────────────────────────────────────────────────
//
// A rule is "Tool" or "Tool(pattern)". Tool is a Vortelio tool name
// (run_shell, edit_file, write_file, read_file, …) or a Claude Code alias
// (Bash, Edit, Write, Read, Glob, Grep, WebFetch). The pattern is matched
// against the shell command or the file path, with * as a wildcard (a trailing
// ":*" is accepted as in Claude Code). Deny rules win over allow rules.

var toolAliasRule = map[string]string{
	"bash": "run_shell", "shell": "run_shell", "edit": "edit_file", "write": "write_file",
	"read": "read_file", "glob": "glob_search", "grep": "grep_search", "ls": "list_directory",
	"webfetch": "fetch_url", "websearch": "web_search", "multiedit": "edit_file",
}

var ruleRE = regexp.MustCompile(`^\s*([A-Za-z_]+)\s*(?:\((.*)\))?\s*$`)

type permRule struct {
	tool    string
	pattern string // "" = any
}

func parseRule(s string) (permRule, bool) {
	m := ruleRE.FindStringSubmatch(s)
	if m == nil {
		return permRule{}, false
	}
	tool := strings.ToLower(m[1])
	if a, ok := toolAliasRule[tool]; ok {
		tool = a
	}
	pat := strings.TrimSpace(m[2])
	pat = strings.TrimSuffix(pat, ":*")
	if strings.HasSuffix(strings.TrimSpace(m[2]), ":*") {
		pat += "*"
	}
	return permRule{tool: tool, pattern: pat}, true
}

// ruleSubject is what a rule pattern is matched against for a tool call.
func ruleSubject(tool, argsJSON string) string {
	var a map[string]interface{}
	json.Unmarshal([]byte(argsJSON), &a)
	for _, k := range []string{"command", "path", "url", "pattern", "query"} {
		if v, ok := a[k].(string); ok {
			if k == "path" {
				return filepath.ToSlash(v)
			}
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func wildcardMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

func (r permRule) matches(tool, argsJSON string) bool {
	if r.tool != tool {
		return false
	}
	if r.pattern == "" || r.pattern == "*" {
		return true
	}
	return wildcardMatch(r.pattern, ruleSubject(tool, argsJSON))
}

// decide returns "deny", "allow" or "" for a tool call.
func (p codePermissions) decide(tool, argsJSON string) string {
	for _, s := range p.Deny {
		if r, ok := parseRule(s); ok && r.matches(tool, argsJSON) {
			return "deny"
		}
	}
	for _, s := range p.Allow {
		if r, ok := parseRule(s); ok && r.matches(tool, argsJSON) {
			return "allow"
		}
	}
	return ""
}

// suggestShellRule proposes an allow rule for a command: its first two words
// ("go test ./x" → "run_shell(go test*)").
func suggestShellRule(command string) string {
	f := strings.Fields(command)
	if len(f) == 0 {
		return ""
	}
	prefix := f[0]
	if len(f) > 1 && !strings.HasPrefix(f[1], "-") {
		prefix += " " + f[1]
	}
	return "run_shell(" + prefix + "*)"
}

// ── Sessions ─────────────────────────────────────────────────────────────────

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type savedSession struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Dir      string    `json:"dir"`
	Model    string    `json:"model"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages []chatMsg `json:"messages"`
}

func sessionsDir() string { return filepath.Join(config.HomeDir(), "code_sessions") }

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func saveSession(s *savedSession) error {
	if len(s.Messages) == 0 {
		return nil
	}
	s.Updated = time.Now()
	if err := os.MkdirAll(sessionsDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(sessionsDir(), s.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(sessionsDir(), s.ID+".json"))
}

func loadSession(id string) (*savedSession, error) {
	data, err := os.ReadFile(filepath.Join(sessionsDir(), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("session %s not found", id)
	}
	var s savedSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// listSessions returns saved sessions, newest first; dir != "" keeps only the
// sessions started in that folder.
func listSessions(dir string) []*savedSession {
	entries, _ := os.ReadDir(sessionsDir())
	var out []*savedSession
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := loadSession(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if dir != "" && !samePath(s.Dir, dir) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// ── Instruction files (AGENTS.md & co.) ──────────────────────────────────────

type instrFile struct {
	Path    string
	Content string
}

var instructionNames = []string{"AGENTS.md", "VORTELIO.md", "CLAUDE.md"}

// loadInstructions collects the project instructions the agent must follow:
// ~/.vortelio/AGENTS.md, then AGENTS.md / VORTELIO.md / CLAUDE.md from the git
// root (or the working dir) down to the working dir, plus a legacy PROJECT.md.
func loadInstructions(workdir string) []instrFile {
	var out []instrFile
	add := func(p string) {
		data, err := os.ReadFile(p)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			return
		}
		c := string(data)
		if len(c) > 24000 {
			c = c[:24000] + "\n… (truncated)"
		}
		for _, f := range out {
			if samePath(f.Path, p) {
				return
			}
		}
		out = append(out, instrFile{Path: p, Content: c})
	}
	add(filepath.Join(config.HomeDir(), "AGENTS.md"))

	dirs := []string{workdir}
	if root := gitRoot(workdir); root != "" && !samePath(root, workdir) {
		for d := filepath.Dir(workdir); ; d = filepath.Dir(d) {
			dirs = append([]string{d}, dirs...)
			if samePath(d, root) || filepath.Dir(d) == d {
				break
			}
		}
	}
	for _, d := range dirs {
		for _, n := range instructionNames {
			add(filepath.Join(d, n))
		}
	}
	if len(out) == 0 || !hasProjectInstr(out, workdir) {
		add(filepath.Join(workdir, "PROJECT.md"))
	}
	return out
}

func hasProjectInstr(files []instrFile, workdir string) bool {
	for _, f := range files {
		if samePath(filepath.Dir(f.Path), workdir) {
			return true
		}
	}
	return false
}

func gitRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

// ── Custom slash commands ────────────────────────────────────────────────────
//
// Markdown files in ~/.vortelio/commands/ and <project>/.vortelio/commands/
// become /<name> commands. The body is the prompt; $ARGUMENTS is replaced by
// everything after the command, $1…$9 by single words. An optional frontmatter
// "description:" line is shown in /help.

type customCommand struct {
	Name        string
	Description string
	Body        string
	Path        string
}

func loadCustomCommands(workdir string) map[string]customCommand {
	out := map[string]customCommand{}
	for _, dir := range []string{filepath.Join(config.HomeDir(), "commands"), filepath.Join(workdir, ".vortelio", "commands")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			name := strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			desc, body := parseFrontmatter(string(data))
			out[name] = customCommand{Name: name, Description: desc, Body: body, Path: p}
		}
	}
	return out
}

func parseFrontmatter(s string) (desc, body string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.HasPrefix(s, "---\n") {
		if end := strings.Index(s[4:], "\n---"); end >= 0 {
			for _, line := range strings.Split(s[4:4+end], "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "description" {
					desc = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
			rest := s[4+end+4:]
			return desc, strings.TrimLeft(rest, "\n")
		}
	}
	return "", s
}

func (c customCommand) expand(args string) string {
	body := strings.ReplaceAll(c.Body, "$ARGUMENTS", args)
	words := strings.Fields(args)
	for i := 9; i >= 1; i-- {
		v := ""
		if i <= len(words) {
			v = words[i-1]
		}
		body = strings.ReplaceAll(body, fmt.Sprintf("$%d", i), v)
	}
	if !strings.Contains(c.Body, "$ARGUMENTS") && !regexp.MustCompile(`\$[1-9]`).MatchString(c.Body) && args != "" {
		body += "\n\n" + args
	}
	return body
}
