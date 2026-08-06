package protocol

import (
	"context"
	"time"
)

// detachedContext 实现了 context.Context，只继承 Values，不继承 Deadline/Cancel
type detachedContext struct {
	parent context.Context
}

func (d detachedContext) Deadline() (deadline time.Time, ok bool) {
	return
}

func (d detachedContext) Done() <-chan struct{} {
	return nil
}

func (d detachedContext) Err() error {
	return nil
}

func (d detachedContext) Value(key any) any {
	return d.parent.Value(key)
}

// Detach 创建一个脱离原生命周期（取消和超时）但继承上下文中所有值的 Context。
// 常用于开启后台 Goroutine 时，保留原请求的 TraceID 等身份信息。
func Detach(ctx context.Context) context.Context {
	return detachedContext{parent: ctx}
}

// CtxCapabilityToken 已删除（2026-08-06，ADR-0088 决策二）：
// 该键自定义以来从未被任何代码写入过，唯一读取点是 agent_execute_dag.go 里
// 一段结构上不可达的 MaxCallsPerTask 配额校验。留着一个"看起来是能力令牌
// 传递通道、实际没有任何东西经过"的键，只会让人误判系统里已有该防护。
// 令牌使用次数的真实兑现点是 security/token.TokenManager.Consume。

// CtxDryRun 用于在 context 中指示当前是否为 dry run 模式
type CtxDryRun struct{}

// CtxIdempotencyKey 用于在 context 中传递幂等键
type CtxIdempotencyKey struct{}

// CtxTaskIDKey 用于在 context 中传递任务 ID (防止 TOCTOU)
type CtxTaskIDKey struct{}

// CtxSessionIDKey 用于在 context 中传递 Session ID
type CtxSessionIDKey struct{}

// CtxAgentIDKey 用于在 context 中传递 Agent ID (防止 TOCTOU)
type CtxAgentIDKey struct{}

// CtxVersionKey 用于在 context 中传递乐观锁版本 (防止 TOCTOU)
type CtxVersionKey struct{}

// CtxTaintLevelKey 用于在 context 中向进程内工具传递当前调用的污点等级
// （UP-03：core_memory_edit 等需要按写入时污点落库的工具消费；只升不降由消费方保证）。
type CtxTaintLevelKey struct{}

// CtxAnomalyFilterKey 用于在 context 中传递 AnomalyDistanceFilter 实例 (按会话隔离)
type CtxAnomalyFilterKey struct{}
