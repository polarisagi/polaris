package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── git_diff ─────────────────────────────────────────────────────────────────

type gitDiffArgs struct {
	Path   string `json:"path"`
	Ref1   string `json:"ref1"`
	Ref2   string `json:"ref2"`
	File   string `json:"file"`
	Staged bool   `json:"staged"`
}

type gitDiffStat struct {
	Path    string `json:"path"`
	Added   int    `json:"lines_added"`
	Removed int    `json:"lines_removed"`
}

// MakeGitDiffFn 返回 git_diff 工具实现。
// 使用 bash.RunSandboxedArgv 以 argv 形式传参（非 bash -c 拼接字符串），
// 无 shell 注入风险；path 受 allowedPaths 白名单约束。
// 输出：结构化文件变更列表 + 统计 + 原始 unified diff（上限 1MB）。
func MakeGitDiffFn(allowedPaths []string, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args gitDiffArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "git_diff: invalid args", err)
		}
		if err := validateGitDiffArgs(args); err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "git_diff: validate args", err)
		}
		if err := guard.CheckReadablePath(ctx, args.Path, paths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "MakeGitDiffFn", err)
		}
		return executeGitDiff(ctx, args, sandboxEnabled, bwrapPath)
	}
}

func validateGitDiffArgs(args gitDiffArgs) error {
	if args.Path == "" {
		return apperr.New(apperr.CodeInvalidInput, "git_diff: path is required")
	}
	if args.Ref1 == "" && args.Ref2 != "" {
		return apperr.New(apperr.CodeInvalidInput, "git_diff: ref2 requires ref1")
	}
	if strings.HasPrefix(args.Ref1, "-") || strings.HasPrefix(args.Ref2, "-") {
		return apperr.New(apperr.CodeInvalidInput, "git_diff: refs must not start with '-'")
	}
	if args.File != "" && !validGitDiffFilePath(args.File) {
		return apperr.New(apperr.CodeInvalidInput, "git_diff: file must be a relative path without '..' components")
	}
	return nil
}

func executeGitDiff(parent context.Context, args gitDiffArgs, sandboxEnabled bool, bwrapPath string) ([]byte, error) {
	workDir := filepath.Clean(args.Path)
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	rawOut, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", buildGitDiffArgs(args, false), workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("git_diff: failed: %s", strings.TrimSpace(string(rawOut))), err)
	}
	raw := string(rawOut)
	truncated := len(raw) > 1<<20
	if truncated {
		raw = raw[:1<<20]
	}
	numstatOut, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", buildGitDiffArgs(args, true), workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("git_diff: numstat failed: %s", strings.TrimSpace(string(numstatOut))), err)
	}
	files, totalAdded, totalRemoved, err := parseGitNumstat(numstatOut)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "git_diff: parse numstat", err)
	}
	out, err := json.Marshal(map[string]any{"files": files, "lines_added": totalAdded, "lines_removed": totalRemoved, "raw": raw, "truncated": truncated})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "git_diff: encode result", err)
	}
	return out, nil
}

func parseGitNumstat(data []byte) ([]gitDiffStat, int, int, error) {
	var files []gitDiffStat
	totalAdded, totalRemoved := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return nil, 0, 0, apperr.New(apperr.CodeInternal, "git_diff: malformed numstat record")
		}
		file := gitDiffStat{Path: parts[2]}
		if parts[0] != "-" {
			if _, err := fmt.Sscanf(parts[0], "%d", &file.Added); err != nil {
				return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "git_diff: invalid numstat added count", err)
			}
		}
		if parts[1] != "-" {
			if _, err := fmt.Sscanf(parts[1], "%d", &file.Removed); err != nil {
				return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "git_diff: invalid numstat removed count", err)
			}
		}
		totalAdded += file.Added
		totalRemoved += file.Removed
		files = append(files, file)
	}
	return files, totalAdded, totalRemoved, nil
}

func buildGitDiffArgs(args gitDiffArgs, numstat bool) []string {
	result := []string{"-c", "diff.external=", "-c", "core.fsmonitor=false", "--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv"}
	if args.Staged {
		result = append(result, "--cached")
	}
	if args.Ref1 != "" {
		result = append(result, args.Ref1)
		if args.Ref2 != "" {
			result = append(result, args.Ref2)
		}
	}
	if numstat {
		result = append(result, "--numstat")
	}
	if args.File != "" {
		result = append(result, "--", args.File)
	}
	return result
}

