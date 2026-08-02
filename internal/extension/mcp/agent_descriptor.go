package mcp

import (
	"context"
	"encoding/json"
)

// ADR-0084：把外部 Agent 建模为一种特殊的 MCP 工具类别——已连接的 MCP Server
// 若暴露约定名工具 A2ADelegateToolName，即视为具备 A2A 出站委派能力；若同时
// 暴露 A2AListAgentsToolName，则可枚举其内部可寻址的子 Agent。二者均为纯约定
// （非 MCP 协议本身的一部分），供 executeTransferToAgent 的 "mcp:<server>/<agent>"
// 前缀寻址与 list_a2a_agents 内置工具使用。
const (
	// A2ADelegateToolName 目标 MCP Server 必须暴露的委派入口工具名。
	A2ADelegateToolName = "a2a_delegate"
	// A2AListAgentsToolName 可选：目标 MCP Server 暴露此工具时，
	// ListA2AAgents 用它枚举可寻址的子 Agent；未暴露时退化为单条 "default"。
	A2AListAgentsToolName = "a2a_list_agents"
)

// MCPAgentDescriptor 描述一个可通过 "mcp:<Server>/<Agent>" 寻址的外部委派目标。
type MCPAgentDescriptor struct {
	Server      string
	Agent       string
	Description string
}

// a2aListAgentsEntry 是 a2a_list_agents 工具约定的响应元素形状。
type a2aListAgentsEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListA2AAgents 枚举所有已连接、暴露 A2ADelegateToolName 的 MCP Server 及其
// 可寻址子 Agent。供内置工具 list_a2a_agents 使用，是 target_agent_role 的
// "mcp:<server>/<agent>" 前缀约定第一次拥有的真实可发现路径（ADR-0084）。
// 单个 Server 的枚举失败不影响其余 Server 的结果（尽力而为，descriptor 里
// 用 Description 携带失败原因，不中断整体调用）。
func (m *MCPManager) ListA2AAgents(ctx context.Context) ([]MCPAgentDescriptor, error) {
	type candidate struct {
		serverID string
		name     string
		hasList  bool
	}

	m.mu.RLock()
	candidates := make([]candidate, 0, len(m.entries))
	for id, e := range m.entries {
		if e == nil || e.errMsg != "" {
			continue // tombstone/连接失败的 entry 不参与枚举
		}
		if !hasToolNamed(e.tools, A2ADelegateToolName) {
			continue
		}
		candidates = append(candidates, candidate{
			serverID: id,
			name:     e.name,
			hasList:  hasToolNamed(e.tools, A2AListAgentsToolName),
		})
	}
	m.mu.RUnlock()

	descriptors := make([]MCPAgentDescriptor, 0, len(candidates))
	for _, c := range candidates {
		if !c.hasList {
			descriptors = append(descriptors, MCPAgentDescriptor{
				Server:      c.name,
				Agent:       "default",
				Description: "(server did not expose a2a_list_agents; single default target assumed)",
			})
			continue
		}

		raw, err := m.CallTool(ctx, c.serverID, A2AListAgentsToolName, map[string]any{})
		if err != nil {
			descriptors = append(descriptors, MCPAgentDescriptor{
				Server:      c.name,
				Agent:       "default",
				Description: "(a2a_list_agents call failed: " + err.Error() + ")",
			})
			continue
		}

		var entries []a2aListAgentsEntry
		if uerr := json.Unmarshal([]byte(raw), &entries); uerr != nil || len(entries) == 0 {
			descriptors = append(descriptors, MCPAgentDescriptor{
				Server:      c.name,
				Agent:       "default",
				Description: "(a2a_list_agents returned an unparseable or empty payload)",
			})
			continue
		}
		for _, e := range entries {
			name := e.Name
			if name == "" {
				name = "default"
			}
			descriptors = append(descriptors, MCPAgentDescriptor{Server: c.name, Agent: name, Description: e.Description})
		}
	}
	return descriptors, nil
}

// ResolveServerIDByName 按 LLM 侧服务器名（mcp:<server>/<agent> 前缀中的
// server 段）反查 entries map 的内部 serverID 键。target_agent_role 的寻址
// 约定用的是服务器名而非内部 serverID（与 MCPToolName 的命名惯例一致），
// CallTool 则要求 serverID，故需要这一反查（ADR-0084）。
func (m *MCPManager) ResolveServerIDByName(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, e := range m.entries {
		if e != nil && e.errMsg == "" && e.name == name {
			return id, true
		}
	}
	return "", false
}

// hasToolNamed 判断工具列表中是否存在指定名称的工具。
func hasToolNamed(tools []MCPTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
