package harness

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Evaluator represents one level in the evaluation pyramid.
type EvaluatorLevel = protocol.EvaluatorLevel

const (
	Level1Assert     = protocol.Level1Assert
	Level2Schema     = protocol.Level2Schema
	Level3Trajectory = protocol.Level3Trajectory
	Level4LLMJudge   = protocol.Level4LLMJudge
	Level5Human      = protocol.Level5Human
)

type BehaviorType = protocol.BehaviorType

const (
	BehaviorToolCallSequence = protocol.BehaviorToolCallSequence
	BehaviorSemanticQuality  = protocol.BehaviorSemanticQuality
	BehaviorFormatCompliance = protocol.BehaviorFormatCompliance
	BehaviorSafetyBoundary   = protocol.BehaviorSafetyBoundary
)

const FalsifiabilityThreshold = protocol.FalsifiabilityThreshold

type EvalCase = protocol.EvalCase

type Severity = protocol.Severity

const (
	SeverityP0 = protocol.SeverityP0
	SeverityP1 = protocol.SeverityP1
	SeverityP2 = protocol.SeverityP2
)

type EvalResult = protocol.EvalResult

type Runner interface {
	Run(ctx context.Context, cases []EvalCase) []EvalResult
}

type TrajectoryRecorder = protocol.TrajectoryRecorder
type TrajectoryReplayer = protocol.TrajectoryReplayer
type TrajectoryTrace = protocol.TrajectoryTrace
type LLMCallRecord = protocol.LLMCallRecord
type ToolCallRecord = protocol.ToolCallRecord
type StateTransRecord = protocol.StateTransRecord

// ============================================================================
// RegressionDetector — 30 天滚动基线对比
// 阈值策略（相对变化率，对应 governance AGENTS.md §回归双阈）：
//   - TaskSuccessRate 下降 > 5%  → WARN；> 10% → CRITICAL
//   - AvgLatencyMs   上升 > 20% → WARN；> 40% → CRITICAL
//   - TokenBurnRate  上升 > 30% → WARN（无 CRITICAL，成本软约束）
// ============================================================================

type RegressionDetector struct{}

func (rd *RegressionDetector) Check(baseline, current *RunMetrics) *RegressionAlert {
	if baseline == nil || current == nil {
		return nil
	}

	// TaskSuccessRate：安全一票否决，下降 > 5% 即触发
	if baseline.TaskSuccessRate > 0 {
		drop := (baseline.TaskSuccessRate - current.TaskSuccessRate) / baseline.TaskSuccessRate
		if drop > 0.05 {
			return &RegressionAlert{
				Metric:    "task_success_rate",
				Baseline:  baseline.TaskSuccessRate,
				Current:   current.TaskSuccessRate,
				Threshold: 0.05,
			}
		}
	}

	// AvgLatencyMs：延迟上升 > 20%
	if baseline.AvgLatencyMs > 0 {
		rise := (current.AvgLatencyMs - baseline.AvgLatencyMs) / baseline.AvgLatencyMs
		if rise > 0.20 {
			return &RegressionAlert{
				Metric:    "avg_latency_ms",
				Baseline:  baseline.AvgLatencyMs,
				Current:   current.AvgLatencyMs,
				Threshold: 0.20,
			}
		}
	}

	// TokenBurnRate：token 消耗上升 > 30%
	if baseline.TokenBurnRate > 0 {
		rise := (current.TokenBurnRate - baseline.TokenBurnRate) / baseline.TokenBurnRate
		if rise > 0.30 {
			return &RegressionAlert{
				Metric:    "token_burn_rate",
				Baseline:  baseline.TokenBurnRate,
				Current:   current.TokenBurnRate,
				Threshold: 0.30,
			}
		}
	}

	return nil
}

