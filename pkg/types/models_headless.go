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
}

// WithSpawnDepth 设置本次 headless 执行继承的委派链深度（ADR-0084）。
func WithSpawnDepth(depth int) HeadlessOption {
	return func(o *HeadlessOptions) { o.SpawnDepth = depth }
}
