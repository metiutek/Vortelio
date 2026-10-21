package runtime

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/docs"
)

// ── Tool definitions (OpenAI-compatible schema) ──────────────────────────────

// ToolDef describes a tool that can be offered to the LLM.
type ToolDef struct {
	Type     string      `json:"type"` // always "function"
	Function ToolFuncDef `json:"function"`
}

type ToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a single tool invocation requested by the LLM.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ToolResult is broadcast to the UI for display.
type ToolResult struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// ── Tool provider abstraction ────────────────────────────────────────────────

// ToolProvider supplies the set of tools available to a single chat request and
// executes them. This lets each request (assistant chat, coding agent, MCP-enabled
// session, skills) expose a different toolset without a coarse global registry.
type ToolProvider interface {
	// Tools returns the OpenAI-schema tool definitions offered to the model.
	Tools() []ToolDef
	// Execute runs the named tool with JSON arguments and returns a result string.
	Execute(name, argsJSON string) (string, error)
}

// builtinProvider is the default provider backed by BuiltinTools/ExecuteTool.
type builtinProvider struct{}

func (builtinProvider) Tools() []ToolDef                          { return BuiltinTools() }
func (builtinProvider) Execute(name, args string) (string, error) { return ExecuteTool(name, args) }

// DefaultToolProvider returns the builtin tool provider.
func DefaultToolProvider() ToolProvider { return builtinProvider{} }

// CompositeProvider merges several providers into one, concatenating their tools
// and dispatching Execute to whichever provider declares the named tool.
type CompositeProvider struct {
	providers []ToolProvider
}

// NewCompositeProvider builds a provider from the given (non-nil) providers.
func NewCompositeProvider(ps ...ToolProvider) *CompositeProvider {
	var out []ToolProvider
	for _, p := range ps {
		if p != nil {
			out = append(out, p)
		}
	}
	return &CompositeProvider{providers: out}
}

func (c *CompositeProvider) Tools() []ToolDef {
	var all []ToolDef
	for _, p := range c.providers {
		all = append(all, p.Tools()...)
	}
	return all
}

// toolAliases maps common names models hallucinate (used to other agents) onto
// the real tool names, so the call succeeds instead of wasting a round.
var toolAliases = map[string]string{
	"search":     "grep_search",
	"grep":       "grep_search",
	"find":       "glob_search",
	"glob":       "glob_search",
	"tree":       "list_directory",
	"print_tree": "list_directory",
	"ls":         "list_directory",
	"cat":        "read_file",
	"read":       "read_file",
}

func (c *CompositeProvider) Execute(name, args string) (string, error) {
	if alias, ok := toolAliases[name]; ok {
		// Only remap if a provider actually offers the canonical tool.
		for _, p := range c.providers {
			for _, t := range p.Tools() {
				if t.Function.Name == alias {
					name = alias
				}
			}
		}
	}
	for _, p := range c.providers {
		for _, t := range p.Tools() {
			if t.Function.Name == name {
				return p.Execute(name, args)
			}
		}
	}
	// List the real tool names so a model that hallucinated a name (e.g. "search",
	// "print_tree") can correct itself on the next round instead of looping on the
	// same invalid call until it runs out of rounds.
	var names []string
	for _, p := range c.providers {
		for _, t := range p.Tools() {
			names = append(names, t.Function.Name)
		}
	}
	return "", fmt.Errorf("unknown tool %q. Available tools: %s", name, strings.Join(names, ", "))
}

// ── Built-in tool catalog ────────────────────────────────────────────────────

