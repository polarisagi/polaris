package action

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 action 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// 设计目的：
//   action 包（capability_token + taint_preserving_decoder + tool_usage_policy）
//   被 agent、gateway、tool 等多个上层模块调用。
//   此文件声明 action 包所需的外部依赖，防止与 security/tool 产生循环 import。
//
// @consumer: action/tool_usage_policy.go, action/capability_token.go
// @producer: 各具体模块在构造时注入

// ToolExecutor action 包对工具执行器的消费端接口。
// 实现：tool.Registry（InMemoryToolRegistry）
// 禁止：action 直接 import tool 包（防循环）
type ToolExecutor interface {
	// Execute 按工具名执行工具，返回结构化结果。
	Execute(ctx context.Context, toolName string, params map[string]any) (*types.ToolResult, error)
	// Exists 检查工具是否已注册。
	Exists(toolName string) bool
}

// PolicyStoreReader action 包对策略存储的只读接口（ToolUsagePolicy 持久化）。
// 实现：store/repo.SQLitePreferencesRepo（preferences 表 key-value）
type PolicyStoreReader interface {
	// GetPolicy 读取工具的持久化策略，不存在返回 (nil, nil)。
	GetPolicy(toolName string) (*ToolUsagePolicy, error)
	// SavePolicy 持久化策略（PolicyEvolver 演化后写入）。
	SavePolicy(policy *ToolUsagePolicy) error
}
