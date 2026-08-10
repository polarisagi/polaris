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
	// EventCallback 可选事件回调（GD-13-001 子 Agent 事件透传）：若设置，
	// AcquireHeadless 在消费每个 AgentStreamEvent 时调用此回调，供调用方将
	// 子 Agent 事件中继到 Blackboard 或父 Session Stream。nil = 不回调。
	EventCallback func(AgentStreamEvent)
	// SessionID 透传调用方真实的业务 SessionID（GD-13-001）。AcquireHeadless
	// 内部以 sessionID 为 key 复用/新建 per-session Agent 内核实例（含常驻
	// FSM Run() 循环）；此前恒不设置，AcquireHeadless 每次调用都自行生成
	// "headless-"+时间戳 的一次性 ID，导致同一业务会话（如 orchestrator_headless.go
	// 的多轮 Cron/Workflow/Webhook 对话）每一轮都新建一个内核实例，Pool
	// "按 session 复用/idle 挂起"的设计初衷对 headless 路径完全失效。
	// 空串 = 退回原自生成行为（如 Blackboard DAG 一次性任务执行，本就没有
	// 会话/多轮语义，见 execute/orchestrator/default_worker.go 调用点注释）。
	SessionID string
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

// WithEventCallback 注入流式事件回调（GD-13-001 子 Agent 事件透传）。
// 若设置，AcquireHeadless 在消费每个 AgentStreamEvent 时同步调用 fn（非阻塞建议）。
// fn 不得在内部阻塞，否则会延迟子 Agent 执行；建议配合带缓冲的 channel 异步转发。
func WithEventCallback(fn func(AgentStreamEvent)) HeadlessOption {
	return func(o *HeadlessOptions) { o.EventCallback = fn }
}

// WithSessionID 透传调用方真实的业务 SessionID（GD-13-001），供 AcquireHeadless
// 复用/新建对应的 per-session Agent 内核实例，而非每次调用都自生成一次性 ID。
func WithSessionID(id string) HeadlessOption {
	return func(o *HeadlessOptions) { o.SessionID = id }
}
