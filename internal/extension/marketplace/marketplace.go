package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"log/slog"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPMarketplaceClient handles interactions with external MCP registries.
type MCPMarketplaceClient struct {
	httpClient     network.SafeHTTPClient
	registryURL    string
	baseInstallDir string
}

// NewMCPMarketplaceClient 创建市场客户端。
// httpClient 必须是经 SafeDialer 包装的客户端（来自 network.NewSafeHTTPClient）。
func NewMCPMarketplaceClient(registryURL, baseInstallDir string, httpClient network.SafeHTTPClient) (*MCPMarketplaceClient, error) {
	if registryURL == "" {
		registryURL = "https://registry.modelcontextprotocol.io/v0.1"
	}
	if !httpClient.IsSafe() {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: httpClient 必须是经 SafeDialer 包装的 SafeHTTPClient")
	}
	return &MCPMarketplaceClient{
		httpClient:     httpClient,
		registryURL:    registryURL,
		baseInstallDir: baseInstallDir,
	}, nil
}

// mcpRegistryResponse 对应 registry.modelcontextprotocol.io /v0.1/servers 响应体。
type mcpRegistryResponse struct {
	Servers []mcpRegistryServer `json:"servers"`
}

type mcpRegistryServer struct {
	Server mcpServerDef `json:"server"`
}

type mcpServerDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Repository  mcpRepository   `json:"repository"`
	Remotes     []mcpRemoteDef  `json:"remotes"`
	Packages    []mcpPackageDef `json:"packages"`
}

type mcpRepository struct {
	URL string `json:"url"`
}

type mcpRemoteDef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Search 查询官方 MCP 注册表（GET /v0.1/servers?search=<query>）并映射为 RegistryEntry 列表。
func (c *MCPMarketplaceClient) Search(ctx context.Context, query string) ([]protocol.RegistryEntry, error) {
	searchURL := fmt.Sprintf("%s/servers?search=%s", c.registryURL, url.QueryEscape(query))
	slog.Info("marketplace: searching for packages", "query", query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		slog.Error("marketplace: invalid search request", "err", err)
		return nil, apperr.Wrap(apperr.CodeInternal, "marketplace: invalid search request", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marketplace: search failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("marketplace: search returned %d", resp.StatusCode))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marketplace: failed to read response", err)
	}

	var raw mcpRegistryResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marketplace: failed to parse response", err)
	}

	results := make([]protocol.RegistryEntry, 0, len(raw.Servers))
	for _, s := range raw.Servers {
		if entry, ok := registryEntry(s.Server); ok {
			results = append(results, entry)
		}
	}
	return results, nil
}

// publisherFromName 从 "publisher/name" 格式提取 publisher 部分。
func publisherFromName(name string) string {
	if idx := strings.Index(name, "/"); idx > 0 {
		return name[:idx]
	}
	return name
}

// verifyDownload 校验已下载文件的 SHA-256。
// expectedHex 非空时直接比对；否则尝试从 checksumURL 拉取并解析。
// 两者均为空时：社区来源记录 Warn 并放行（降级策略），官方来源返回 error。
func verifyDownload(ctx context.Context, client network.SafeHTTPClient, filePath, expectedHex, checksumURL string, trustTier int) error {
	if expectedHex == "" && checksumURL != "" {
		hex, err := fetchChecksumFromURL(ctx, client, checksumURL, filepath.Base(filePath))
		if err != nil {
			slog.Warn("marketplace: checksum fetch failed", "url", checksumURL, "err", err)
		} else {
			expectedHex = hex
		}
	}

	if expectedHex == "" {
		if trustTier >= int(types.TrustOfficial) {
			return apperr.New(apperr.CodeInternal, "marketplace: official extension missing checksum")
		}
		// F7: 社区插件无 checksum 时拒绝安装
		return apperr.New(apperr.CodeInternal, "marketplace: community extension missing checksum (rejected)")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marketplace: open file for checksum", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marketplace: sha256 read", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expectedHex) {
		return apperr.New(apperr.CodeInternal,
			fmt.Sprintf("marketplace: checksum mismatch (expected %s, got %s)", expectedHex, actual))
	}
	slog.Info("marketplace: checksum verified", "file", filepath.Base(filePath))
	return nil
}

