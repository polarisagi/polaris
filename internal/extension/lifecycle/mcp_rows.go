package lifecycle

import (
	"encoding/json"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mcpRowParams 构造 mcp_servers 行所需的归属信息（插件子 MCP 与独立连接器共用）。
type mcpRowParams struct {
	ID        string
	Name      string
	PluginID  string
	CatalogID string
	TrustTier int
	Enabled   bool
}

// mcpRowFromSpec 归一化服务器定义 → mcp_servers 行。字符串保留 ${...} 占位原文，
// 由 MCPManager.ConfigFromRow 在每次启动时展开（userConfig 可在安装后修改）。
func mcpRowFromSpec(srv pluginspec.MCPServer, p mcpRowParams) types.MCPServerRow {
	now := time.Now().UTC().Format(time.RFC3339)
	return types.MCPServerRow{
		ID:        p.ID,
		Name:      p.Name,
		Transport: rowTransport(srv.Type),
		Command:   srv.Command,
		Args:      jsonOr(srv.Args, "[]"),
		Env:       jsonOr(srv.Env, "{}"),
		URL:       srv.URL,
		Headers:   jsonOr(srv.Headers, "{}"),
		Enabled:   p.Enabled,
		Timeout:   30,
		TrustTier: p.TrustTier,
		CatalogID: p.CatalogID,
		PluginID:  p.PluginID,
		WorkDir:   srv.Cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// rowTransport pluginspec 归一化类型 → mcp_servers.transport 存储值。
func rowTransport(t string) string {
	if t == pluginspec.MCPTypeHTTP {
		return string(protocol.MCPStreamableHTTP)
	}
	return t
}

func jsonOr[T any](v T, empty string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return empty
	}
	return string(b)
}