// BuiltinTools returns the list of tool definitions available for server-side tool-use.
func BuiltinTools() []ToolDef {
	return []ToolDef{
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "get_current_time",
				Description: "Returns the current date and time with timezone.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"format":{"type":"string","description":"Optional Go time layout string. Default is RFC3339."}},"required":[]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "calculator",
				Description: "Evaluates a mathematical expression. Supports +, -, *, /, parentheses, sqrt, pow, abs, sin, cos, tan, log, exp, floor, ceil, round, max, min, pi, e.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string","description":"The math expression, e.g. '(3+5)*2' or 'sqrt(144)'"}},"required":["expression"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "web_search",
				Description: "Searches the web for current information, news, or facts.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"The search query"}},"required":["query"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "read_file",
				Description: "Reads a text file. Optionally read only a line range with line_start/line_end (1-based, inclusive) to navigate large files.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Absolute or relative path to the file"},"line_start":{"type":"integer","description":"First line to return (1-based, inclusive). Optional."},"line_end":{"type":"integer","description":"Last line to return (1-based, inclusive). Optional."}},"required":["path"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "write_file",
				Description: "Writes content to a file. Creates it if missing. Set append=true to add to the end instead of overwriting — use this to build large files (e.g. a big dataset) in several chunks.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"content":{"type":"string","description":"Content to write"},"append":{"type":"boolean","description":"If true, append to the end of the file instead of overwriting. Default false."}},"required":["path","content"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "list_directory",
				Description: "Lists files and directories at the given path.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path to list. Defaults to current directory.","default":"."}},"required":[]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "fetch_url",
				Description: "Fetch a web page or document by URL and return its text content (HTML is stripped to readable text). Use to open and read links the user provides.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The full URL to fetch, e.g. https://example.com/page"}},"required":["url"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "deep_research",
				Description: "Do thorough web research on a topic: runs a web search and reads the top result pages, returning combined excerpts to synthesize a well-sourced answer.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"The research topic or question"}},"required":["query"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "create_document",
				Description: "Create a document file on disk in a given format. Supports: pdf, docx, txt, md, html, csv, json. Use to deliver reports, letters, notes, etc.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"format":{"type":"string","description":"pdf | docx | txt | md | html | csv | json"},"path":{"type":"string","description":"Output file path (with the right extension)"},"title":{"type":"string","description":"Optional document title/heading"},"content":{"type":"string","description":"The document body (plain text or markdown). Use \\n for new lines."}},"required":["format","path","content"]}`),
			},
		},
		{
			Type: "function",
			Function: ToolFuncDef{
				Name:        "run_code",
				Description: "Execute a code snippet and return its output. Supports python, javascript, bash, powershell, go, ruby, php, c, cpp, java. Use to compute, test, or run code to accomplish a task.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"language":{"type":"string","description":"python | javascript | bash | powershell | go | ruby | php | c | cpp | java"},"code":{"type":"string","description":"The source code to run"}},"required":["language","code"]}`),
			},
		},
	}
}

// ── Tool executor ────────────────────────────────────────────────────────────

