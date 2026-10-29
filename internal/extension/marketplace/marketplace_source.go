package marketplace

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// NormalizeMarketplaceSource 把用户添加市场时的输入规范为同步地址（两家 `marketplace add` 的输入形式）：
//
//	owner/repo[@ref|#ref]        → https://github.com/owner/repo.git[#ref]
//	https://host/repo(.git)[#ref] → git 仓库
//	https://host/marketplace.json → 直链市场文件（相对路径条目无法解析）
//	/abs/dir                      → 本机目录
//
// mcp 市场须为 MCP Registry 的 https 基址。ssh / file / http 一律拒绝（同 downloader.GitFetchRevision）。
func NormalizeMarketplaceSource(input, mpType string) (string, error) {
	in := strings.TrimSpace(input)
	switch {
	case in == "":
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace source is required")
	case mpType == "mcp":
		if !strings.HasPrefix(in, "https://") {
			return "", apperr.New(apperr.CodeInvalidInput, "an MCP registry must be an https URL")
		}
		return strings.TrimRight(in, "/"), nil
	case filepath.IsAbs(in):
		if st, err := os.Stat(in); err != nil || !st.IsDir() || strings.Contains(in, "..") {
			return "", apperr.New(apperr.CodeInvalidInput, "local marketplace must be an existing directory")
		}
		return filepath.Clean(in), nil
	case strings.HasPrefix(in, "https://"):
		return in, nil
	case strings.Contains(in, "://") || strings.HasPrefix(in, "git@"):
		return "", apperr.New(apperr.CodeInvalidInput, "only https marketplace sources are supported")
	}
	return githubMarketplace(in)
}

// githubMarketplace owner/repo[@ref|#ref] → https://github.com/owner/repo.git[#ref]。
func githubMarketplace(in string) (string, error) {
	repo, ref := in, ""
	if i := strings.LastIndexAny(in, "@#"); i > 0 {
		repo, ref = in[:i], in[i+1:]
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(repo, "..") {
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace source must be owner/repo, an https URL or an absolute path")
	}
	out := "https://github.com/" + repo + ".git"
	if ref != "" {
		out += "#" + ref
	}
	return out, nil
}

// splitRef "url#ref" → (url, ref)。
func splitRef(u string) (string, string) {
	if i := strings.LastIndex(u, "#"); i > 0 && !strings.HasSuffix(strings.ToLower(u), ".json") {
		return u[:i], u[i+1:]
	}
	return u, ""
}
