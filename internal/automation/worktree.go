package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// commitAuthor 自动化流水线生成的 git 提交统一署名。
// 依据 CLAUDE.md「[强制] Git 署名」：所有 Git 提交必须统一使用此署名，
// 防止代理 AI 工具意外污染 GitHub 贡献者列表。
const commitAuthor = "MrLaoLiAI <polarisagi.online@gmail.com>"

// WorktreeManager manages git worktrees for automation tasks.
type WorktreeManager struct {
	baseDir string // 基础仓库目录
	tmpDir  string // 存放 worktree 的临时目录
}

func NewWorktreeManager(baseDir, tmpDir string) *WorktreeManager {
	return &WorktreeManager{
		baseDir: baseDir,
		tmpDir:  tmpDir,
	}
}

// PrepareWorktree 创建一个独立的 git worktree. 返回新目录路径和 branch 名字。
func (w *WorktreeManager) PrepareWorktree(ctx context.Context, runID string) (string, string, error) {
	branchName := fmt.Sprintf("polaris-auto-%s", runID)
	wtDir := filepath.Join(w.tmpDir, branchName)

	if err := os.MkdirAll(w.tmpDir, 0755); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "WorktreeManager.PrepareWorktree mkdir", err)
	}

	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branchName, wtDir)
	cmd.Dir = w.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git worktree add failed: %s", string(out)), err)
	}

	return wtDir, branchName, nil
}

// CommitChanges 检查 worktree 是否有改动；若有，add + commit（不 push）。
// 返回 (是否有改动, diff 摘要, error)。diff 摘要用于 HITL 审批展示，调用方应在
// 拿到 hasChanges=true 后先经过风险审批，再调用 PushBranch —— 本方法本身不做任何
// 网络操作，仅落地本地提交，因此可安全地无条件调用。
//
// 2026-07-04 审计修复（Task 19）：
//   - 提交此前未带 --author，违反 CLAUDE.md「[强制] Git 署名」；现固定使用 commitAuthor。
//   - 原 FinalizeWorktree 在 commit 后无条件 push，无任何风险评估/人工审批即可将 LLM
//     生成的代码改动推送到 origin；已拆分为 CommitChanges/PushBranch 两步，push 决策
//     上收给调用方（cron.go），由其接入 HITLGateway 后再决定是否 PushBranch。
func (w *WorktreeManager) CommitChanges(ctx context.Context, wtDir, branchName string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = wtDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "", apperr.Wrap(apperr.CodeInternal, "git status failed", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return false, "", nil // 没有改动
	}

	cmd = exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git add failed: %s", string(out)), err)
	}

	// diff --stat 摘要（供 HITL 审批展示）：必须在 add 之后、commit 之前，用 --cached
	// 才能覆盖新增的未跟踪文件（git diff 默认不显示 untracked 文件）。
	statCmd := exec.CommandContext(ctx, "git", "diff", "--stat", "--cached", "HEAD")
	statCmd.Dir = wtDir
	statOut, _ := statCmd.CombinedOutput() // 摘要生成失败不阻断提交，仅审批展示信息缺失

	cmd = exec.CommandContext(ctx, "git", "commit",
		"--author", commitAuthor,
		"-m", fmt.Sprintf("automation: run %s", branchName))
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git commit failed: %s", string(out)), err)
	}

	return true, string(statOut), nil
}

// PushBranch 推送分支到 origin。
// 安全边界：调用方必须在此之前完成风险审批（见 CommitChanges 文档），本方法不做任何审批检查。
func (w *WorktreeManager) PushBranch(ctx context.Context, wtDir, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "-u", "origin", branchName)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git push failed: %s", string(out)), err)
	}
	return nil
}

// Cleanup 清理 worktree 目录与 git 元数据。无论提交/推送是否成功都应调用（幂等）。
func (w *WorktreeManager) Cleanup(wtDir string) {
	_ = os.RemoveAll(wtDir)
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "prune")
	cmd.Dir = w.baseDir
	_ = cmd.Run()
	// branch 保留在本地/远端（若已 push），供人工审查或恢复；不做自动 branch 删除。
}

var githubRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/.]+?)(\.git)?$`)

// CreatePullRequest 通过 GitHub REST API 为已推送的分支创建 PR。
//
// 2026-07-04 审计修复（Task 19）：此前 cron.go 中 PR 创建为纯日志占位符，push 后的分支
// 无法进入常规代码评审流程。GITHUB_TOKEN 环境变量未配置时视为「未启用 PR 自动创建」，
// 静默跳过而非报错 —— PR 创建是评审流程的便利性增强而非安全边界（push 前的 HITL 审批
// 才是安全边界，见 cron.go executeAutomation），故此路径 fail-open 是合理的。
func (w *WorktreeManager) CreatePullRequest(ctx context.Context, branchName, title, body string) error {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil
	}

	owner, repoName, err := w.parseGitHubRemote(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CreatePullRequest: parse github remote failed", err)
	}

	base, err := w.defaultBranch(ctx)
	if err != nil {
		base = "main" // 探测失败时回退到通用默认分支名，不阻断 PR 创建
	}

	payload, err := json.Marshal(map[string]string{
		"title": title,
		"head":  branchName,
		"base":  base,
		"body":  body,
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CreatePullRequest: marshal payload failed", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CreatePullRequest: build request failed", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CreatePullRequest: request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("CreatePullRequest: github api returned %d: %s", resp.StatusCode, string(respBody)))
	}
	return nil
}

// parseGitHubRemote 解析 origin remote URL，提取 GitHub owner/repo。
// 支持 https://github.com/owner/repo.git 与 git@github.com:owner/repo.git 两种形式。
func (w *WorktreeManager) parseGitHubRemote(ctx context.Context) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = w.baseDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "git remote get-url failed", err)
	}
	m := githubRemoteRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if len(m) < 3 {
		return "", "", apperr.New(apperr.CodeInternal, "origin remote is not a recognizable github.com URL: "+string(out))
	}
	return m[1], m[2], nil
}

// defaultBranch 探测远端默认分支（PR 的 base）。
func (w *WorktreeManager) defaultBranch(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = w.baseDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "git symbolic-ref failed", err)
	}
	ref := strings.TrimSpace(string(out)) // refs/remotes/origin/main
	parts := strings.Split(ref, "/")
	if len(parts) == 0 {
		return "", apperr.New(apperr.CodeInternal, "unexpected symbolic-ref output: "+ref)
	}
	return parts[len(parts)-1], nil
}
