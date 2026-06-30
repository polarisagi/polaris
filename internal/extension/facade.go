package extension

import (
	"context"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ExtensionFacade extension 包对外统一接口（扩展生命周期 + MCP 连接管理）。
//
// 对外暴露：
//   - 扩展生命周期：Install / Uninstall / Activate / ListInstalled
//   - MCP 连接管理：ConnectMCP / DisconnectMCP / CallMCPTool / ListMCPServers
//
// 所有 MCP 类型均使用 protocol.MCPClientConfig / protocol.MCPServerInfo（canonical SSoT）。
// 内部 lifecycle FSM / marketplace 授权 / MCP 传输层对调用方透明。
//
// @consumer: gateway/server/server.go
// @producer: extension/bus.ExtensionBus + mcp.MCPManager（由 cli.go/bootstrap 构造注入）
type ExtensionFacade interface {
	// --- 扩展生命周期 ---

	// Install 安装一个扩展（plugin/skill/mcp/app），经授权校验后执行安装流程。
	Install(ctx context.Context, req marketplace.InstallRequest) error

	// Uninstall 卸载指定扩展（按 catalogID 级联清理工具注册、进程、DB 记录）。
	Uninstall(ctx context.Context, catalogID string) error

	// ListInstalled 返回当前所有已安装扩展实例（含状态）。
	ListInstalled(ctx context.Context) ([]types.ExtInstanceRow, error)

	// Activate 根据当前任务目标语义激活最相关的扩展，返回激活提示列表。
	// 供 agent 每轮 Think 前调用，动态扩展工具集。
	Activate(ctx context.Context, goal string) ([]protocol.ActivatedHint, error)

	// --- MCP 连接管理 ---

	// ConnectMCP 连接一个 MCP 服务器并注册其工具（热插拔入口）。
	// serverID 为内部唯一 ID（对应 mcp_servers.id），cfg 来自 extension catalog。
	ConnectMCP(ctx context.Context, serverID, name string, cfg protocol.MCPClientConfig) error

	// DisconnectMCP 断开 MCP 服务器连接并注销其工具。
	DisconnectMCP(serverID string)

	// CallMCPTool 调用指定 MCP 服务器的工具（由 tool/dispatch 路由层调用）。
	CallMCPTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error)

	// MCPToolSchemas 返回所有在线 MCP 服务器的工具 Schema（供注入 InferRequest）。
	MCPToolSchemas() []types.ToolSchema

	// ListMCPServers 返回所有 MCP 服务器的运行时状态快照。
	ListMCPServers() []protocol.MCPServerInfo

	// IsPluginConnected 检查指定 Plugin 的 MCP 进程是否在线。
	IsPluginConnected(pluginID string) bool
}
