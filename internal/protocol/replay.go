package protocol

import "sync/atomic"

var isReplaying atomic.Bool

// SetReplayMode 设置全局回放模式标志。
func SetReplayMode(replaying bool) {
	isReplaying.Store(replaying)
}

// IsReplaying 返回当前是否处于全局回放模式。
// M4、M5、M11 在执行 EmitEvent、ToolCall、Outbox 投递等外部副作用前，
// 必须检查此标志，若为 true 则物理切断副作用。
func IsReplaying() bool {
	return isReplaying.Load()
}

// ReplayLLMCall 崩溃恢复回放用的历史 LLM 调用记录（M04-Agent-Kernel.md §8，
// 2026-07-22 接线）。Request/Response 字段形状与
// internal/eval/harness.LLMCallRecord 同源——均来自
// TrajectoryRecorderImpl.Record 对 events:session:{id}: 前缀的扫描重建
// （payload["request"]/["response"] 原样透传）。
//
// 类型落在 internal/protocol（L0）而非直接复用 harness.LLMCallRecord：
// internal/agent 是 L1，internal/eval/harness 是 L3，Test_inv_NoCrossLayerImport
// 禁止 L1 反向 import L3（HE-3：接口/共享类型在消费方能触达的最低层定义）。
// cmd/polaris（组合根）负责把 harness.LLMCallRecord 转换为本类型。
type ReplayLLMCall struct {
	Request  map[string]any
	Response map[string]any
}
