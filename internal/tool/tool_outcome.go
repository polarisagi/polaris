// tool_outcome.go：ExecuteTool 调用结果上报（R7 拆分自 tool.go，因新增内容
// 使 tool.go 超过 400 行文件行数上限）。
package tool

// ToolOutcomeRecorder 消费方接口（防止包循环，定义在调用方）：供工具自进化闭环
// （如 action.PolicyEvolver）在每次真实执行（非幂等缓存命中/限流拦截/DryRun 模拟）
// 完成后记录调用结果，用于滑动窗口成功率统计与失败模式识别（2026-07-12
// unwired-code-audit 补齐：PolicyEvolver 此前完整实现但从未被任何调用方喂入过
// 真实数据，见 internal/action/tool_usage_policy.go 文档注释）。
type ToolOutcomeRecorder interface {
	RecordToolOutcome(toolName string, success bool, latencyMs int64, errMsg string)
}

// WithOutcomeRecorder 注入工具调用结果观察者（可选）。
func (r *InMemoryToolRegistry) WithOutcomeRecorder(rec ToolOutcomeRecorder) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomeRecorder = rec
	return r
}

// reportOutcome 是 ExecuteTool 两条真实执行结果路径（成功/envelope 报错）共用的
// 上报出口，nil 观察者时零开销跳过。
func (r *InMemoryToolRegistry) reportOutcome(toolName string, success bool, latencyMs int64, errMsg string) {
	r.mu.RLock()
	rec := r.outcomeRecorder
	r.mu.RUnlock()
	if rec == nil {
		return
	}
	rec.RecordToolOutcome(toolName, success, latencyMs, errMsg)
}
