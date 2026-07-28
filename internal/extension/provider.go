package extension

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 extension 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// extension 包（扩展注册 + MCP 管理）需要以下外部能力：
//   1. EmbedSearcher       — 扩展语义激活时的向量检索（native.ExtensionActivator 依赖）
//   2. ToolRegistrar       — MCP 工具注册回调（已在 mcp/mcp_manager.go 声明，此处文档化）
//
// @consumer: extension/marketplace/manager.go（授权），extension/native/extension_activator.go（激活）
// @producer: 各具体模块由 cli.go/bootstrap 注入

// EmbedSearcher extension/native 对向量检索的消费端接口（语义激活路径）。
// 实现：knowledge.KnowledgeFacade 或 memory.MemoryFacade（通过 DependencyMap["EmbedSearcher"] 注入）
// 用途：ExtensionActivator 根据任务 goal 向量化后检索最相关的扩展描述，决定激活哪些扩展。
type EmbedSearcher interface {
	// Search 按语义相似度检索最相关的 k 个扩展实例。
	// query 为任务描述文本，返回按 Score 降序排列的 ExtInstanceRow。
	Search(ctx context.Context, query string, k int) ([]types.ExtInstanceRow, error)
}
