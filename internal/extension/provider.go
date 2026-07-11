package extension

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 extension 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// extension 包（扩展注册 + MCP 管理）需要以下外部能力：
//   1. ExtensionPolicyGate — 扩展安装授权策略检查（Cedar deny-by-default）
//   2. EmbedSearcher       — 扩展语义激活时的向量检索（native.ExtensionActivator 依赖）
//   3. ToolRegistrar       — MCP 工具注册回调（已在 mcp/mcp_manager.go 声明，此处文档化）
//
// @consumer: extension/marketplace/manager.go（授权），extension/native/extension_activator.go（激活）
// @producer: 各具体模块由 cli.go/bootstrap 注入

// ExtensionPolicyGate extension 包对安全策略检查的消费端接口。
// 预期由 Cedar 策略引擎（`internal/security/policy.Gate`，deny-by-default）实现，
// 签名（`AllowInstall(ctx, principal, extType, trustTier) bool`）与
// `security/policy.Gate.IsAuthorized(ctx, principal, action, resource, ctxData) (bool, error)`
// 不一致，当前全仓库零消费方、零实现——本次审查核实为未接线的消费端接口占位，
// 不在本次范围内处理（非 doc02 迁移点名对象），如实标注避免误导。
// 禁止：extension 直接 import security（防止 extension→security 循环）
type ExtensionPolicyGate interface {
	// AllowInstall 检查指定主体（principal）是否有权安装该扩展类型。
	// principal 为认证后的用户/系统身份，extType 为 plugin/skill/mcp/app。
	AllowInstall(ctx context.Context, principal, extType string, trustTier int) bool
}

// EmbedSearcher extension/native 对向量检索的消费端接口（语义激活路径）。
// 实现：knowledge.KnowledgeFacade 或 memory.MemoryFacade（通过 DependencyMap["EmbedSearcher"] 注入）
// 用途：ExtensionActivator 根据任务 goal 向量化后检索最相关的扩展描述，决定激活哪些扩展。
type EmbedSearcher interface {
	// Search 按语义相似度检索最相关的 k 个扩展实例。
	// query 为任务描述文本，返回按 Score 降序排列的 ExtInstanceRow。
	Search(ctx context.Context, query string, k int) ([]types.ExtInstanceRow, error)
}
