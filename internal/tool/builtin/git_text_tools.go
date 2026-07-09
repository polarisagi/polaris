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

// MakeGitDiffFn 返回 git_diff 工具实现。
// 用 exec.Command 而非 bash，无 shell 注入风险；path 受 allowedPaths 白名单约束。
// 输出：结构化文件变更列表 + 统计 + 原始 unified diff（上限 1MB）。
func MakeGitDiffFn(allowedPaths []string, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path   string `json:"path"`   // git 仓库根目录，必须在 allowedPaths 内
			Ref1   string `json:"ref1"`   // 起始 ref（可选，如 "HEAD~1"、branch 名、commit hash）
			Ref2   string `json:"ref2"`   // 结束 ref（可选；配合 ref1 使用）
			File   string `json:"file"`   // 限定单文件（可选，相对于 path 的路径）
			Staged bool   `json:"staged"` // 是否展示已 stage 的变更（--cached）
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "git_diff: invalid args", err)
		}
		if args.Path == "" {
			return nil, apperr.New(apperr.CodeInternal, "git_diff: path is required")
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "MakeGitDiffFn", err)
		}
		workDir := filepath.Clean(args.Path)

		// 构造 diff 参数（全部独立传参，无 shell 拼接）
		diffArgs := []string{"diff"}
		if args.Staged {
			diffArgs = append(diffArgs, "--cached")
		}
		if args.Ref1 != "" {
			if args.Ref2 != "" {
				diffArgs = append(diffArgs, args.Ref1, args.Ref2)
			} else {
				diffArgs = append(diffArgs, args.Ref1)
			}
		}
		if args.File != "" {
			// 防路径穿越：拒绝 ".." 分量
			if strings.Contains(args.File, "..") {
				return nil, apperr.New(apperr.CodeInternal, "git_diff: file path must not contain '..'")
			}
			diffArgs = append(diffArgs, "--", args.File)
		}

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// 1. 原始 unified diff（上限 1MB）
		rawOut, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", diffArgs, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)
		if err != nil {
			// bash.RunSandboxedArgv 底层沙箱把 stdout/stderr 合并返回，rawOut 里就是 git 的
			// 具体报错文本（如 "not a git repository"），带出来而不是只报一个空洞的
			// "exit status N"，否则调用方排障时看不出失败原因。
			return nil, apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("git_diff: failed: %s", strings.TrimSpace(string(rawOut))), err)
		}
		raw := string(rawOut)
		truncated := false
		if len(raw) > 1<<20 {
			raw = raw[:1<<20]
			truncated = true
		}

		// 2. 结构化统计（--numstat 输出：<added>\t<removed>\t<file>）
		numstatArgs := append([]string{"diff", "--numstat"}, diffArgs[1:]...)
		numstatOut, _ := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", numstatArgs, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)

		type fileStat struct {
			Path    string `json:"path"`
			Added   int    `json:"lines_added"`
			Removed int    `json:"lines_removed"`
		}
		var files []fileStat
		totalAdded, totalRemoved := 0, 0
		for _, line := range strings.Split(strings.TrimSpace(string(numstatOut)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			var f fileStat
			f.Path = parts[2]
			fmt.Sscanf(parts[0], "%d", &f.Added)   //nolint:errcheck
			fmt.Sscanf(parts[1], "%d", &f.Removed) //nolint:errcheck
			totalAdded += f.Added
			totalRemoved += f.Removed
			files = append(files, f)
		}

		return json.Marshal(map[string]any{
			"files":         files,
			"lines_added":   totalAdded,
			"lines_removed": totalRemoved,
			"raw":           raw,
			"truncated":     truncated,
		})
	}
}

// ─── git_commit ───────────────────────────────────────────────────────────────

// MakeGitCommitFn 返回 git_commit 工具实现。
// 依次执行 git add + git commit，返回 commit hash 与 branch 名。
// 调用方对提交内容负责；此工具仅执行机械操作，不做内容审查。
func MakeGitCommitFn(allowedPaths []string, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
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
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
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

// templateRenderFn 用 Go text/template 渲染模板字符串，纯内存计算无外部依赖。
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

	return json.Marshal(map[string]any{
		"output":    result,
		"truncated": truncated,
	})
}
