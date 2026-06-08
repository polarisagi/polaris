package marketplace

import (
	"context"
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

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// MCPMarketplaceClient handles interactions with external MCP registries.
type MCPMarketplaceClient struct {
	httpClient     *http.Client
	registryURL    string
	baseInstallDir string
}

// NewMCPMarketplaceClient creates a new client. Default registry is registry.modelcontextprotocol.io
func NewMCPMarketplaceClient(registryURL, baseInstallDir string) *MCPMarketplaceClient {
	if registryURL == "" {
		registryURL = "https://registry.modelcontextprotocol.io/v0.1"
	}
	return &MCPMarketplaceClient{
		httpClient:     &http.Client{},
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
		return nil, perrors.Wrap(perrors.CodeInternal, "marketplace: invalid search request", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "marketplace: search failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("marketplace: search returned %d", resp.StatusCode))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "marketplace: failed to read response", err)
	}

	var raw mcpRegistryResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "marketplace: failed to parse response", err)
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
		TrustTier:   int(protocol.TrustCommunity),
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

// Install auto-configures the downloaded MCP server into a local plugin layout.
//
//nolint:gocyclo,nestif
func (c *MCPMarketplaceClient) Install(ctx context.Context, pkg protocol.RegistryEntry) (string, error) {
	// HTTP/SSE 传输的 MCP 服务器无本地命令，仅需 URL；stdio 类型必须有 command
	isRemote := pkg.Transport == "streamable-http" || pkg.Transport == "streamable_http" ||
		pkg.Transport == "http" || pkg.Transport == "sse"
	if !isRemote && pkg.Command == "" {
		return "", perrors.New(perrors.CodeInternal, "marketplace: package missing install command")
	}

	pluginDir := filepath.Join(c.baseInstallDir, strings.ReplaceAll(pkg.ID, "/", "_"))
	_ = os.RemoveAll(pluginDir)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to create directory", err)
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pkg.URL, nil)
		if err != nil {
			return "", perrors.Wrap(perrors.CodeInternal, "marketplace: invalid download request", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return "", perrors.Wrap(perrors.CodeInternal, "marketplace: binary download failed", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("marketplace: download returned %d", resp.StatusCode))
		}

		outFile, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to create binary file", err)
		}

		if _, err := io.Copy(outFile, resp.Body); err != nil {
			outFile.Close()
			return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to write binary file", err)
		}
		outFile.Close()
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
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: marshal mcp.json failed", err)
	}

	mcpPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpPath, mcpData, 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to write .mcp.json", err)
	}

	// Generate .codex-plugin/plugin.json
	pluginMetaDir := filepath.Join(pluginDir, ".codex-plugin")
	if err := os.MkdirAll(pluginMetaDir, 0755); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to create .codex-plugin directory", err)
	}

	manifest := protocol.PluginJSON{
		Name:        pkg.Name,
		Version:     "1.0.0", // from market
		Description: pkg.Description,
		MCPServers:  "./.mcp.json",
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: marshal plugin.json failed", err)
	}

	manifestPath := filepath.Join(pluginMetaDir, "plugin.json")
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		slog.Error("marketplace: failed to write plugin.json", "err", err)
		return "", perrors.Wrap(perrors.CodeInternal, "marketplace: failed to write plugin.json", err)
	}

	slog.Info("marketplace: install success", "pkg_id", pkg.ID, "dir", pluginDir)
	return pluginDir, nil
}
