package llm

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// IsLocalProvider 依据 DB 中的权威字段判定 Provider 是否为本地模型厂商（R2-07 唯一权威判定函数）。
// 契约：只允许依据 DB 中的权威字段（sys_providers.is_local 或 providers.is_local 派生的 ProviderRow.IsLocal）。
// 禁止在任何业务代码中针对 provider 类型或名称做本地判断的字符串比较。
func IsLocalProvider(row types.ProviderRow) bool {
	return row.IsLocal
}

// InferIsLocal 根据厂商类型推导是否为本地厂商（用于手工创建/更新 API 用户未显式指定 is_local 的场景，R3-03）。
func InferIsLocal(providerType string) bool {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "ollama", "llama_server", "llama_local":
		return true
	default:
		return false
	}
}