func validGitDiffFilePath(path string) bool {
	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return false
	}
	for _, component := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == ".." {
			return false
		}
	}
	return true
}

// ─── git_commit ───────────────────────────────────────────────────────────────

// MakeGitCommitFn 返回 git_commit 工具实现。
// 依次执行 git add + git commit，返回 commit hash 与 branch 名。
// 调用方对提交内容负责；此工具仅执行机械操作，不做内容审查。
func MakeGitCommitFn(allowedPaths []string, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args struct {
			Path       string   `json:"path"`        // git 仓库根目录
			Message    string   `json:"message"`     // commit 消息
			Files      []string `json:"files"`       // 要 stage 的文件（空 = git add -A）
			AllowEmpty bool     `json:"allow_empty"` // 是否允许空提交
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "git_commit: invalid args", err)
		}
		if args.Path == "" {
			return nil, apperr.New(apperr.CodeInternal, "git_commit: path is required")
		}
		if args.Message == "" {
			return nil, apperr.New(apperr.CodeInternal, "git_commit: message is required")
		}
		if err := guard.CheckWritablePath(ctx, args.Path, paths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "MakeGitCommitFn", err)
		}
		workDir := filepath.Clean(args.Path)

		// 拒绝文件路径中的路径穿越
		for _, f := range args.Files {
			if strings.Contains(f, "..") {
				return nil, apperr.New(apperr.CodeInternal,
					fmt.Sprintf("git_commit: file path %q must not contain '..'", f))
			}
		}

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// git add
		var addArgs []string
		if len(args.Files) > 0 {
			addArgs = append([]string{"add", "--"}, args.Files...)
		} else {
			addArgs = []string{"add", "-A"}
		}
		if out, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", addArgs, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath); err != nil {
			return nil, apperr.New(apperr.CodeInternal,
				fmt.Sprintf("git_commit: git add failed: %s", string(out)))
		}

		// git commit
		commitArgs := []string{"commit", "-m", args.Message}
		if args.AllowEmpty {
			commitArgs = append(commitArgs, "--allow-empty")
		}
		if out, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", commitArgs, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath); err != nil {
			return nil, apperr.New(apperr.CodeInternal,
				fmt.Sprintf("git_commit: git commit failed: %s", string(out)))
		}

		// rev-parse HEAD → commit hash
		hashOut, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", []string{"rev-parse", "HEAD"}, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "git_commit: rev-parse failed", err)
		}

		// rev-parse --abbrev-ref HEAD → branch name
		branchOut, _ := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", []string{"rev-parse", "--abbrev-ref", "HEAD"}, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)

		return json.Marshal(map[string]any{
			"hash":    strings.TrimSpace(string(hashOut)),
			"branch":  strings.TrimSpace(string(branchOut)),
			"message": args.Message,
		})
	}
}

// ─── template_render ──────────────────────────────────────────────────────────

const (
	templateMaxInputBytes  = 64 * 1024 // 模板字符串输入上限 64KB
	templateMaxOutputBytes = 50 * 1024 // 渲染输出上限 50KB
)

// TemplateRenderFn 用 Go text/template 渲染模板字符串，纯内存计算无外部依赖。
// 使用 text/template 而非 html/template，避免 HTML 转义破坏非 HTML 场景的输出。
func TemplateRenderFn(_ context.Context, input []byte) ([]byte, error) {
	var args struct {
		Template string         `json:"template"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "template_render: invalid args", err)
	}
	if args.Template == "" {
		return nil, apperr.New(apperr.CodeInternal, "template_render: template is required")
	}
	if len(args.Template) > templateMaxInputBytes {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("template_render: template exceeds %dKB limit", templateMaxInputBytes/1024))
	}

	tmpl, err := template.New("t").Parse(args.Template)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("template_render: parse error: %v", err))
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, args.Data); err != nil {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("template_render: execute error: %v", err))
	}

	result := buf.String()
	truncated := false
	if len(result) > templateMaxOutputBytes {
		result = result[:templateMaxOutputBytes] + "\n... (truncated at 50KB)"
		truncated = true
	}

	out, err := json.Marshal(map[string]any{
		"output":    result,
		"truncated": truncated,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "git_text_tools: 序列化结果失败", err)
	}
	return out, nil
}
