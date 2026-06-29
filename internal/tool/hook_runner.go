package tool

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/action/hook"
)

// RunHookScript 执行用户配置的 hook 脚本。
//
// Deprecated: 逻辑已迁移至 internal/action/hook.RunScript（ADR-重构）。
// 此包装层保留以维持现有测试有效，禁止在新代码中直接调用。
// 新调用方请使用 action/hook.RunScript。
func RunHookScript(ctx context.Context, path string, env []string, timeout time.Duration) (int, string, error) {
	return hook.RunScript(ctx, path, env, timeout)
}
