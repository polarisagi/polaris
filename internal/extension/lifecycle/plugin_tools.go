package lifecycle

import (
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
)

// PluginToolRef 插件内裸工具名解析到的运行时坐标。
type PluginToolRef struct {
	// ServerID 是 MCPManager.entries 的键（mcp_servers.id），View 宿主的资源/RPC 接口按它定位服务器。
	ServerID string
	// RegisteredName 是工具在 ToolRegistry 中的注册名（mcp__<server>__<tool>）。
	RegisteredName string
	Tool           protocol.MCPTool
	Connected      bool
}

// ResolvePluginTool 把清单里写的插件内裸工具名（setupTool/statusTool）解析为注册名。
// 注册名前缀是 scopedServerName 后的服务器名而非插件名，且一个插件可带多个子 MCP，
// 所以只能到该插件的运行时服务器里按 MCP 原始工具名查，不能靠字符串拼接推断。
// 服务器归属以安装期固定的 ID 前缀 "plugin_<pluginID>_" 判定（registerBundleMCP）。
func ResolvePluginTool(servers []protocol.MCPServerInfo, pluginID, bareName string) (PluginToolRef, bool) {
	if pluginID == "" || bareName == "" {
		return PluginToolRef{}, false
	}
	prefix := "plugin_" + pluginID + "_"
	for _, srv := range servers {
		if !strings.HasPrefix(srv.ID, prefix) {
			continue
		}
		for _, t := range srv.Tools {
			if t.Name == bareName {
				return PluginToolRef{
					ServerID:       srv.ID,
					RegisteredName: mcp.MCPToolName(srv.Name, t.Name),
					Tool:           t,
					Connected:      srv.Connected,
				}, true
			}
		}
	}
	return PluginToolRef{}, false
}
