package downloader

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// gitSafeArgs 远程插件来源的 git 调用一律禁用本地 file 协议与 LFS 过滤器：来源地址来自第三方市场，
// file:// 或子模块指向本机路径会把宿主文件读进插件目录。
func gitSafeArgs(args ...string) []string {
	return append([]string{"-c", "protocol.file.allow=never", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.required=false"}, args...)
}

// GitFetchRevision 把 repoURL 的 ref（分支/标签）或 sha 检出到全新目录 destDir（浅拉取）。
// 同时给出 sha 时检出 sha 并校验（Claude plugin sources：sha 优先于 ref）。只接受 https 地址。
func GitFetchRevision(ctx context.Context, validator URLValidator, repoURL, ref, sha, destDir string) error {
	if !strings.HasPrefix(repoURL, "https://") {
		return apperr.New(apperr.CodeForbidden, "downloader: only https git sources are allowed: "+repoURL)
	}
	if validator != nil {
		if err := validator(repoURL); err != nil {
			return apperr.Wrap(apperr.CodeForbidden, "downloader: git URL blocked", err)
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: mkdir "+destDir, err)
	}
	target := firstNonEmptyStr(sha, ref, "HEAD")
	var lastErr error
	for _, url := range CandidateURLs(ctx, nil, repoURL) {
		if lastErr = fetchInto(ctx, url, target, destDir); lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: git fetch "+repoURL+"@"+target, lastErr)
	}
	if sha != "" && gitHash(destDir) != sha {
		return apperr.New(apperr.CodeInvalidInput, "downloader: checked out commit does not match pinned sha "+sha)
	}
	return nil
}

func fetchInto(ctx context.Context, url, target, destDir string) error {
	steps := [][]string{
		{"init", "-q", destDir},
		{"-C", destDir, "remote", "remove", "origin"},
		{"-C", destDir, "remote", "add", "origin", url},
		gitSafeArgs("-C", destDir, "fetch", "-q", "--depth", "1", "origin", target),
		gitSafeArgs("-C", destDir, "checkout", "-q", "--force", "FETCH_HEAD"),
	}
	for i, args := range steps {
		out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if err != nil && i != 1 { // remote remove 在首次初始化时必然失败，忽略
			return apperr.Wrap(apperr.CodeInternal, "git "+strings.Join(args, " ")+": "+strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// GitListTags 列出远端标签名（不含 refs/tags/ 前缀），用于依赖版本范围解析。
func GitListTags(ctx context.Context, validator URLValidator, repoURL string) ([]string, error) {
	if !strings.HasPrefix(repoURL, "https://") {
		return nil, apperr.New(apperr.CodeForbidden, "downloader: only https git sources are allowed: "+repoURL)
	}
	if validator != nil {
		if err := validator(repoURL); err != nil {
			return nil, apperr.Wrap(apperr.CodeForbidden, "downloader: git URL blocked", err)
		}
	}
	out, err := exec.CommandContext(ctx, "git", gitSafeArgs("ls-remote", "--tags", "--refs", repoURL)...).Output()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "downloader: git ls-remote "+repoURL, err)
	}
	var tags []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	return tags, nil
}

// GitHeadHash 返回仓库当前 HEAD 完整哈希（缓存键 / 版本回退）。
func GitHeadHash(dir string) string { return gitHash(dir) }

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