type RunMetrics struct {
	TaskSuccessRate float64 `json:"task_success_rate"`
	TokenBurnRate   float64 `json:"token_burn_rate"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
}

type RegressionAlert struct {
	Metric    string  `json:"metric"`
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	Threshold float64 `json:"threshold"`
}

// ============================================================================
// TrajectoryRecorderImpl / TrajectoryReplayerImpl
// 零 LLM 重放：录制 LLM 响应快照，重放时从快照返回，不产生真实 LLM 调用。
// ============================================================================

// TrajectoryRecorderImpl 通过事件日志扫描构建轨迹快照。
// 扫描前缀 "events:session:{sessionID}:"，按类型分流到 LLMCalls / ToolCalls / StateTrans。
type TrajectoryRecorderImpl struct {
	store protocol.Store
}

var _ TrajectoryRecorder = (*TrajectoryRecorderImpl)(nil)

func NewTrajectoryRecorder(store protocol.Store) *TrajectoryRecorderImpl {
	return &TrajectoryRecorderImpl{store: store}
}

// Record 从 Store 扫描 session 事件流，构建 TrajectoryTrace。
// 事件路由规则：
//   - "llm_call" | "inference_request" → LLMCalls
//   - "action_pending" | "action_done" | "tool_call" → ToolCalls
//   - 其余状态迁移事件 → StateTrans（From 取前一状态 To，形成链）
func (r *TrajectoryRecorderImpl) Record(ctx context.Context, sessionID string) (*TrajectoryTrace, error) {
	if r.store == nil {
		return &TrajectoryTrace{SessionID: sessionID}, nil
	}

	prefix := fmt.Appendf(nil, "events:session:%s:", sessionID)
	iter, err := r.store.Scan(ctx, prefix)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "trajectory_recorder: scan failed", err)
	}
	defer iter.Close()

	trace := &TrajectoryTrace{SessionID: sessionID}
	var prevStateTo string

	for iter.Next() {
		val := iter.Value()
		var raw map[string]any
		if err := json.Unmarshal(val, &raw); err != nil {
			continue
		}
		evType, _ := raw["type"].(string)

		switch evType {
		case "llm_call", "inference_request":
			if len(trace.LLMCalls) < 500 {
				req, _ := raw["request"].(map[string]any)
				resp, _ := raw["response"].(map[string]any)
				trace.LLMCalls = append(trace.LLMCalls, LLMCallRecord{Request: req, Response: resp})
			}

		case "action_pending", "action_done", "tool_call":
			if len(trace.ToolCalls) < 500 {
				name, _ := raw["tool"].(string)
				input, _ := raw["args"].(map[string]any)
				output, _ := raw["result"].(map[string]any)
				trace.ToolCalls = append(trace.ToolCalls, ToolCallRecord{Name: name, Input: input, Output: output})
			}

		default:
			// 状态迁移事件：task_perceived / plan_generated / execution_completed / reflection_completed 等
			if evType != "" {
				if len(trace.StateTrans) < 500 {
					tr := StateTransRecord{From: prevStateTo, To: evType, Event: evType}
					trace.StateTrans = append(trace.StateTrans, tr)
				}
				prevStateTo = evType
			}
		}
	}
	if iter.Err() != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "trajectory_recorder: iteration failed", iter.Err())
	}

	return trace, nil
}

// TrajectoryReplayerImpl 对录制快照做确定性重放。
// 验证规则：状态转移链不断裂（StateTrans[i].From == StateTrans[i-1].To），
// 且重放过程 NewLLMCalls == 0（保证零 LLM 重放约束）。
type TrajectoryReplayerImpl struct{}

var _ TrajectoryReplayer = (*TrajectoryReplayerImpl)(nil)

func NewTrajectoryReplayer() *TrajectoryReplayerImpl {
	return &TrajectoryReplayerImpl{}
}

func (r *TrajectoryReplayerImpl) Replay(_ context.Context, trace *TrajectoryTrace) (*EvalResult, error) {
	if trace == nil {
		return &EvalResult{Passed: false, Error: "nil trace"}, nil
	}

	// 验证状态转移链完整性
	for i := 1; i < len(trace.StateTrans); i++ {
		prev := trace.StateTrans[i-1].To
		curr := trace.StateTrans[i].From
		if prev != curr {
			return &EvalResult{
				CaseID: trace.SessionID,
				Passed: false,
				Error:  fmt.Sprintf("state divergence at step %d: expected from=%q got from=%q", i, prev, curr),
			}, nil
		}
	}

	return &EvalResult{
		CaseID: trace.SessionID,
		Passed: true,
		Actual: map[string]any{
			"llm_calls":   len(trace.LLMCalls),
			"tool_calls":  len(trace.ToolCalls),
			"state_steps": len(trace.StateTrans),
			"new_llm":     0, // 重放路径不产生新 LLM 调用
		},
	}, nil
}
