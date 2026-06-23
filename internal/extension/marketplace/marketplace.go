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

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPMarketplaceClient handles interactions with external MCP registries.
type MCPMarketplaceClient struct {
	httpClient     *http.Client
	registryURL    string
	baseInstallDir string
}

// NewMCPMarketplaceClient 创建市场客户端。
// httpClient 必须是经 SafeDialer 包装的客户端（来自 network.NewSafeHTTPClient）；
// 传 nil 时降级为裸 http.Client（仅测试场景允许）。
func NewMCPMarketplaceClient(registryURL, baseInstallDir string, httpClient *http.Client) *MCPMarketplaceClient {
	if registryURL == "" {
		registryURL = "https://registry.modelcontextprotocol.io/v0.1"
	}
	if httpClient == nil {
		panic("marketplace: httpClient must not be nil; inject a SafeDialer-backed client")
	}
	return &MCPMarketplaceClient{
		httpClient:     httpClient,
		registryURL:    registryURL,
		baseInstallDir: baseInstallDir,
	}
}

// mcpRegistryResponse 对应 registry.modelcontextprotocol.io /v0.1/servers 响应体。
type mcpRegistryResponse struct {
	Servers []mcpRegistryServer `json:"servers"`
}

type mcpRegistryServer struct {
	Server mcpServerDef `json:"server"`
}

type mcpServerDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Version     string         `json:"version"`
	Repository  mcpRepository  `json:"repository"`
	Remotes     []mcpRemoteDef `json:"remotes"`
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
		entry := mapMCPServer(s.Server)
		results = append(results, entry)
	}
	return results, nil
}

// mapMCPServer 将注册表原始服务器定义映射为 RegistryEntry。
func mapMCPServer(s mcpServerDef) protocol.RegistryEntry {
	entry := protocol.RegistryEntry{
		ID:          s.Name,
		Publisher:   publisherFromName(s.Name),
		Type:        "mcp",
		TrustTier:   int(types.TrustCommunity),
		Name:        s.Name,
		Description: s.Description,
		Homepage:    s.Repository.URL,
		Timeout:     60,
	}
	// 优先取第一个 remote 作为连接方式
	if len(s.Remotes) > 0 {
		r := s.Remotes[0]
		entry.Transport = r.Type
		entry.URL = r.URL
	}
	return entry
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
func verifyDownload(ctx context.Context, client *http.Client, filePath, expectedHex, checksumURL string, trustTier int) error {
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
		slog.Warn("marketplace: no checksum available for community extension, skipping verification",
			"file", filepath.Base(filePath), "trust_tier", trustTier)
		return nil
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
func fetchChecksumFromURL(ctx context.Context, client *http.Client, checksumURL, filename string) (string, error) {
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

	pluginDir := filepath.Join(c.baseInstallDir, strings.ReplaceAll(pkg.ID, "/", "_"))
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
		if err := downloader.DownloadFile(ctx, c.httpClient, pkg.URL, binaryPath); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "marketplace: binary download failed", err)
		}
		// 下载完成后立即校验 SHA-256，不通过则拒绝并删除文件
		if err := verifyDownload(ctx, c.httpClient, binaryPath, pkg.Checksum, pkg.ChecksumURL, pkg.TrustTier); err != nil {
			_ = os.Remove(binaryPath)
			return "", apperr.Wrap(apperr.CodeInternal, "marketplace: checksum verification failed", err)
		}
		actualCommand = binaryPath
	}

	// Generate .mcp.json — 根据传输类型选择正确字段
	var serverDef protocol.MCPServerDef
	switch pkg.Transport {
	case "http", "streamable-http", "streamable_http":
		// HTTP transport：URL 是 MCP 端点，无本地命令
		serverDef = protocol.MCPServerDef{
			Type: "http",
			URL:  pkg.URL,
			Env:  pkg.Env,
		}
	case "sse":
		serverDef = protocol.MCPServerDef{
			Type: "sse",
			URL:  pkg.URL,
			Env:  pkg.Env,
		}
	default:
		// stdio（默认）：本地进程
		serverDef = protocol.MCPServerDef{
			Type:    "stdio",
			Command: actualCommand,
			Args:    pkg.Args,
			Env:     pkg.Env,
		}
	}
	mcpConfig := protocol.MCPConfig{
		MCPServers: map[string]protocol.MCPServerDef{
			pkg.Name: serverDef,
		},
	}

	mcpData, err := json.MarshalIndent(mcpConfig, "", "  ")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: marshal mcp.json failed", err)
	}

	mcpPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpPath, mcpData, 0644); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: failed to write .mcp.json", err)
	}

	// Generate .polaris-plugin/plugin.json
	pluginMetaDir := filepath.Join(pluginDir, ".polaris-plugin")
	if err := os.MkdirAll(pluginMetaDir, 0755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: failed to create .polaris-plugin directory", err)
	}

	manifest := protocol.PluginJSON{
		Name:        pkg.Name,
		Version:     "1.0.0", // from market
		Description: pkg.Description,
		MCPServers:  "./.mcp.json",
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: marshal plugin.json failed", err)
	}

	manifestPath := filepath.Join(pluginMetaDir, "plugin.json")
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		slog.Error("marketplace: failed to write plugin.json", "err", err)
		return "", apperr.Wrap(apperr.CodeInternal, "marketplace: failed to write plugin.json", err)
	}

	slog.Info("marketplace: install success", "pkg_id", pkg.ID, "dir", pluginDir)
	return pluginDir, nil
}
