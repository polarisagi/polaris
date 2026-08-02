package types

type Intent struct {
	Query      string
	WorkingDir string
	Env        map[string]string
}

type AgentResult struct {
	Output    string
	LatencyMs int64
}

type HeadlessOption func(*HeadlessOptions)

type HeadlessOptions struct {
	MaxRounds int
	// SpawnDepth 透传发起方（如 DefaultTaskWorker/MCPA2AWorker）读到的
	// TaskEntry.SpawnDepth，供 AcquireHeadless 注入新建 Agent 的 sCtx，使
	// transfer_to_agent 委派链深度校验（inv_M8_06）对经 headless 路径执行的
	// 委派任务同样生效（ADR-0084；此前恒为 0，深度上限从未真正启用）。
	SpawnDepth int
	// Namespace 透传发起方（DefaultTaskWorker）读到的 TaskEntry.Namespace
	// （GD-14-001 协同任务共享记忆命名空间），供 AcquireHeadless 在 Run() 前
	// 调用 AgentController.SetMemoryNamespace 注入。2026-08-02 补齐：此前
	// PostTask/PeekTask 早已正确读写 namespace 列，但 AcquireHeadless 从未
	// 调用 SetMemoryNamespace——本地 agent_handoff:<role> 委派任务落到
	// DefaultTaskWorker 后，NamespaceID 全程恒为空，委派方与被委派方无法共享
	// 记忆检索范围（ADR-0084"已知限制"，见 99-new-findings.md 阶段05 P-03
	// 续 发现）。空值 = 不共享，等同于引入本机制前的行为。
	Namespace string
}

// WithSpawnDepth 设置本次 headless 执行继承的委派链深度（ADR-0084）。
func WithSpawnDepth(depth int) HeadlessOption {
	return func(o *HeadlessOptions) { o.SpawnDepth = depth }
}

// WithNamespace 设置本次 headless 执行继承的协同任务共享记忆命名空间
// （GD-14-001，2026-08-02 补齐）。
func WithNamespace(ns string) HeadlessOption {
	return func(o *HeadlessOptions) { o.Namespace = ns }
}
