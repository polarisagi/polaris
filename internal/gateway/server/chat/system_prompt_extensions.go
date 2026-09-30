package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
)

// ============================================================================
// 插件 / MCP 感知摘要构建（R7 拆分自 system_prompt.go）。
// InjectSystemPrompt 主入口见 system_prompt.go；ambient skills 见
// system_prompt_ambient.go。
//
// 前缀账本（ADR-0105 决策一）：摘要拆成两份——
//   - 名称清单（extensionNames）：只含名称且确定序，进入 L0 稳定层；
//   - 连接状态（extensionStatus）：✓/~/✗ 随 MCP 连接/断开变化，属易变量，进入 L3 易变层。
//
// ============================================================================

// extensionEntry 一个已安装扩展（插件或独立 MCP）的名称与连接标记。
type extensionEntry struct {
	name string
	mark string
}

// extensionSnapshot 是同一时刻插件/独立 MCP 的一致快照，供名称清单与状态摘要各取所需。
type extensionSnapshot struct {
	plugins []extensionEntry
	mcps    []extensionEntry
}

// snapshotExtensions 采集扩展快照。两类条目均按名称（再按原始序）稳定排序，
// 使同一安装集合恒得同一渲染字节，与 DB 行序 / map 遍历序无关。
func (s *PromptAssemblyService) snapshotExtensions(ctx context.Context) extensionSnapshot {
	var snap extensionSnapshot
	if s.DB != nil {
		snap.plugins = s.queryPluginEntries(ctx)
	}
	if s.MCPMgr != nil {
		snap.mcps = s.standaloneMCPEntries()
	}
	sortEntries(snap.plugins)
	sortEntries(snap.mcps)
	return snap
}

func sortEntries(es []extensionEntry) {
	sort.SliceStable(es, func(i, j int) bool { return es[i].name < es[j].name })
}

// extensionNames 渲染稳定层的名称清单（单行，| 分隔），不含任何连接状态。
func (snap extensionSnapshot) extensionNames() string {
	var parts []string
	if len(snap.plugins) > 0 {
		names := make([]string, len(snap.plugins))
		for i, e := range snap.plugins {
			names[i] = e.name
		}
		parts = append(parts, "Plugins: "+strings.Join(names, ", "))
	}
	if len(snap.mcps) > 0 {
		names := make([]string, len(snap.mcps))
		for i, e := range snap.mcps {
			names[i] = e.name
		}
		parts = append(parts, "MCPs: "+strings.Join(names, ", "))
	}
	return strings.Join(parts, " | ")
}

// extensionStatus 渲染易变层的连接状态摘要；无扩展时返回空串。
// ✓ = 全部已连接；~ = 部分连接；✗ = 未连接。
func (snap extensionSnapshot) extensionStatus() string {
	var parts []string
	if len(snap.plugins) > 0 {
		items := make([]string, len(snap.plugins))
		for i, e := range snap.plugins {
			items[i] = e.name + "(" + e.mark + ")"
		}
		parts = append(parts, "Plugins: "+strings.Join(items, ", "))
	}
	if len(snap.mcps) > 0 {
		items := make([]string, len(snap.mcps))
		for i, e := range snap.mcps {
			items[i] = e.name + " " + e.mark
		}
		parts = append(parts, "MCPs: "+strings.Join(items, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Extension connection status: " + strings.Join(parts, " | ")
}

// queryPluginEntries 查询已安装插件名称与 MCP 整体连接状态。
// SQL 必须带 ORDER BY：无排序时 SQLite 行序不保证，插件安装/更新后行序漂移会打断前缀缓存。
func (s *PromptAssemblyService) queryPluginEntries(ctx context.Context) []extensionEntry {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, display_name, mcp_policy FROM plugins WHERE enabled=1 ORDER BY name, id`)
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

	var result []extensionEntry
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

		result = append(result, extensionEntry{name: label, mark: pluginConnectMark(plugID, policy, connectedSet)})
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

// standaloneMCPEntries 返回非插件独立 MCP 服务的名称+连接标记（ListServers 遍历 map，
// 顺序由调用方 sortEntries 固定）。
func (s *PromptAssemblyService) standaloneMCPEntries() []extensionEntry {
	servers := s.MCPMgr.ListServers()
	result := make([]extensionEntry, 0, len(servers))
	for _, srv := range servers {
		if strings.HasPrefix(srv.ID, "plugin_") {
			continue
		}
		mark := "✗"
		if srv.Connected {
			mark = "✓"
		}
		result = append(result, extensionEntry{name: srv.Name, mark: mark})
	}
	return result
}
