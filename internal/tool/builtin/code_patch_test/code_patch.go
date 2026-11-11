package codepatchtest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	maxPatchBytes   = 256 * 1024
	maxTestPatch    = 256 * 1024
	maxTestCommands = 8
	maxOutputBytes  = 8 * 1024
)

const TotalTimeout = 10 * time.Minute

type Deps struct {
	CmdRunner    sandbox.CmdRunner
	ScratchRoot  string
	AllowedPaths []string
}

type request struct {
	Path               string   `json:"path"`
	BaseCommit         string   `json:"base_commit"`
	CandidatePatch     string   `json:"candidate_patch"`
	TestPatch          string   `json:"test_patch"`
	FailToPassCommands []string `json:"fail_to_pass_commands"`
	PassToPassCommands []string `json:"pass_to_pass_commands"`
}

type commandResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

type result struct {
	Passed       bool            `json:"passed"`
	Phase        string          `json:"phase"`
	BaselineF2P  []commandResult `json:"baseline_fail_to_pass,omitempty"`
	BaselinePTP  []commandResult `json:"baseline_pass_to_pass,omitempty"`
	CandidateF2P []commandResult `json:"candidate_fail_to_pass,omitempty"`
	CandidatePTP []commandResult `json:"candidate_pass_to_pass,omitempty"`
}

func MakeCodePatchTestFn(deps Deps) func(context.Context, []byte) ([]byte, error) {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if len(input) > 600*1024 {
			return nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: input exceeds 600 KiB")
		}
		if deps.CmdRunner == nil || isNopRunner(deps.CmdRunner) {
			return nil, apperr.New(apperr.CodeServiceUnavailable, "code_patch_test: physical sandbox runner required")
		}
		var req request
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: invalid input", err)
		}
		if err := validateRequest(req, deps); err != nil {
			return nil, err
		}
		return execute(ctx, deps, req)
	}
}

func validateRequest(req request, deps Deps) error {
	if !filepath.IsAbs(req.Path) || !regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(req.BaseCommit) {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: absolute local repo and full base_commit required")
	}
	if !filepath.IsAbs(deps.ScratchRoot) || strings.TrimSpace(req.CandidatePatch) == "" || len(req.CandidatePatch) > maxPatchBytes || len(req.TestPatch) > maxTestPatch {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: scratch root and bounded non-empty candidate patch required")
	}
	if len(deps.AllowedPaths) == 0 {
		return apperr.New(apperr.CodeServiceUnavailable, "code_patch_test: allowed project roots are required")
	}
	if err := validateCommands(req.FailToPassCommands); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: invalid fail_to_pass commands", err)
	}
	if err := validateCommands(req.PassToPassCommands); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: invalid pass_to_pass commands", err)
	}
	return nil
}

func validateCommands(commands []string) error {
	if len(commands) == 0 || len(commands) > maxTestCommands {
		return apperr.New(apperr.CodeInvalidInput, "command count must be between 1 and 8")
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" || len(command) > 4096 {
			return apperr.New(apperr.CodeInvalidInput, "command is empty or too long")
		}
	}
	return nil
}

func execute(ctx context.Context, deps Deps, req request) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()
	workspace, err := prepareWorkspace(execCtx, ctx, deps, req)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workspace.scratch)
	return evaluateWorkspace(execCtx, deps.CmdRunner, workspace, req)
}

type patchWorkspace struct {
	repo     string
	scratch  string
	checkout string
}

func prepareWorkspace(execCtx, scopeCtx context.Context, deps Deps, req request) (patchWorkspace, error) {
	repo, allowedPaths, err := validateRepository(scopeCtx, req.Path, deps.AllowedPaths)
	if err != nil {
		return patchWorkspace{}, err
	}
	scratch, err := prepareScratch(scopeCtx, deps.ScratchRoot, repo, allowedPaths)
	if err != nil {
		return patchWorkspace{}, err
	}
	workspace := patchWorkspace{repo: repo, scratch: scratch, checkout: filepath.Join(scratch, "repo")}
	if err := prepareCheckout(execCtx, deps.CmdRunner, workspace, req.BaseCommit); err != nil {
		return patchWorkspace{}, cleanupScratchAfterError(scratch, err)
	}
	return workspace, nil
}

