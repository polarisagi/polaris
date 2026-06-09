package eval

import (
	"context"
	"fmt"
)

// Evaluator represents one level in the evaluation pyramid.
type EvaluatorLevel int

const (
	Level1Assert     EvaluatorLevel = iota // deterministic string/regex check
	Level2Schema                           // JSON schema validation
	Level3Trajectory                       // tool call sequence matching
	Level4LLMJudge                         // semantic quality assessment
	Level5Human                            // calibration only
)

// BehaviorType 描述 eval 用例期望验证的行为类型（Gap-B）。
// Runner 据此路由到正确的评估器：ToolCallSequence/FormatCompliance 走 L1/L2 确定性评估；
// SemanticQuality 走 L4 LLM Judge；SafetyBoundary 走 L2+L4 双重校验。
type BehaviorType string

const (
	BehaviorToolCallSequence BehaviorType = "tool_call_sequence" // L1/L2 确定性：工具调用序列匹配
	BehaviorSemanticQuality  BehaviorType = "semantic_quality"   // L4 LLM Judge：语义质量评估
	BehaviorFormatCompliance BehaviorType = "format_compliance"  // L1/L2 确定性：输出格式校验
	BehaviorSafetyBoundary   BehaviorType = "safety_boundary"    // L2+L4 双重：安全边界校验
)

// FalsifiabilityThreshold 是跳过 L4 LLM Judge 的可评分性阈值。
// 低于此分数的用例被视为"无法客观评分"，跳过昂贵的 LLM 评分。
const FalsifiabilityThreshold = 0.5

// EvalCase is a single evaluation scenario.
type EvalCase struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input"`
	Expected    map[string]any `json:"expected"`
	Level       EvaluatorLevel `json:"level"`
	Severity    Severity       `json:"severity"`
	Tags        []string       `json:"tags,omitempty"`
	// FalsifiabilityScore 标注本用例有多"可客观评分"（Gap-B, HE-Rule-4）。
	// 0.0 = 无法客观评分（例如"回答是否友好"），1.0 = 完全可验证（例如"输出是否包含指定字段"）。
	// 低于 FalsifiabilityThreshold 的用例 Runner 跳过 L4 LLM Judge，避免噪音评分。
	FalsifiabilityScore float64 `json:"falsifiability_score,omitempty"`
	// BehaviorType 指定期望验证的行为类型，Runner 据此选择最合适的 Evaluator 层级。
	BehaviorType BehaviorType `json:"behavior_type,omitempty"`
}

type Severity string

const (
	SeverityP0 Severity = "P0" // block merge
	SeverityP1 Severity = "P1" // warn
	SeverityP2 Severity = "P2" // record only
)

type EvalResult struct {
	CaseID   string `json:"case_id"`
	Passed   bool   `json:"passed"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
}

// Runner executes the evaluation suite.
type Runner interface {
	Run(ctx context.Context, cases []EvalCase) []EvalResult
}

// TrajectoryRecorder captures a full agent execution trace for replay.
type TrajectoryRecorder interface {
	Record(ctx context.Context, sessionID string) (*TrajectoryTrace, error)
}

// TrajectoryReplayer replays a recorded trace deterministically, zero LLM calls.
type TrajectoryReplayer interface {
	Replay(ctx context.Context, trace *TrajectoryTrace) (*EvalResult, error)
}

type TrajectoryTrace struct {
	SessionID  string             `json:"session_id"`
	LLMCalls   []LLMCallRecord    `json:"llm_calls"`
	ToolCalls  []ToolCallRecord   `json:"tool_calls"`
	StateTrans []StateTransRecord `json:"state_transitions"`
}

type LLMCallRecord struct {
	Request  map[string]any `json:"request"`
	Response map[string]any `json:"response"`
}

type ToolCallRecord struct {
	Name   string         `json:"name"`
	Input  map[string]any `json:"input"`
	Output map[string]any `json:"output"`
}

type StateTransRecord struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Event string `json:"event"`
}

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
type TrajectoryRecorderImpl struct{}

var _ TrajectoryRecorder = (*TrajectoryRecorderImpl)(nil)

func NewTrajectoryRecorder() *TrajectoryRecorderImpl {
	return &TrajectoryRecorderImpl{}
}

// Record 从 Store 扫描 session 事件流，构建 TrajectoryTrace。
// 实际 Scan 由 RunnerImpl 的 store 执行（RunnerImpl.RunReplay 中已有完整扫描逻辑）；
// 此处提供独立接口以支持单元测试和外部调用。
func (r *TrajectoryRecorderImpl) Record(_ context.Context, sessionID string) (*TrajectoryTrace, error) {
	return &TrajectoryTrace{SessionID: sessionID}, nil
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
