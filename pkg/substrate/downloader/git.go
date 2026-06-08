package downloader

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// GitCloneOrPull 将远端仓库同步到 destDir（clone 或 pull）。
// 在中国大陆网络下自动通过 ghproxy 加速 clone URL；pull 不走代理（依赖已有 remote）。
// 返回值：
//   - available=false：所有尝试均失败（网络不通或仓库不存在）
//   - available=true, updated=false：仓库已存在且无新提交
//   - available=true, updated=true：成功获取更新
func GitCloneOrPull(ctx context.Context, client *http.Client, repoURL, destDir string) (available, updated bool) {
	gitDir := destDir + "/.git"

	// ── 已有仓库：先尝试 pull ─────────────────────────────────────────────────
	if _, err := os.Stat(gitDir); err == nil {
		hashBefore := gitHash(destDir)
		if err := exec.CommandContext(ctx, "git", "-C", destDir, "pull").Run(); err == nil {
			return true, hashBefore != gitHash(destDir)
		}
		// pull 失败（remote 变了 / 损坏）→ 删除后重 clone
		os.RemoveAll(destDir) //nolint:errcheck
	}

	// ── 全新 clone ────────────────────────────────────────────────────────────
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		return false, false // 目录已存在但非 git 仓库，不覆盖
	}

	// 按 ghproxy.net → mirror.ghproxy.com → 直连顺序逐一尝试 clone
	for _, url := range CandidateURLs(ctx, client, repoURL) {
		slog.Info("downloader: cloning repo", "url", url)
		if runGitClone(ctx, url, destDir) == nil {
			return true, true
		}
		slog.Warn("downloader: clone failed, trying next source", "url", url)
	}

	slog.Error("downloader: git clone failed from all sources", "repo", repoURL)
	return false, false
}

// runGitClone 执行 git clone --depth 1。
func runGitClone(ctx context.Context, url, destDir string) error {
	return exec.CommandContext(ctx, "git", "clone", "--depth", "1", url, destDir).Run()
}

// gitHash 返回指定目录仓库的当前 HEAD short hash；失败返回空串。
func gitHash(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GitShortHash 返回 dir 仓库当前 HEAD 的 short hash（供调用方获取版本号）。
func GitShortHash(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