func validateRepository(ctx context.Context, path string, roots []string) (string, []string, error) {
	repo, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: local repository unavailable", err)
	}
	info, err := os.Stat(repo)
	if err != nil || !info.IsDir() {
		return "", nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: repository must be a local directory")
	}
	if err := guard.CheckScopeRoot(repo); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeForbidden, "code_patch_test: unsafe repository scope", err)
	}
	allowedPaths, err := canonicalAllowedPaths(ctx, roots)
	if err != nil {
		return "", nil, err
	}
	if err := guard.CheckReadablePath(ctx, repo, allowedPaths); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeForbidden, "code_patch_test: repository outside allowed paths", err)
	}
	return repo, allowedPaths, nil
}

func prepareScratch(ctx context.Context, scratchRoot, repo string, allowedPaths []string) (string, error) {
	root, err := filepath.Abs(scratchRoot)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: invalid scratch root", err)
	}
	if pathWithin(repo, root) {
		return "", apperr.New(apperr.CodeForbidden, "code_patch_test: scratch root must not be inside the source repository")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "code_patch_test: create scratch root", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || canonicalRoot != filepath.Clean(root) {
		return "", apperr.New(apperr.CodeForbidden, "code_patch_test: scratch root must be canonical and symlink-free")
	}
	if pathWithin(repo, canonicalRoot) {
		return "", apperr.New(apperr.CodeForbidden, "code_patch_test: scratch root must not be inside the source repository")
	}
	if err := guard.CheckScopeRoot(root); err != nil {
		return "", apperr.Wrap(apperr.CodeForbidden, "code_patch_test: unsafe scratch scope", err)
	}
	if err := guard.CheckWritablePath(ctx, root, allowedPaths); err != nil {
		return "", apperr.Wrap(apperr.CodeForbidden, "code_patch_test: scratch root outside allowed paths", err)
	}
	scratch, err := os.MkdirTemp(root, "patch-eval-")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "code_patch_test: create scratch", err)
	}
	if err := prepareCaches(scratch); err != nil {
		return "", cleanupScratchAfterError(scratch, err)
	}
	return scratch, nil
}

func cleanupScratchAfterError(scratch string, cause error) error {
	if err := os.RemoveAll(scratch); err != nil {
		slog.Error("code_patch_test: scratch cleanup failed", "scratch", scratch, "cause", cause, "cleanup_error", err)
		return apperr.Wrap(apperr.CodeOf(cause), "code_patch_test: scratch cleanup failed after operation error", errors.Join(cause, err))
	}
	return cause
}

func prepareCaches(scratch string) error {
	cacheRoot := filepath.Join(scratch, ".cache")
	for _, name := range []string{"go", "tmp", "cargo", "npm"} {
		if err := os.MkdirAll(filepath.Join(cacheRoot, name), 0700); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "code_patch_test: prepare isolated caches", err)
		}
	}
	return nil
}

func prepareCheckout(ctx context.Context, runner sandbox.CmdRunner, workspace patchWorkspace, baseCommit string) error {
	clone := "git clone --no-local --no-hardlinks --quiet " + shellQuote(workspace.repo) + " " + shellQuote(workspace.checkout) + " && git -C " + shellQuote(workspace.checkout) + " checkout --quiet --detach " + baseCommit
	if _, err := run(ctx, runner, clone, workspace.scratch, runConfig{AllowedPaths: []string{workspace.scratch, workspace.repo}, DenyWritePaths: []string{workspace.repo}}); err != nil {
		return err
	}
	if err := executionContextError(ctx); err != nil {
		return err
	}
	checkedOut, err := run(ctx, runner, "git -C "+shellQuote(workspace.checkout)+" rev-parse HEAD", workspace.scratch, runConfig{AllowedPaths: []string{workspace.scratch}, DenyPaths: []string{workspace.repo}, DenyWritePaths: []string{workspace.repo}})
	if err != nil {
		return err
	}
	if err := executionContextError(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(checkedOut.Output) != baseCommit {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: checkout does not match requested base_commit")
	}
	return nil
}

func evaluateWorkspace(ctx context.Context, runner sandbox.CmdRunner, workspace patchWorkspace, req request) ([]byte, error) {
	testPaths, err := applyPatch(ctx, runner, workspace.scratch, workspace.checkout, "trusted-tests.patch", req.TestPatch, workspace.scratch, workspace.repo, nil)
	if err != nil {
		return nil, err
	}
	res := result{Phase: "baseline"}
	res.BaselineF2P = runCommands(ctx, runner, workspace.scratch, workspace.checkout, req.FailToPassCommands, workspace.scratch, workspace.repo)
	res.BaselinePTP = runCommands(ctx, runner, workspace.scratch, workspace.checkout, req.PassToPassCommands, workspace.scratch, workspace.repo)
	if err := executionContextError(ctx); err != nil {
		return nil, err
	}
	if !allCommandsFail(res.BaselineF2P) || !allCommandsPass(res.BaselinePTP) {
		return marshalResult(res)
	}
	if _, err := applyPatch(ctx, runner, workspace.scratch, workspace.checkout, "candidate.patch", req.CandidatePatch, workspace.scratch, workspace.repo, testPaths); err != nil {
		return nil, err
	}
	res.Phase = "candidate"
	res.CandidateF2P = runCommands(ctx, runner, workspace.scratch, workspace.checkout, req.FailToPassCommands, workspace.scratch, workspace.repo)
	res.CandidatePTP = runCommands(ctx, runner, workspace.scratch, workspace.checkout, req.PassToPassCommands, workspace.scratch, workspace.repo)
	if err := executionContextError(ctx); err != nil {
		return nil, err
	}
	res.Passed = allCommandsPass(res.CandidateF2P) && allCommandsPass(res.CandidatePTP)
	return marshalResult(res)
}

func executionContextError(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.Wrap(apperr.CodeTimeout, "code_patch_test: evaluation deadline exceeded", err)
	}
	return apperr.Wrap(apperr.CodeCancelled, "code_patch_test: execution cancelled", err)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func canonicalAllowedPaths(ctx context.Context, roots []string) ([]string, error) {
	paths := guard.ScopedPaths(ctx, roots)
	canonical := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		if _, ok := seen[real]; ok {
			continue
		}
		seen[real] = struct{}{}
		canonical = append(canonical, real)
	}
	if len(canonical) == 0 {
		return nil, apperr.New(apperr.CodeForbidden, "code_patch_test: no existing allowed project roots")
	}
	return canonical, nil
}

