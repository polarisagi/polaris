package tool

import (
	"context"

	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// ToolFacade tool 包对外统一接口（工具注册 + 执行 + 目录）。
//
// 问题背景：
//
//	当前 tool 包对外暴露了 InMemoryToolRegistry / dispatch.Dispatcher /
//	catalog.CompositeCatalog 三个入口，agent.go 和 gateway/server.go
//	分别持有不同的具体 struct，工具调用路径难以追踪。
//
// 解决方案：
//   - ToolFacade 是 tool 包对外的统一入口接口
//   - 注册（Register/Unregister）、执行（Execute）、目录（List/Lookup）统一入口
//   - 内部五阶段 PolicyGate（幂等检查→侧效分析→沙箱路由→执行→审计）对外透明
//
// @consumer: agent/agent.go, gateway/server/server.go, extension/mcp/manager.go
// @producer: tool.InMemoryToolRegistry（由 cli.go/bootstrap 构造注入）
type ToolFacade interface {
	// Register 注册一个工具（builtin 启动时批量注册；MCP 连接后热注册）。
	Register(t types.Tool) error

	// Unregister 注销工具（MCP 断开 / Plugin 卸载时调用）。
	Unregister(name string)

	// Execute 执行指定工具（五阶段 PolicyGate 保护）。
	// taintLevel 由调用方按当前会话污点级别传入。
	Execute(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)

	// List 返回满足最低信任等级的所有工具目录条目。
	// 供 LLM 推理时注入 ToolSchema 使用。
	List(ctx context.Context, minTrust types.TrustTier) []catalog.CatalogEntry

	// Lookup 按工具名查找目录条目（供执行前校验和路由决策使用）。
	Lookup(name string) (catalog.CatalogEntry, bool)

	// Schemas 返回 []types.ToolSchema，可直接注入 InferRequest.Tools。
	Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema
}
