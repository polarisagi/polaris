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

// CtxCapabilityTokenKey 用于在 context 中传递能力令牌 *token.Token (A-7/inv_M7_01)
type CtxCapabilityTokenKey struct{}

// CtxBackgroundWorkKey 标记当前调用链属于可降级的后台工作。InferenceRouter 据此以
// priority=1 申请 LLM 额度（受水位线约束），且不刷新"用户活跃"时间（否则后台自己的
// 推理会把空闲窗口顶掉）。值为 backgroundMode，决定被水位线拒绝时的处置。
type CtxBackgroundWorkKey struct{}

type backgroundMode int

const (
	backgroundSuspend backgroundMode = iota + 1 // 挂起重试至 ctx 到期
	backgroundDefer                             // 立即返回 ErrBackgroundDeferred
)

// WithBackgroundWork 标记后台工作：被水位线拒绝时挂起，直到压力解除或 ctx 到期
// （空闲自进化、headless 自动化）。
func WithBackgroundWork(ctx context.Context) context.Context {
	return context.WithValue(ctx, CtxBackgroundWorkKey{}, backgroundSuspend)
}

// WithDeferrableBackgroundWork 标记可推迟的后台工作：被水位线拒绝时不挂起，立即返回
// ErrBackgroundDeferred，由调用方择机重试。用于串行消费队列（outbox）——挂起一条会
// 阻塞其后与用户相关的记录（Agent 中断、消息持久化重试）。
func WithDeferrableBackgroundWork(ctx context.Context) context.Context {
	return context.WithValue(ctx, CtxBackgroundWorkKey{}, backgroundDefer)
}

func backgroundModeOf(ctx context.Context) backgroundMode {
	if ctx == nil {
		return 0
	}
	m, _ := ctx.Value(CtxBackgroundWorkKey{}).(backgroundMode)
	return m
}

// IsBackgroundWork 未标记即视为用户可见请求。
func IsBackgroundWork(ctx context.Context) bool { return backgroundModeOf(ctx) != 0 }

// IsDeferrableBackgroundWork 是否为可推迟的后台工作。
func IsDeferrableBackgroundWork(ctx context.Context) bool {
	return backgroundModeOf(ctx) == backgroundDefer
}

// CtxProjectRootKey 会话所属项目的工作目录（规范路径，ADR-0097 决策五）。
// 由 agent 在执行 effect 前注入（仅当项目绑定了目录且目录的规范路径未变），
// 内置文件/命令工具据此把该目录追加为本次调用的可访问根——会话级收窄，
// 不改动进程级 sandbox.allowed_paths。
type CtxProjectRootKey struct{}

// WithProjectRoot 注入项目工作目录；root 为空时原样返回。
func WithProjectRoot(ctx context.Context, root string) context.Context {
	if root == "" {
		return ctx
	}
	return context.WithValue(ctx, CtxProjectRootKey{}, root)
}

// ProjectRootFrom 读取项目工作目录；未注入返回空串。
func ProjectRootFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	root, _ := ctx.Value(CtxProjectRootKey{}).(string)
	return root
}

// CtxProjectIDKey 会话所属项目 ID（ADR-0097 决策三修订）。由 agent 在执行 effect 前
// 注入；EpisodicMem.Append 在事件未显式打标时以此兜底，memory_search 工具据此限定
// 情景记忆检索范围。未注入视为默认项目（fail-closed：看不到带项目标记的记忆）。
type CtxProjectIDKey struct{}

// WithProjectID 注入项目 ID；id 为空时原样返回。
func WithProjectID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, CtxProjectIDKey{}, id)
}

// ProjectIDFrom 读取项目 ID；未注入返回空串。
func ProjectIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(CtxProjectIDKey{}).(string)
	return id
}