func applyPatch(ctx context.Context, runner sandbox.CmdRunner, scratch, checkout, name, patch, root, source string, protected map[string]struct{}) (map[string]struct{}, error) {
	paths, err := patchPaths(patch)
	if err != nil {
		return nil, err
	}
	for path := range paths {
		if _, ok := protected[path]; ok {
			return nil, apperr.New(apperr.CodeForbidden, "code_patch_test: candidate patch changes trusted test_patch target")
		}
	}
	if patch == "" {
		return paths, nil
	}
	file := filepath.Join(scratch, name)
	if err := os.WriteFile(file, []byte(patch), 0600); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_patch_test: write patch file", err)
	}
	_, err = run(ctx, runner, "git -C "+shellQuote(checkout)+" apply --whitespace=nowarn "+shellQuote(file), scratch, runConfig{AllowedPaths: []string{root}, DenyPaths: []string{source}, DenyWritePaths: []string{source}})
	return paths, err
}

func patchPaths(patch string) (map[string]struct{}, error) {
	if len(patch) > maxPatchBytes {
		return nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: patch exceeds parser limit")
	}
	state := patchPathState{paths: make(map[string]struct{})}
	for _, line := range strings.Split(patch, "\n") {
		if err := state.consume(line); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(patch) != "" && len(state.paths) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: patch has no parsed file targets")
	}
	return state.paths, nil
}

type patchPathState struct {
	paths    map[string]struct{}
	currentA string
	currentB string
}

func (state *patchPathState) consume(line string) error {
	if isRenameOrCopy(line) {
		return apperr.New(apperr.CodeForbidden, "code_patch_test: rename/copy patches are unsupported")
	}
	if strings.HasPrefix(line, "diff --git ") {
		return state.parseDiffHeader(line)
	}
	if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
		return state.parseFileHeader(line)
	}
	return nil
}

func isRenameOrCopy(line string) bool {
	for _, prefix := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func (state *patchPathState) parseDiffHeader(line string) error {
	parts := strings.Split(line, " ")
	if len(parts) != 4 || !strings.HasPrefix(parts[2], "a/") || !strings.HasPrefix(parts[3], "b/") {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: unsupported patch header")
	}
	state.currentA, state.currentB = parts[2][2:], parts[3][2:]
	if err := addSafePatchPath(state.paths, state.currentA); err != nil {
		return err
	}
	return addSafePatchPath(state.paths, state.currentB)
}

func (state *patchPathState) parseFileHeader(line string) error {
	field := strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "+++ ")
	field = strings.SplitN(field, "\t", 2)[0]
	if field == "/dev/null" {
		return nil
	}
	prefix, current := "a/", state.currentA
	if strings.HasPrefix(line, "+++ ") {
		prefix, current = "b/", state.currentB
	}
	if !strings.HasPrefix(field, prefix) || state.currentA == "" || state.currentB == "" {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: unsupported file header")
	}
	if strings.TrimPrefix(field, prefix) != current {
		return apperr.New(apperr.CodeInvalidInput, "code_patch_test: inconsistent patch paths")
	}
	return nil
}