// fetchChecksumFromURL 下载 checksums.txt 并提取指定文件名的 SHA-256。
// 格式：每行 "<sha256hex>  <filename>"（与 GitHub Releases 格式一致）。
func fetchChecksumFromURL(ctx context.Context, client network.SafeHTTPClient, checksumURL, filename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "fetchChecksumFromURL", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "fetchChecksumFromURL", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("fetchChecksumFromURL: server returned %d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 上限 1MB
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "fetchChecksumFromURL", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[1], filename) {
			return fields[0], nil
		}
	}
	return "", apperr.New(apperr.CodeNotFound, fmt.Sprintf("fetchChecksumFromURL: filename %q not found in checksums", filename))
}

// Install auto-configures the downloaded MCP server into a local plugin layout.
//
//nolint:gocyclo,nestif
func (c *MCPMarketplaceClient) Install(ctx context.Context, pkg protocol.RegistryEntry) (string, error) {
	// HTTP/SSE 传输的 MCP 服务器无本地命令，仅需 URL；stdio 类型必须有 command
	isRemote := pkg.Transport == "streamable-http" || pkg.Transport == "streamable_http" ||
		pkg.Transport == "http" || pkg.Transport == "sse"
	if !isRemote && pkg.Command == "" {
		return "", apperr.New(apperr.CodeInternal, "marketplace: package missing install command")
	}

	// GR-8-003：pkg.ID / pkg.Command 来自远端市场元数据，不可信。原实现只把 "/"
	// 替换成 "_"：ID 为 ".." 时 pluginDir 即 baseInstallDir 的父目录，紧接着的
	// RemoveAll 会递归删除它；Windows 下 "\" 分隔符与 Command 中的 "../" 同样
	// 可逃逸。目录名必须是单段本地名，Command 必须是 pluginDir 内的本地相对路径。
	dirName := strings.NewReplacer("/", "_", "\\", "_").Replace(pkg.ID)
	if !filepath.IsLocal(dirName) || dirName == "." || strings.ContainsRune(dirName, filepath.Separator) {
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace: invalid package id")
	}
	if !isRemote && !filepath.IsLocal(pkg.Command) && pkg.URL != "" && pkg.URL != "npx-mode" {
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace: package command escapes install dir")
	}
	pluginDir := filepath.Join(c.baseInstallDir, dirName)
	_ = os.RemoveAll(pluginDir)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: failed to create directory", err)
	}

	// 动态安装逻辑：根据 URL 判断是否需要下载二进制（仅 stdio 模式适用）
	actualCommand := pkg.Command
	if !isRemote && pkg.URL != "" && pkg.URL != "npx-mode" {
		// 这是需要下载二进制文件的模式
		binaryPath := filepath.Join(pluginDir, pkg.Command)
		if runtime.GOOS == "windows" {
			binaryPath += ".exe"
		}

		slog.Info("marketplace: downloading binary release", "url", pkg.URL, "to", binaryPath)
		if err := downloader.DownloadFile(ctx, c.httpClient.Client, pkg.URL, binaryPath); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "marketplace: binary download failed", err)
		}
		// 下载完成后立即校验 SHA-256，不通过则拒绝并删除文件
		if err := verifyDownload(ctx, c.httpClient, binaryPath, pkg.Checksum, pkg.ChecksumURL, pkg.TrustTier); err != nil {
			_ = os.Remove(binaryPath)
			return "", apperr.Wrap(apperr.CodeInternal, "marketplace: checksum verification failed", err)
		}
		actualCommand = binaryPath
	}

	// 生成标准 .mcp.json（两家共同的 mcpServers 映射）；独立连接器由 MCPInstaller 经 pluginspec 读取。
	server := map[string]any{"env": pkg.Env}
	switch pkg.Transport {
	case "http", "streamable-http", "streamable_http":
		server["type"], server["url"] = "http", pkg.URL
	case "sse":
		server["type"], server["url"] = "sse", pkg.URL
	default:
		server["type"], server["command"], server["args"] = "stdio", actualCommand, pkg.Args
	}
	mcpData, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{pkg.Name: server}}, "", "  ")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: marshal mcp.json failed", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ".mcp.json"), mcpData, 0o644); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: failed to write .mcp.json", err)
	}

	slog.Info("marketplace: install success", "pkg_id", pkg.ID, "dir", pluginDir)
	return pluginDir, nil
}
