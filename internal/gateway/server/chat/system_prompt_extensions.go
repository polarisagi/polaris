package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

// ============================================================================
// 插件 / MCP 感知摘要构建（R7 拆分自 system_prompt.go）。
// InjectSystemPrompt 主入口见 system_prompt.go；ambient skills 见
// system_prompt_ambient.go。
// ============================================================================

// buildExtensionSummary 构建插件/MCP 感知摘要字符串（单行，| 分隔）。
// 只注入名称和连接状态；详细工具参数由 BuildToolSchemas() 注入 function schema 传递，避免双重注入。
func (s *PromptAssemblyService) buildExtensionSummary(ctx context.Context) string {
	var parts []string
	if s.DB != nil {
		if plugParts := s.queryPluginSummary(ctx); len(plugParts) > 0 {
			parts = append(parts, "Plugins: "+strings.Join(plugParts, ", "))
		}
	}
	if s.MCPMgr != nil {
		if mcpParts := s.standaloneMCPSummary(); len(mcpParts) > 0 {
			parts = append(parts, "MCPs: "+strings.Join(mcpParts, ", "))
		}
	}
	return strings.Join(parts, " | ")
}

// queryPluginSummary 查询已安装插件名称与 MCP 整体连接状态（格式："PluginName(✓)"）。
// ✓ = 所有 MCP 已连接；~ = 部分连接；✗ = 未连接。
func (s *PromptAssemblyService) queryPluginSummary(ctx context.Context) []string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, display_name, mcp_policy FROM plugins WHERE enabled=1`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	connectedSet := make(map[string]bool)
	if s.MCPMgr != nil {
		for _, srv := range s.MCPMgr.ListServers() {
			connectedSet[srv.ID] = srv.Connected
		}
	}

	var result []string
	for rows.Next() {
		var plugID, plugName, displayName, policyJSON string
		if rows.Scan(&plugID, &plugName, &displayName, &policyJSON) != nil {
			continue
		}
		label := displayName
		if label == "" {
			label = plugName
		}

		var policy map[string]map[string]any
		// 2026-08-02 HE-1 补齐：解析失败此前静默丢弃，policy 保持 nil，下方循环
		// 空转，摘要里该插件会被误报为"0个已连接 MCP"，而非真实反映
		// mcp_policy 数据本身已损坏（L3：仅影响系统提示词摘要展示，非阻断路径，
		// 记录日志即可）。
		if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
			slog.Warn("system_prompt: plugin mcp_policy 解析失败，摘要连接数可能不准确",
				"plugin_id", plugID, "err", err)
		}

		result = append(result, label+"("+pluginConnectMark(plugID, policy, connectedSet)+")")
	}
	if err := rows.Err(); err != nil {
		// 迭代中途出错会让摘要静默少插件；摘要非阻断路径，留痕后返回已读部分。
		slog.Warn("system_prompt: plugins 摘要迭代中断，结果可能不完整", "err", err)
	}
	return result
}

// pluginConnectMark 按 mcp_policy 中启用的子 MCP 计算连接标记：
// ✓ 全部已连接；~ 部分连接；✗ 无连接或无启用子 MCP。
func pluginConnectMark(plugID string, policy map[string]map[string]any, connectedSet map[string]bool) string {
	connected, total := 0, 0
	for serverName, entry := range policy {
		if v, ok := entry["enabled"].(bool); ok && !v {
			continue
		}
		total++
		if connectedSet["plugin_"+plugID+"_"+serverName] {
			connected++
		}
	}
	switch {
	case total > 0 && connected == total:
		return "✓"
	case connected > 0:
		return "~"
	default:
		return "✗"
	}
}

// standaloneMCPSummary 返回非插件独立 MCP 服务的名称+连接状态列表。
func (s *PromptAssemblyService) standaloneMCPSummary() []string {
	result := make([]string, 0, len(s.MCPMgr.ListServers()))
	for _, srv := range s.MCPMgr.ListServers() {
		if strings.HasPrefix(srv.ID, "plugin_") {
			continue
		}
		mark := "✗"
		if srv.Connected {
			mark = "✓"
		}
		result = append(result, srv.Name+" "+mark)
	}
	return result
}
