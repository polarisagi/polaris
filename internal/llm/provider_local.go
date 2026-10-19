package llm

import (
	"github.com/polarisagi/polaris/pkg/types"
)

// IsLocalProvider 依据 DB 中的权威字段判定 Provider 是否为本地模型厂商（R2-07 唯一权威判定函数）。
// 契约：只允许依据 DB 中的权威字段（sys_providers.is_local 或 providers.is_local 派生的 ProviderRow.IsLocal）。
// 禁止在任何业务代码中针对 provider 类型或名称做本地判断的字符串比较。
func IsLocalProvider(row types.ProviderRow) bool {
	return row.IsLocal
}