// ExecuteTool runs the named tool with the given JSON arguments and returns a result string.
func ExecuteTool(name string, argsJSON string) (string, error) {
	switch name {
	case "get_current_time":
		return toolGetCurrentTime(argsJSON)
	case "calculator":
		return toolCalculator(argsJSON)
	case "web_search":
		return toolWebSearch(argsJSON)
	case "read_file":
		return toolReadFile(argsJSON)
	case "write_file":
		return toolWriteFile(argsJSON)
	case "list_directory":
		return toolListDirectory(argsJSON)
	case "fetch_url":
		return toolFetchURL(argsJSON)
	case "deep_research":
		return toolDeepResearch(argsJSON)
	case "create_document":
		return toolCreateDocument(argsJSON)
	case "run_code":
		return toolRunCode(argsJSON)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ── deep_research ────────────────────────────────────────────────────────────

func toolDeepResearch(argsJSON string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	results, err := WebSearch(a.Query, 5)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("RICERCA: " + a.Query + "\n\n")
	fetched := 0
	for _, r := range results {
		b.WriteString("• " + r.Title + " — " + r.URL + "\n  " + r.Snippet + "\n")
		if fetched < 3 && r.URL != "" {
			if txt := fetchURLText(r.URL); txt != "" {
				if len(txt) > 1800 {
					txt = txt[:1800] + "…"
				}
				b.WriteString("  [contenuto] " + strings.ReplaceAll(txt, "\n", " ") + "\n")
				fetched++
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ── create_document ──────────────────────────────────────────────────────────

func toolCreateDocument(argsJSON string) (string, error) {
	var a struct {
		Format  string `json:"format"`
		Path    string `json:"path"`
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	// A bare filename (no directory) is saved to the user's Downloads folder so
	// the document is actually findable, instead of the server's working dir.
	if filepath.Dir(a.Path) == "." {
		a.Path = filepath.Join(DefaultOutputDir(), a.Path)
	}
	if dir := filepath.Dir(a.Path); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	p, err := CreateDocument(a.Format, a.Path, a.Title, a.Content)
	if err != nil {
		return "", err
	}
	if abs, e := filepath.Abs(p); e == nil {
		p = abs
	}
	fi, _ := os.Stat(p)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}
	b, _ := json.Marshal(map[string]interface{}{
		"status": "ok", "path": p, "size": size,
		"note": "Document saved. The user can open/download it from the path above.",
	})
	return string(b), nil
}

// ── run_code ─────────────────────────────────────────────────────────────────

func toolRunCode(argsJSON string) (string, error) {
	var a struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Code) == "" {
		return "", fmt.Errorf("code is required")
	}
	// Models frequently omit the language; default to Python (the most common)
	// instead of failing the round with "unsupported language".
	if strings.TrimSpace(a.Language) == "" {
		a.Language = "python"
	}
	out, err := RunCodeSnippet(a.Language, a.Code)
	if err != nil {
		return fmt.Sprintf("Errore: %v\n%s", err, out), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(nessun output)"
	}
	return out, nil
}

// ── fetch_url ────────────────────────────────────────────────────────────────

func toolFetchURL(argsJSON string) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u := strings.TrimSpace(args.URL)
	if u == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	text := fetchURLText(u)
	if text == "" {
		return "", fmt.Errorf("could not fetch or page was empty")
	}
	if len(text) > 8000 {
		text = text[:8000] + "\n…[contenuto troncato]"
	}
	out := map[string]interface{}{"url": u, "content": text}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// fetchURLText downloads a URL and returns readable text (HTML stripped).
func fetchURLText(u string) string {
	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Vortelio)")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return htmlToText(string(body))
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reTags        = regexp.MustCompile(`(?s)<[^>]+>`)
	reWS          = regexp.MustCompile(`[ \t]+`)
	reBlankLines  = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

func htmlToText(h string) string {
	h = reScriptStyle.ReplaceAllString(h, " ")
	h = reTags.ReplaceAllString(h, " ")
	h = htmlUnescape(h)
	h = reWS.ReplaceAllString(h, " ")
	h = reBlankLines.ReplaceAllString(h, "\n\n")
	return strings.TrimSpace(h)
}

func htmlUnescape(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&nbsp;", " ")
	return r.Replace(s)
}

// ── get_current_time ─────────────────────────────────────────────────────────

func toolGetCurrentTime(argsJSON string) (string, error) {
	var args struct {
		Format string `json:"format"`
	}
	json.Unmarshal([]byte(argsJSON), &args)

	now := time.Now()
	layout := time.RFC3339
	if args.Format != "" && args.Format != "RFC3339" {
		layout = args.Format
	}

	result := map[string]string{
		"datetime":    now.Format(layout),
		"unix":        strconv.FormatInt(now.Unix(), 10),
		"timezone":    now.Location().String(),
		"day_of_week": now.Weekday().String(),
	}
	b, _ := json.Marshal(result)
	return string(b), nil
}

// ── calculator ───────────────────────────────────────────────────────────────

func toolCalculator(argsJSON string) (string, error) {
	var args struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Expression == "" {
		return "", fmt.Errorf("expression is required")
	}

	result, err := evalMathExpr(args.Expression)
	if err != nil {
		return "", fmt.Errorf("evaluation error: %w", err)
	}

	out := map[string]interface{}{
		"expression": args.Expression,
		"result":     result,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// evalMathExpr evaluates simple math expressions safely using Go's AST parser.
// It supports: +, -, *, /, parentheses, and function calls sqrt/pow/abs/sin/cos/tan/log/exp/floor/ceil.
func evalMathExpr(expr string) (float64, error) {
	// Preprocess: replace common math function names with Go-parseable constructs
	// We'll parse as a Go expression and evaluate the AST
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, fmt.Errorf("cannot parse expression %q: %w", expr, err)
	}
	return evalNode(node)
}

func evalNode(node ast.Expr) (float64, error) {
	switch n := node.(type) {
	case *ast.BasicLit:
		if n.Kind == token.INT || n.Kind == token.FLOAT {
			return strconv.ParseFloat(n.Value, 64)
		}
		return 0, fmt.Errorf("unsupported literal: %s", n.Value)

	case *ast.UnaryExpr:
		val, err := evalNode(n.X)
		if err != nil {
			return 0, err
		}
		if n.Op == token.SUB {
			return -val, nil
		}
		if n.Op == token.ADD {
			return val, nil
		}
		return 0, fmt.Errorf("unsupported unary op: %s", n.Op)

	case *ast.BinaryExpr:
		left, err := evalNode(n.X)
		if err != nil {
			return 0, err
		}
		right, err := evalNode(n.Y)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return left / right, nil
		case token.REM:
			if right == 0 {
				return 0, fmt.Errorf("modulo by zero")
			}
			return math.Mod(left, right), nil
		default:
			return 0, fmt.Errorf("unsupported binary op: %s", n.Op)
		}

	case *ast.ParenExpr:
		return evalNode(n.X)

	case *ast.CallExpr:
		// Function calls like sqrt(x), pow(x,y), etc.
		ident, ok := n.Fun.(*ast.Ident)
		if !ok {
			return 0, fmt.Errorf("unsupported function call")
		}
		fname := strings.ToLower(ident.Name)
		args := make([]float64, len(n.Args))
		for i, a := range n.Args {
			v, err := evalNode(a)
			if err != nil {
				return 0, err
			}
			args[i] = v
		}
		return evalFunc(fname, args)

	case *ast.Ident:
		// Constants
		switch strings.ToLower(n.Name) {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		default:
			return 0, fmt.Errorf("unknown identifier: %s", n.Name)
		}

	default:
		return 0, fmt.Errorf("unsupported expression type: %T", node)
	}
}

func evalFunc(name string, args []float64) (float64, error) {
	switch name {
	case "sqrt":
		if len(args) != 1 {
			return 0, fmt.Errorf("sqrt requires 1 argument")
		}
		return math.Sqrt(args[0]), nil
	case "pow":
		if len(args) != 2 {
			return 0, fmt.Errorf("pow requires 2 arguments")
		}
		return math.Pow(args[0], args[1]), nil
	case "abs":
		if len(args) != 1 {
			return 0, fmt.Errorf("abs requires 1 argument")
		}
		return math.Abs(args[0]), nil
	case "sin":
		if len(args) != 1 {
			return 0, fmt.Errorf("sin requires 1 argument")
		}
		return math.Sin(args[0]), nil
	case "cos":
		if len(args) != 1 {
			return 0, fmt.Errorf("cos requires 1 argument")
		}
		return math.Cos(args[0]), nil
	case "tan":
		if len(args) != 1 {
			return 0, fmt.Errorf("tan requires 1 argument")
		}
		return math.Tan(args[0]), nil
	case "log":
		if len(args) != 1 {
			return 0, fmt.Errorf("log requires 1 argument")
		}
		return math.Log(args[0]), nil
	case "log10":
		if len(args) != 1 {
			return 0, fmt.Errorf("log10 requires 1 argument")
		}
		return math.Log10(args[0]), nil
	case "exp":
		if len(args) != 1 {
			return 0, fmt.Errorf("exp requires 1 argument")
		}
		return math.Exp(args[0]), nil
	case "floor":
		if len(args) != 1 {
			return 0, fmt.Errorf("floor requires 1 argument")
		}
		return math.Floor(args[0]), nil
	case "ceil":
		if len(args) != 1 {
			return 0, fmt.Errorf("ceil requires 1 argument")
		}
		return math.Ceil(args[0]), nil
	case "round":
		if len(args) != 1 {
			return 0, fmt.Errorf("round requires 1 argument")
		}
		return math.Round(args[0]), nil
	case "max":
		if len(args) != 2 {
			return 0, fmt.Errorf("max requires 2 arguments")
		}
		return math.Max(args[0], args[1]), nil
	case "min":
		if len(args) != 2 {
			return 0, fmt.Errorf("min requires 2 arguments")
		}
		return math.Min(args[0], args[1]), nil
	default:
		return 0, fmt.Errorf("unknown function: %s", name)
	}
}

// ── read_file ────────────────────────────────────────────────────────────────

func toolReadFile(argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
		// Accept several common aliases models use for line ranges so navigation
		// works instead of silently re-reading the file head.
		LineStart *int `json:"line_start"`
		LineEnd   *int `json:"line_end"`
		Offset    *int `json:"offset"`
		Limit     *int `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	var content string
	if docs.Supported(args.Path) { // PDF, Word, … → their text
		d, err := docs.Extract(args.Path)
		if err != nil {
			return "", fmt.Errorf("cannot read %q: %w", args.Path, err)
		}
		content = d.Text
	} else {
		data, err := os.ReadFile(args.Path)
		if err != nil {
			return "", fmt.Errorf("cannot read file %q: %w", args.Path, err)
		}
		content = string(data)
	}
	lines := strings.Split(content, "\n")
	total := len(lines)

	// Resolve a 1-based inclusive [start,end] window from whichever params were
	// given (line_start/line_end, or offset+limit).
	start, end := 1, total
	if args.LineStart != nil {
		start = *args.LineStart
	} else if args.Offset != nil {
		start = *args.Offset + 1
	}
	if args.LineEnd != nil {
		end = *args.LineEnd
	} else if args.Limit != nil {
		end = start + *args.Limit - 1
	}
	ranged := args.LineStart != nil || args.LineEnd != nil || args.Offset != nil || args.Limit != nil
	if start < 1 {
		start = 1
	}
	if end > total {
		end = total
	}
	if ranged && start <= end {
		content = strings.Join(lines[start-1:end], "\n")
	}

	const maxLen = 32000
	truncated := false
	if len(content) > maxLen {
		content = content[:maxLen]
		truncated = true
	}
	// Return RAW text, not JSON-wrapped: JSON escaping (\n, \") roughly doubles
	// the tokens of source code. A tiny bracketed header carries the metadata only
	// when it's actually useful (a partial or truncated read).
	var header string
	if ranged {
		header += fmt.Sprintf("[%s · lines %d-%d of %d]\n", args.Path, start, end, total)
	}
	if truncated {
		header += fmt.Sprintf("[truncated to %d chars — request a smaller line range]\n", maxLen)
	}
	return header + content, nil
}

// ── write_file ───────────────────────────────────────────────────────────────

func toolWriteFile(argsJSON string) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return "", fmt.Errorf("path and content are required")
	}
	if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil {
		return "", fmt.Errorf("cannot create directory: %w", err)
	}
	if args.Append {
		f, err := os.OpenFile(args.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return "", fmt.Errorf("cannot open file %q: %w", args.Path, err)
		}
		if _, err := f.WriteString(args.Content); err != nil {
			f.Close()
			return "", fmt.Errorf("cannot append to file %q: %w", args.Path, err)
		}
		f.Close()
	} else if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("cannot write file %q: %w", args.Path, err)
	}
	total := int64(0)
	if fi, err := os.Stat(args.Path); err == nil {
		total = fi.Size()
	}
	b, _ := json.Marshal(map[string]interface{}{
		"path":       args.Path,
		"written":    len(args.Content),
		"total_size": total,
		"appended":   args.Append,
		"status":     "ok",
	})
	return string(b), nil
}

// ── list_directory ───────────────────────────────────────────────────────────

func toolListDirectory(argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	json.Unmarshal([]byte(argsJSON), &args)
	dir := args.Path
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("cannot list directory %q: %w", dir, err)
	}
	// Compact, token-efficient listing instead of an array of repeated
	// {name,type,size} objects: directories end with "/", files show their size.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s (%d items) — dirs end with /, files show size in bytes:\n", dir, len(entries)))
	for _, e := range entries {
		if e.IsDir() {
			sb.WriteString(e.Name() + "/\n")
			continue
		}
		var sz int64
		if info, err := e.Info(); err == nil {
			sz = info.Size()
		}
		sb.WriteString(fmt.Sprintf("%s\t%d\n", e.Name(), sz))
	}
	return sb.String(), nil
}

// ── web_search ───────────────────────────────────────────────────────────────

func toolWebSearch(argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	results, err := WebSearch(args.Query, 4)
	if err != nil {
		out := map[string]interface{}{
			"query":  args.Query,
			"status": "error",
			"error":  err.Error(),
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	// Trim snippets to keep the result compact in the model's context window.
	for i := range results {
		if len(results[i].Snippet) > 160 {
			results[i].Snippet = results[i].Snippet[:160] + "…"
		}
	}
	out := map[string]interface{}{
		"query":   args.Query,
		"status":  "ok",
		"results": results,
	}
	if len(results) == 0 {
		out["message"] = "No results found."
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}
