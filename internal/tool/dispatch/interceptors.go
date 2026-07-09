// interceptors.go 只实现横切关注点（当前仅审计）。
//
// PolicyGate / Capability Token / 沙箱分级 / RateLimit / Idempotency 均已是
// protocol.ToolRegistry.ExecuteTool（internal/tool/tool.go）与
// protocol.SkillExecutor.ExecuteSkill（internal/extension/skill/skill.go）各自的职责，
// 不在此处重复实现——重复实现会产生两套互相可能不一致的安全判定，正是本次重构要消除的
// "两条线"问题本身。曾经存在的 RateLimitInterceptor / IdempotencyInterceptor / DryRunInterceptor
// 均为空桩（调用 next 直接放行，不做任何事），已删除；不留假装生效的占位代码。
package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AuditInterceptor 在工具执行后写入审计轨迹（不阻断响应；写入失败仅记录 WARN）。
// at 为 nil 时该拦截器整体跳过（不影响放行/拒绝语义）。
func AuditInterceptor(at AuditLogger) Interceptor {
	return func(ctx context.Context, entry protocol.CatalogEntry, args []byte, next ExecFn) (*types.ToolResult, error) {
		res, err := next(ctx, entry, args)
		if at == nil {
			return res, err
		}
		if auditErr := at.RecordAudit(ctx, entry.Name, args); auditErr != nil {
			slog.WarnContext(ctx, "dispatch: audit record failed", "tool", entry.Name, "err", auditErr)
		}
		return res, err
	}
}

// SchemaValidateInterceptor 对输入参数进行基础 JSON 校验，防止透传脏数据导致运行时崩溃。
func SchemaValidateInterceptor() Interceptor {
	return func(ctx context.Context, entry protocol.CatalogEntry, args []byte, next ExecFn) (*types.ToolResult, error) {
		if len(args) > 0 {
			var dummy map[string]any
			if err := json.Unmarshal(args, &dummy); err != nil {
				slog.WarnContext(ctx, "dispatch: schema validation failed (invalid json)", "tool", entry.Name, "err", err)
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "tool args validation failed: invalid json", err)
			}
		}
		return next(ctx, entry, args)
	}
}
