package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxPluginDownloadBytes 单个插件包（archive / npm tarball）下载上限，防止恶意市场条目耗尽磁盘。
const maxPluginDownloadBytes = 256 << 20

// SourceFetcher 按市场条目来源取回插件目录（ADR-0103 决策七）。所有出站 HTTP 走 SafeDialer；
// git 子进程绕过 SafeDialer，由 ValidateGitURL 在 Go 层先行拦截，且只接受 https。
type SourceFetcher struct {
	http   network.SafeHTTPClient
	tmpDir string
}

func NewSourceFetcher(httpClient network.SafeHTTPClient, tmpDir string) *SourceFetcher {
	return &SourceFetcher{http: httpClient, tmpDir: tmpDir}
}

// Fetched 取回结果：Dir 为插件根；Version 为可比较的来源版本（npm 版本 / git 标签版本），可空。
type Fetched struct {
	Dir     string
	Version string
}

// Fetch 把 src 取回到全新目录 destDir。ref 覆盖来源自身的 ref（依赖版本解析选中的标签）。
func (f *SourceFetcher) Fetch(ctx context.Context, src pluginspec.PluginSource, ref, destDir string) (Fetched, error) {
	if _, err := os.Stat(destDir); err == nil {
		return Fetched{}, apperr.New(apperr.CodeConflict, "marketplace: destination already exists: "+destDir)
	}
	switch src.Type {
	case pluginspec.SourceRelative:
		return Fetched{Dir: destDir}, copyTree(src.Path, destDir)
	case pluginspec.SourceGitHub:
		return f.fetchGit(ctx, "https://github.com/"+src.Repo+".git", firstNonEmpty(ref, src.Ref), src.SHA, "", destDir)
	case pluginspec.SourceURL:
		return f.fetchGit(ctx, src.URL, firstNonEmpty(ref, src.Ref), src.SHA, "", destDir)
	case pluginspec.SourceGitSubdir:
		return f.fetchGit(ctx, gitURL(src.URL), firstNonEmpty(ref, src.Ref), src.SHA, src.Path, destDir)
	case pluginspec.SourceNPM:
		return f.fetchNPM(ctx, src, destDir)
	case pluginspec.SourceArchive:
		return Fetched{Dir: destDir}, f.fetchArchive(ctx, src, destDir)
	}
	return Fetched{}, apperr.New(apperr.CodeInvalidInput, "marketplace: unsupported plugin source "+src.Type)
}

// gitURL git-subdir 的 url 接受 GitHub owner/repo 简写（Claude 规则）。
func gitURL(u string) string {
	if !strings.Contains(u, "://") && strings.Count(u, "/") == 1 {
		return "https://github.com/" + u + ".git"
	}
	return u
}

func (f *SourceFetcher) fetchGit(ctx context.Context, repoURL, ref, sha, subdir, destDir string) (Fetched, error) {
	validator := func(u string) error { return network.ValidateGitURL(ctx, u) }
	if subdir == "" {
		if err := downloader.GitFetchRevision(ctx, validator, repoURL, ref, sha, destDir); err != nil {
			return Fetched{}, apperr.Wrap(apperr.CodeOf(err), "marketplace: git source", err)
		}
		if err := os.RemoveAll(filepath.Join(destDir, ".git")); err != nil {
			return Fetched{}, apperr.Wrap(apperr.CodeInternal, "marketplace: strip .git", err)
		}
		return Fetched{Dir: destDir, Version: tagVersion(ref)}, nil
	}
	work, err := os.MkdirTemp(f.tmpDir, "git-subdir-*")
	if err != nil {
		return Fetched{}, apperr.Wrap(apperr.CodeInternal, "marketplace: temp dir", err)
	}
	defer os.RemoveAll(work) //nolint:errcheck
	clone := filepath.Join(work, "repo")
	if err := downloader.GitFetchRevision(ctx, validator, repoURL, ref, sha, clone); err != nil {
		return Fetched{}, apperr.Wrap(apperr.CodeOf(err), "marketplace: git-subdir source", err)
	}
	sub := filepath.Join(clone, filepath.FromSlash(subdir))
	if rel, err := filepath.Rel(clone, sub); err != nil || strings.HasPrefix(rel, "..") {
		return Fetched{}, apperr.New(apperr.CodeInvalidInput, "marketplace: git-subdir path escapes the repository")
	}
	return Fetched{Dir: destDir, Version: tagVersion(ref)}, copyTree(sub, destDir)
}

// tagVersion 依赖解析选中的 "<name>--v<version>" 标签中的版本。
func tagVersion(ref string) string {
	if _, v, ok := strings.Cut(ref, "--v"); ok {
		return v
	}
	return ""
}

// fetchArchive https zip；给出 sha256 时不匹配即拒绝。插件根可在压缩包顶层或下一层（Claude 规则）。
func (f *SourceFetcher) fetchArchive(ctx context.Context, src pluginspec.PluginSource, destDir string) error {
	zipPath, digest, err := f.download(ctx, src.URL, nil, "archive-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(zipPath) //nolint:errcheck
	if src.SHA256 != "" && !strings.EqualFold(src.SHA256, digest) {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: archive sha256 mismatch")
	}
	stage := destDir + ".stage"
	defer os.RemoveAll(stage) //nolint:errcheck
	if err := downloader.ExtractZip(zipPath, stage, nil); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "marketplace: extract archive", err)
	}
	return os.Rename(pluginRootWithin(stage), destDir) //nolint:wrapcheck
}

// pluginRootWithin 顶层无插件标记且只有一个子目录时，插件根在下一层。
func pluginRootWithin(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || hasPluginMarker(dir) || len(entries) != 1 || !entries[0].IsDir() {
		return dir
	}
	return filepath.Join(dir, entries[0].Name())
}

func hasPluginMarker(dir string) bool {
	for _, m := range []string{".claude-plugin", ".codex-plugin", "plugin.json", "skills", "commands", "agents", "hooks", "SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// download 把 https 地址下载到临时文件，返回路径与 sha256；超过上限即中止。
func (f *SourceFetcher) download(ctx context.Context, rawURL string, headers map[string]string, pattern string) (string, string, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return "", "", apperr.New(apperr.CodeForbidden, "marketplace: only https downloads are allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeInvalidInput, "marketplace: download request", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: download "+rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", apperr.New(apperr.CodeNetworkUnavailable, "marketplace: download "+rawURL+": "+resp.Status)
	}
	tmp, err := os.CreateTemp(f.tmpDir, pattern)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "marketplace: temp file", err)
	}
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxPluginDownloadBytes+1))
	if err != nil || n > maxPluginDownloadBytes {
		os.Remove(tmp.Name()) //nolint:errcheck
		if err == nil {
			err = apperr.New(apperr.CodeResourceExhausted, "download exceeds size limit")
		}
		return "", "", apperr.Wrap(apperr.CodeOf(err), "marketplace: download "+rawURL, err)
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// copyTree 复制插件目录；符号链接一律跳过（Claude：本地来源遍历到符号链接不读取）。
func copyTree(src, dst string) error {
	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr //nolint:wrapcheck
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			return nil
		case d.IsDir():
			return os.MkdirAll(target, 0o755) //nolint:wrapcheck
		}
		return copyRegular(path, target)
	}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marketplace: copy "+src, err)
	}
	return nil
}

func copyRegular(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyRegular", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyRegular", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o600)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyRegular", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "copyRegular", err)
	}
	return out.Close() //nolint:wrapcheck
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
