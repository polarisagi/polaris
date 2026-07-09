package chat

import (
	"context"
	"encoding/json"
	"strings"
)

// ============================================================================
// 插件 / MCP / App 感知摘要构建（R7 拆分自 system_prompt.go）。
// InjectSystemPrompt 主入口见 system_prompt.go；ambient skills 见
// system_prompt_ambient.go。
// ============================================================================

// buildExtensionSummary 构建插件/MCP/App 感知摘要字符串（单行，| 分隔）。
// 只注入名称和连接状态；详细工具参数由 BuildToolSchemas() 注入 function schema 传递，避免双重注入。
func (s *ChatHandler) buildExtensionSummary(ctx context.Context) string {
	var parts []string
	if s.DB != nil {
		if plugParts := s.queryPluginSummary(ctx); len(plugParts) > 0 {
			parts = append(parts, "Plugins: "+strings.Join(plugParts, ", "))
		}
		if appParts := s.queryAppSummary(ctx); len(appParts) > 0 {
			parts = append(parts, "Apps: "+strings.Join(appParts, ", "))
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
func (s *ChatHandler) queryPluginSummary(ctx context.Context) []string {
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
		_ = json.Unmarshal([]byte(policyJSON), &policy)

		connected, total := 0, 0
		for serverName, entry := range policy {
			enabled := true
			if v, ok := entry["enabled"].(bool); ok {
				enabled = v
			}
			if !enabled {
				continue
			}
			total++
			if connectedSet["plugin_"+plugID+"_"+serverName] {
				connected++
			}
		}

		mark := "✗"
		if total > 0 && connected == total {
			mark = "✓"
		} else if connected > 0 {
			mark = "~"
		}
		result = append(result, label+"("+mark+")")
	}
	return result
}

// queryAppSummary 查询已启用 App 的显示名称列表。
func (s *ChatHandler) queryAppSummary(ctx context.Context) []string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT display_name, name FROM apps WHERE enabled=1`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var displayName, name string
		if rows.Scan(&displayName, &name) != nil {
			continue
		}
		label := displayName
		if label == "" {
			label = name
		}
		result = append(result, label)
	}
	return result
}

// standaloneMCPSummary 返回非插件独立 MCP 服务的名称+连接状态列表。
func (s *ChatHandler) standaloneMCPSummary() []string {
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
