package project_diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	maxOutputBytes = 64 * 1024
	maxFiles       = 128
	maxWalkEntries = 2048
	maxSourceBytes = 64 * 1024
)

type Deps struct {
	AllowedPaths []string
	Registry     protocol.ToolRegistry
}

type arguments struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Timeout  int    `json:"timeout"`
}

type Diagnostic struct {
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type result struct {
	Status      string       `json:"status"`
	Language    string       `json:"language"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Raw         string       `json:"raw,omitempty"`
	ExitCode    int          `json:"exit_code"`
	Error       string       `json:"error,omitempty"`
}

// MakeProjectDiagnosticsFn 使用 ExecuteTool 保留统一策略与污点链。
func MakeProjectDiagnosticsFn(deps Deps) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		out, err := run(ctx, spec.Input, spec.TaintLevel, deps)
		if err != nil {
			return &types.ToolResult{Success: false, Error: err.Error(), TaintLevel: types.TaintHigh}, err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "project_diagnostics: encode result", err)
		}
		success := out.Status != "denied" && out.Status != "unavailable" && out.Status != "truncated"
		return &types.ToolResult{Success: success, Output: encoded, Error: out.Error, TaintLevel: types.TaintHigh}, nil
	}
}

func run(ctx context.Context, input []byte, taint types.TaintLevel, deps Deps) (*result, error) {
	args, root, lang, ambiguous, err := prepare(ctx, input, deps)
	if err != nil {
		return nil, err
	}
	if ambiguous {
		return &result{Status: "selection_required", Language: "auto", Diagnostics: []Diagnostic{{Severity: "info", Message: "Multiple project languages found; choose go, rust, typescript, or python."}}}, nil
	}
	if args.Timeout <= 0 || args.Timeout > 120 {
		args.Timeout = 30
	}
	command, available, truncated, err := buildCommand(root, lang, ctx, deps.AllowedPaths)
	if err != nil {
		return nil, err
	}
	if !available {
		return &result{Status: "unavailable", Language: lang, Error: "required compiler or project files are unavailable", Diagnostics: []Diagnostic{{Severity: "warning", Message: "Required compiler or project files are unavailable."}}}, nil
	}
	return execute(ctx, args.Path, root, lang, command, args.Timeout, truncated, taint, deps.Registry)
}

func prepare(ctx context.Context, input []byte, deps Deps) (arguments, string, string, bool, error) {
	var args arguments
	if err := json.Unmarshal(input, &args); err != nil {
		return arguments{}, "", "", false, apperr.Wrap(apperr.CodeInvalidInput, "project_diagnostics: invalid input", err)
	}
	if deps.Registry == nil {
		return arguments{}, "", "", false, apperr.New(apperr.CodeServiceUnavailable, "project_diagnostics: tool registry unavailable")
	}
	root, err := validateRoot(ctx, args.Path, deps.AllowedPaths)
	if err != nil {
		return arguments{}, "", "", false, err
	}
	lang, ambiguous, err := selectLanguage(ctx, root, args.Language, deps.AllowedPaths)
	if err != nil {
		return arguments{}, "", "", false, err
	}
	return args, root, lang, ambiguous, nil
}

func execute(ctx context.Context, workDir, root, lang, command string, timeout int, truncated bool, taint types.TaintLevel, registry protocol.ToolRegistry) (*result, error) {
	callCtx := context.WithValue(ctx, protocol.CtxTaintLevelKey{}, maxTaint(taint, contextTaint(ctx)))
	callCtx = protocol.WithPermissionGrant(callCtx, protocol.GrantNone)
	callCtx, cancel := context.WithTimeout(callCtx, time.Duration(timeout)*time.Second)
	defer cancel()
	argsJSON, _ := json.Marshal(map[string]any{"command": command, "working_dir": filepath.Clean(workDir), "timeout_s": timeout})
	grant, reason, err := authorize(callCtx, registry, "run_command", argsJSON, maxTaint(taint, contextTaint(ctx)))
	if err != nil {
		return &result{Status: "denied", Language: lang, Error: reason, Raw: command + "\n" + reason, Diagnostics: []Diagnostic{{Severity: "warning", Message: reason}}}, nil
	}
	callCtx = protocol.WithPermissionGrant(callCtx, grant)
	toolResult, err := registry.ExecuteTool(callCtx, "run_command", argsJSON, maxTaint(taint, contextTaint(ctx)))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "project_diagnostics: execute compiler", err)
	}
	if toolResult == nil {
		return nil, apperr.New(apperr.CodeInternal, "project_diagnostics: empty command result")
	}
	if !toolResult.Success {
		return nil, apperr.New(apperr.CodeInternal, "project_diagnostics: run_command failed: "+toolResult.Error)
	}
	if len(toolResult.Output) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "project_diagnostics: run_command returned empty output")
	}
	return resultFromCommand(root, lang, truncated, toolResult.Output)
}

func resultFromCommand(root, lang string, truncated bool, commandOutput []byte) (*result, error) {
	var raw map[string]any
	if err := json.Unmarshal(commandOutput, &raw); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "project_diagnostics: parse command result", err)
	}
	fullOutput, ok := raw["output"].(string)
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, "project_diagnostics: command result missing string output")
	}
	output := boundedString(fullOutput, maxOutputBytes)
	code, hasCode := intValue(raw["exit_code"])
	if !hasCode {
		return nil, apperr.New(apperr.CodeInternal, "project_diagnostics: command result missing exit_code")
	}
	status := "ok"
	if code != 0 {
		status = "diagnostics"
	}
	if (code == -1 || code == 126 || code == 127) && missingExecutable(output) {
		status = "unavailable"
	}
	if truncated || len(fullOutput) > maxOutputBytes {
		status = "truncated"
	}
	message := ""
	switch status {
	case "unavailable":
		message = "required compiler is unavailable"
	case "truncated":
		message = "diagnostic input or output exceeded configured limits; result is incomplete"
	}
	return &result{Status: status, Language: lang, Diagnostics: parseDiagnostics(root, lang, output), Raw: output, ExitCode: code, Error: message}, nil
}

func validateRoot(ctx context.Context, path string, allowed []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", apperr.New(apperr.CodeInvalidInput, "project_diagnostics: path must be absolute")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "project_diagnostics: project root unavailable", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", apperr.New(apperr.CodeInvalidInput, "project_diagnostics: path must be a project directory")
	}
	checkCtx := ctx
	if err := guard.CheckReadablePath(checkCtx, root, guard.ScopedPaths(checkCtx, allowed)); err != nil {
		return "", apperr.Wrap(apperr.CodeForbidden, "project_diagnostics: project root outside allowed paths", err)
	}
	return root, nil
}

func selectLanguage(ctx context.Context, root, requested string, allowed []string) (string, bool, error) {
	if requested == "" {
		requested = "auto"
	}
	if requested != "auto" && requested != "go" && requested != "rust" && requested != "typescript" && requested != "python" {
		return "", false, apperr.New(apperr.CodeInvalidInput, "project_diagnostics: unsupported language")
	}
	if requested != "auto" {
		return requested, false, nil
	}
	langs := detectLanguages(ctx, root, allowed)
	if len(langs) == 0 {
		return "", false, apperr.New(apperr.CodeInvalidInput, "project_diagnostics: no supported project language detected")
	}
	if len(langs) > 1 {
		return "auto", true, nil
	}
	return langs[0], false, nil
}

func detectLanguages(ctx context.Context, root string, allowed []string) []string {
	found := map[string]bool{}
	found["go"] = rootFileExists(root, "go.mod")
	found["rust"] = rootFileExists(root, "Cargo.toml")
	found["typescript"] = hasTypeScriptManifest(root)
	if files, _, err := pythonFiles(root, ctx, allowed); err == nil && len(files) > 0 {
		found["python"] = true
	}
	langs := make([]string, 0, len(found))
	for lang := range found {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

func buildCommand(root, lang string, ctx context.Context, allowed []string) (string, bool, bool, error) {
	switch lang {
	case "go":
		return buildGoCommand(root)
	case "rust":
		return buildRustCommand(root)
	case "typescript":
		return buildTypeScriptCommand(root)
	case "python":
		return buildPythonCommand(root, ctx, allowed)
	default:
		return "", false, false, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("project_diagnostics: unsupported language %q", lang))
	}
}

func buildGoCommand(root string) (string, bool, bool, error) {
	if !rootFileExists(root, "go.mod") {
		return "", false, false, nil
	}
	return "go vet -json ./...", true, false, nil
}

func buildRustCommand(root string) (string, bool, bool, error) {
	if !rootFileExists(root, "Cargo.toml") {
		return "", false, false, nil
	}
	return "cargo check --offline --message-format=json", true, false, nil
}

func buildTypeScriptCommand(root string) (string, bool, bool, error) {
	tsc := filepath.Join(root, "node_modules", ".bin", "tsc")
	if st, err := os.Stat(tsc); err == nil && !st.IsDir() {
		resolved, resolveErr := filepath.EvalSymlinks(tsc)
		if resolveErr == nil {
			if _, ok := containedPath(root, resolved); ok {
				return shellQuote(resolved) + " --noEmit", true, false, nil
			}
		}
	}
	if !rootFileExists(root, "tsconfig.json") {
		return "", false, false, nil
	}
	path, found := findExecutable("tsc")
	if !found {
		return "", false, false, nil
	}
	return shellQuote(path) + " --noEmit", true, false, nil
}

func findExecutable(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

func buildPythonCommand(root string, ctx context.Context, allowed []string) (string, bool, bool, error) {
	files, truncated, err := pythonFiles(root, ctx, allowed)
	if err != nil || len(files) == 0 {
		return "", false, truncated, err
	}
	parts := make([]string, len(files))
	for i, path := range files {
		parts[i] = shellQuote(path)
	}
	program := "import ast, pathlib, sys\nbad = 0\nfor p in sys.argv[1:]:\n try:\n  ast.parse(pathlib.Path(p).read_text(encoding='utf-8'), filename=p)\n except SyntaxError as e:\n  print('%s:%s:%s: error: %s' % (p, e.lineno or 0, e.offset or 0, e.msg))\n  bad = 1\n except (UnicodeError, OSError) as e:\n  print('%s:0:0: error: %s' % (p, str(e)))\n  bad = 1\nsys.exit(bad)"
	return "python3 -c " + shellQuote(program) + " " + strings.Join(parts, " "), true, truncated, nil
}

func rootFileExists(root, name string) bool {
	st, err := os.Lstat(filepath.Join(root, name))
	return err == nil && st.Mode()&os.ModeSymlink == 0 && !st.IsDir()
}

func hasTypeScriptManifest(root string) bool {
	if rootFileExists(root, "tsconfig.json") {
		return true
	}
	path := filepath.Join(root, "package.json")
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink == 0 && !st.IsDir() && st.Size() <= 1<<20 && fileContains(path, `"typescript"`)
}

func pythonFiles(root string, ctx context.Context, allowed []string) ([]string, bool, error) {
	collector := pythonCollector{root: root, ctx: ctx, allowed: allowed}
	err := filepath.WalkDir(root, collector.visit)
	if err != nil {
		return nil, collector.truncated, apperr.Wrap(apperr.CodeInternal, "project_diagnostics: enumerate Python files", err)
	}
	sort.Strings(collector.files)
	return collector.files, collector.truncated, nil
}

type pythonCollector struct {
	root      string
	ctx       context.Context
	allowed   []string
	files     []string
	total     int64
	entries   int
	truncated bool
}

func (c *pythonCollector) visit(path string, entry os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	c.entries++
	if c.entries > maxWalkEntries {
		c.truncated = true
		return filepath.SkipAll
	}
	if entry.IsDir() {
		return skipPythonDirectory(c.root, path, entry)
	}
	return c.addPythonFile(path, entry)
}

func skipPythonDirectory(root, path string, entry os.DirEntry) error {
	if path != root && (entry.Type()&os.ModeSymlink != 0 || entry.Name() == "node_modules" || entry.Name() == ".venv" || entry.Name() == "venv" || entry.Name() == ".git" || entry.Name() == ".polaris" || entry.Name() == "__pycache__") {
		return filepath.SkipDir
	}
	return nil
}

func (c *pythonCollector) addPythonFile(path string, entry os.DirEntry) error {
	if entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".py") {
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "project_diagnostics: inspect Python source", err)
	}
	if len(c.files) >= maxFiles || c.total+info.Size()+int64(len(path)) > maxSourceBytes {
		c.truncated = true
		return filepath.SkipAll
	}
	if err := guard.CheckReadablePath(c.ctx, path, guard.ScopedPaths(c.ctx, c.allowed)); err != nil {
		c.truncated = true
		return filepath.SkipDir
	}
	c.files = append(c.files, path)
	c.total += info.Size()
	return nil
}

func parseDiagnostics(root, lang, raw string) []Diagnostic {
	if lang == "rust" {
		return parseRustDiagnostics(root, raw)
	}
	if lang == "go" {
		if diagnostics := parseGoDiagnostics(root, raw); diagnostics != nil {
			return diagnostics
		}
	}
	pattern := regexp.MustCompile(`^(.+?):([0-9]+):([0-9]+):\s*(error|warning|info)?:?\s*(.*)$`)
	if lang == "typescript" {
		pattern = regexp.MustCompile(`^(.+?)\(([0-9]+),([0-9]+)\):\s*(error|warning)\s*(.*)$`)
	}
	return parseTextDiagnostics(root, raw, pattern)
}

func parseGoDiagnostics(root, raw string) []Diagnostic {
	type diagnostic struct {
		Pos string `json:"pos"`
		Msg string `json:"message"`
	}
	var packages map[string]map[string][]diagnostic
	if json.Unmarshal([]byte(raw), &packages) != nil {
		return nil
	}
	out := make([]Diagnostic, 0)
	for _, analyzers := range packages {
		for _, records := range analyzers {
			for _, record := range records {
				path, lineNo, column, ok := parsePosition(root, record.Pos)
				if ok {
					out = append(out, Diagnostic{Path: path, Line: lineNo, Column: column, Severity: "error", Message: record.Msg})
				}
			}
		}
	}
	return out
}

func parseTextDiagnostics(root, raw string, pattern *regexp.Regexp) []Diagnostic {
	out := make([]Diagnostic, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "{}" || strings.HasPrefix(line, "# ") {
			continue
		}
		if diagnostic, ok := parseTextLine(root, line, pattern); ok {
			out = append(out, diagnostic)
		}
		if len(out) >= 128 {
			break
		}
	}
	return out
}

func parseTextLine(root, line string, pattern *regexp.Regexp) (Diagnostic, bool) {
	match := pattern.FindStringSubmatch(line)
	if len(match) != 6 {
		return Diagnostic{Severity: "error", Message: line}, true
	}
	pathValue := strings.TrimPrefix(match[1], "vet: ")
	path, ok := containedPath(root, pathValue)
	if !ok {
		return Diagnostic{}, false
	}
	lineNo, _ := strconv.Atoi(match[2])
	column, _ := strconv.Atoi(match[3])
	severity := match[4]
	if severity == "" {
		severity = "error"
	}
	return Diagnostic{Path: path, Line: lineNo, Column: column, Severity: severity, Message: match[5]}, true
}

func parseRustDiagnostics(root, raw string) []Diagnostic {
	type span struct {
		FileName    string `json:"file_name"`
		LineStart   int    `json:"line_start"`
		ColumnStart int    `json:"column_start"`
		IsPrimary   bool   `json:"is_primary"`
	}
	type message struct {
		Level   string `json:"level"`
		Message string `json:"message"`
		Spans   []span `json:"spans"`
	}
	type item struct {
		Reason  string  `json:"reason"`
		Message message `json:"message"`
	}
	out := make([]Diagnostic, 0)
	for _, line := range strings.Split(raw, "\n") {
		var record item
		if json.Unmarshal([]byte(line), &record) != nil || record.Reason != "compiler-message" {
			continue
		}
		for _, itemSpan := range record.Message.Spans {
			if !itemSpan.IsPrimary {
				continue
			}
			path, ok := containedPath(root, itemSpan.FileName)
			if !ok {
				continue
			}
			out = append(out, Diagnostic{Path: path, Line: itemSpan.LineStart, Column: itemSpan.ColumnStart, Severity: record.Message.Level, Message: record.Message.Message})
			break
		}
		if len(out) >= 128 {
			break
		}
	}
	return out
}

func parsePosition(root, value string) (string, int, int, bool) {
	match := regexp.MustCompile(`^(.*?):([0-9]+):([0-9]+)$`).FindStringSubmatch(value)
	if len(match) != 4 {
		return "", 0, 0, false
	}
	path, ok := containedPath(root, match[1])
	if !ok {
		return "", 0, 0, false
	}
	lineNo, _ := strconv.Atoi(match[2])
	column, _ := strconv.Atoi(match[3])
	return path, lineNo, column, true
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func fileContains(path, needle string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), needle)
}
func boundedString(value any, limit int) string {
	s, _ := value.(string)
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
func intValue(value any) (int, bool) {
	n, ok := value.(float64)
	maxInt := float64(int(^uint(0) >> 1))
	minInt := -maxInt - 1
	if !ok || math.Trunc(n) != n || n < minInt || n >= maxInt {
		return 0, false
	}
	return int(n), true
}
func contextTaint(ctx context.Context) types.TaintLevel {
	t, _ := ctx.Value(protocol.CtxTaintLevelKey{}).(types.TaintLevel)
	return t
}
func maxTaint(a, b types.TaintLevel) types.TaintLevel {
	if a > b {
		return a
	}
	return b
}

func missingExecutable(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "no such file") || strings.Contains(lower, "executable file not found")
}

func containedPath(root, value string) (string, bool) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	clean := filepath.Clean(path)
	if !withinRoot(root, clean) {
		return "", false
	}
	resolved, err := resolveDiagnosticPath(clean, root)
	if err != nil || !withinRoot(root, resolved) {
		return "", false
	}
	return resolved, true
}

func resolveDiagnosticPath(path, root string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			relative, relErr := filepath.Rel(current, path)
			if relErr != nil {
				return "", apperr.Wrap(apperr.CodeInvalidInput, "project_diagnostics: resolve diagnostic path", relErr)
			}
			return filepath.Join(resolved, relative), nil
		}
		if current == root || current == filepath.Dir(current) {
			return "", apperr.Wrap(apperr.CodeInvalidInput, "project_diagnostics: diagnostic path has no resolvable parent", err)
		}
	}
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func authorize(ctx context.Context, registry protocol.ToolRegistry, name string, args []byte, taint types.TaintLevel) (protocol.PermissionGrant, string, error) {
	tool, err := registry.Lookup(name)
	if err != nil {
		return protocol.GrantNone, "run_command is unavailable", apperr.Wrap(apperr.CodeOf(err), "project_diagnostics: lookup run_command", err)
	}
	authorizer := protocol.ToolCallAuthorizerFromContext(ctx)
	if authorizer == nil {
		return protocol.GrantNone, "no tool-call authorizer is available; approve run_command separately", apperr.New(apperr.CodeForbidden, "project_diagnostics: authorizer unavailable")
	}
	grant, reason, ok := authorizer.AuthorizeCall(protocol.WithPermissionGrant(ctx, protocol.GrantNone), tool, args, taint)
	if !ok || grant == protocol.GrantNone {
		if reason == "" {
			reason = "run_command requires separate approval"
		}
		return protocol.GrantNone, reason, apperr.New(apperr.CodeForbidden, "project_diagnostics: run_command authorization denied")
	}
	return grant, "", nil
}
