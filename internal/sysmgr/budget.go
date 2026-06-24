package sysmgr

import (
	"context"
	"time"
)

// ResourceBudget 定义执行任务或工具的资源预算。
// 用于 CC-2: GlobalCognitivePressure 场景下的资源受限降级。
type ResourceBudget struct {
	MaxTokens    int           // 最大 Token 消耗限制
	CPUQuota     time.Duration // CPU 执行时间分配
	AllowNetwork bool          // 是否允许进行网络请求
}

type budgetCtxKey struct{}

// WithBudget 在 Context 中注入 ResourceBudget 限制。
func WithBudget(ctx context.Context, budget ResourceBudget) context.Context {
	return context.WithValue(ctx, budgetCtxKey{}, budget)
}

// GetBudget 提取 Context 中的资源预算。如果没有设定，则根据当前压力级别提供默认值。
func GetBudget(ctx context.Context) ResourceBudget {
	if b, ok := ctx.Value(budgetCtxKey{}).(ResourceBudget); ok {
		return b
	}

	// 默认回退逻辑，根据当前的认知压力决定默认预算
	level := GetPressureManager().Current()
	switch level {
	case PressureCritical:
		return ResourceBudget{
			MaxTokens:    1024,
			CPUQuota:     5 * time.Second,
			AllowNetwork: false,
		}
	case PressureHigh:
		return ResourceBudget{
			MaxTokens:    4096,
			CPUQuota:     30 * time.Second,
			AllowNetwork: true,
		}
	default: // PressureNormal
		return ResourceBudget{
			MaxTokens:    16384,
			CPUQuota:     2 * time.Minute,
			AllowNetwork: true,
		}
	}
}

// CheckBudget 校验当前是否超出预算。
// 内部可以通过探针或者实际的 Token Counter 进行消耗比对。
func (b *ResourceBudget) CheckBudget(consumedTokens int, elapsed time.Duration) bool {
	if b.MaxTokens > 0 && consumedTokens > b.MaxTokens {
		return false
	}
	if b.CPUQuota > 0 && elapsed > b.CPUQuota {
		return false
	}
	return true
}