func addSafePatchPath(paths map[string]struct{}, name string) error {
	clean := filepath.Clean(name)
	if name == "" || filepath.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(name, "\\") || clean == ".git" || strings.HasPrefix(filepath.ToSlash(clean), ".git/") || strings.Contains(name, " ") {
		return apperr.New(apperr.CodeForbidden, "code_patch_test: patch contains unsafe or unsupported path")
	}
	paths[filepath.ToSlash(clean)] = struct{}{}
	return nil
}

func runCommands(ctx context.Context, runner sandbox.CmdRunner, scratch, checkout string, commands []string, root, source string) []commandResult {
	results := make([]commandResult, 0, len(commands))
	for _, command := range commands {
		if ctx.Err() != nil {
			return results
		}
		commandCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		out, code, method, err := runner.RunCmd(commandCtx, sandbox.CmdRunnerCfg{
			CallerType: "builtin", Command: "cd " + shellQuote(checkout) + " && " + command,
			WorkDir: scratch, AllowedPaths: []string{scratch}, DenyPaths: []string{source}, DenyWritePaths: []string{source, checkout},
			Env:          []string{"GOCACHE=" + filepath.Join(scratch, ".cache", "go"), "GOTMPDIR=" + filepath.Join(scratch, ".cache", "tmp"), "CARGO_TARGET_DIR=" + filepath.Join(scratch, ".cache", "cargo"), "npm_config_cache=" + filepath.Join(scratch, ".cache", "npm")},
			NetworkBlock: true, TimeoutMs: timeoutMs(commandCtx),
		})
		commandTimedOut := commandCtx.Err() != nil
		cancel()
		if err != nil || commandTimedOut {
			code = -1
			if err != nil {
				out = []byte(err.Error())
			} else {
				out = []byte("test command timed out")
			}
		} else if method != "bwrap" && method != "seatbelt" || code == 126 || code == 127 {
			code = -1
			out = []byte("code_patch_test: Rust physical sandbox unavailable")
		}
		results = append(results, commandResult{ExitCode: code, Output: boundedOutput(out)})
	}
	return results
}

type runConfig struct {
	AllowedPaths   []string
	DenyWritePaths []string
	DenyPaths      []string
}

func run(ctx context.Context, runner sandbox.CmdRunner, command, workdir string, cfg runConfig) (commandResult, error) {
	out, code, method, err := runner.RunCmd(ctx, sandbox.CmdRunnerCfg{
		CallerType: "builtin", Command: command, WorkDir: workdir,
		AllowedPaths: cfg.AllowedPaths, DenyWritePaths: cfg.DenyWritePaths, DenyPaths: cfg.DenyPaths, NetworkBlock: true,
		TimeoutMs: timeoutMs(ctx),
	})
	if err != nil {
		return commandResult{}, apperr.Wrap(apperr.CodeServiceUnavailable, "code_patch_test: sandbox execution failed", err)
	}
	if method != "bwrap" && method != "seatbelt" {
		return commandResult{}, apperr.New(apperr.CodeServiceUnavailable, "code_patch_test: Rust physical sandbox unavailable")
	}
	res := commandResult{ExitCode: code, Output: boundedOutput(out)}
	if code != 0 {
		return res, apperr.New(apperr.CodeInvalidInput, "code_patch_test: sandbox command failed: "+res.Output)
	}
	return res, nil
}

func timeoutMs(ctx context.Context) uint64 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return uint64((120 * time.Second).Milliseconds())
	}
	remaining := time.Until(deadline).Milliseconds()
	if remaining < 1 {
		return 1
	}
	return uint64(remaining)
}

func allCommandsPass(results []commandResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, item := range results {
		if item.ExitCode != 0 {
			return false
		}
	}
	return true
}

func allCommandsFail(results []commandResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, item := range results {
		if item.ExitCode == 0 || item.ExitCode < 1 || item.ExitCode == 126 || item.ExitCode == 127 {
			return false
		}
	}
	return true
}

func isNopRunner(runner sandbox.CmdRunner) bool {
	_, ok := runner.(sandbox.NopCmdRunner)
	return ok
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func boundedOutput(output []byte) string {
	if len(output) > maxOutputBytes {
		output = output[:maxOutputBytes]
	}
	return string(output)
}

func marshalResult(res result) ([]byte, error) {
	encoded, err := json.Marshal(res)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_patch_test: encode result", err)
	}
	return encoded, nil
}
