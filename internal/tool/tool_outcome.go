// tool_outcome.go：ExecuteTool 调用结果上报（R7 拆分自 tool.go，因新增内容
// 使 tool.go 超过 400 行文件行数上限）。
package tool

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

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

// SessionEventWriter records events to the session trajectory store.
type SessionEventWriter interface {
	WriteToolCallEvent(sessionID, toolName string, input, output map[string]any)
}

// WithSessionEventWriter 注入 SessionEventWriter（可选）。
func (r *InMemoryToolRegistry) WithSessionEventWriter(writer SessionEventWriter) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionEventWriter = writer
	return r
}

// reportOutcome 是 ExecuteTool 两条真实执行结果路径（成功/envelope 报错）共用的
// 上报出口，nil 观察者时零开销跳过。
func (r *InMemoryToolRegistry) reportOutcome(toolName string, success bool, latencyMs int64, errMsg string, ctx context.Context, input []byte, res *types.ToolResult) {
	r.mu.RLock()
	rec := r.outcomeRecorder
	sessionWriter := r.sessionEventWriter
	r.mu.RUnlock()

	if sessionWriter != nil && ctx != nil {
		sessionID, _ := ctx.Value(protocol.CtxSessionIDKey{}).(string)
		if sessionID == "" {
			sessionID, _ = ctx.Value(protocol.CtxTaskIDKey{}).(string)
		}
		if sessionID != "" {
			var inMap map[string]any
			_ = json.Unmarshal(input, &inMap)
			var outMap map[string]any
			if res != nil {
				_ = json.Unmarshal([]byte(res.Output), &outMap)
			} else if errMsg != "" {
				outMap = map[string]any{"error": errMsg}
			}
			sessionWriter.WriteToolCallEvent(sessionID, toolName, inMap, outMap)
		}
	}

	if rec != nil {
		rec.RecordToolOutcome(toolName, success, latencyMs, errMsg)
	}
}
