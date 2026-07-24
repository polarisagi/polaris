package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// StagingManager 驱动 7 阶段流水线。
type StagingManager interface {
	Submit(ctx context.Context, c types.StagingCandidate) (string, error)
	GetStage(ctx context.Context, id string) (string, error)
	Promote(ctx context.Context, id string) error // 通过当前阶段 → 下一阶段
	Reject(ctx context.Context, id string, reason string) error
	Rollback(ctx context.Context, id string, reason string) error
}

// EvaluatorLevel represents one level in the evaluation pyramid.
type EvaluatorLevel int

const (
	Level1Assert     EvaluatorLevel = iota // deterministic string/regex check
	Level2Schema                           // JSON schema validation
	Level3Trajectory                       // tool call sequence matching
	Level4LLMJudge                         // semantic quality assessment
	Level5Human                            // calibration only
)

// BehaviorType 描述 eval 用例期望验证的行为类型（Gap-B）。
type BehaviorType string

const (
	BehaviorToolCallSequence BehaviorType = "tool_call_sequence" // L1/L2 确定性：工具调用序列匹配
	BehaviorSemanticQuality  BehaviorType = "semantic_quality"   // L4 LLM Judge：语义质量评估
	BehaviorFormatCompliance BehaviorType = "format_compliance"  // L1/L2 确定性：输出格式校验
	BehaviorSafetyBoundary   BehaviorType = "safety_boundary"    // L2+L4 双重：安全边界校验
)

// FalsifiabilityThreshold 是跳过 L4 LLM Judge 的可评分性阈值。
const FalsifiabilityThreshold = 0.5

type Severity string

const (
	SeverityP0 Severity = "P0" // block merge
	SeverityP1 Severity = "P1" // warn
	SeverityP2 Severity = "P2" // record only
)

// EvalCase is a single evaluation scenario.
type EvalCase struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	Input               map[string]any `json:"input"`
	Expected            map[string]any `json:"expected"`
	Level               EvaluatorLevel `json:"level"`
	Severity            Severity       `json:"severity"`
	Tags                []string       `json:"tags,omitempty"`
	Source              string         `json:"source,omitempty"`
	FalsifiabilityScore float64        `json:"falsifiability_score,omitempty"`
	BehaviorType        BehaviorType   `json:"behavior_type,omitempty"`
	Config              map[string]any `json:"config,omitempty"`
	NeedsHumanAudit     bool           `json:"needs_human_audit,omitempty"`
}

type EvalResult struct {
	CaseID   string `json:"case_id"`
	Passed   bool   `json:"passed"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
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

type TrajectoryTrace struct {
	SessionID  string             `json:"session_id"`
	LLMCalls   []LLMCallRecord    `json:"llm_calls"`
	ToolCalls  []ToolCallRecord   `json:"tool_calls"`
	StateTrans []StateTransRecord `json:"state_transitions"`
}

// @consumer internal/eval/harness/benchmark/benchmark.go (Benchmark Runner)
type BenchmarkAdapter interface {
	Name() string
	Load(ctx context.Context, datasetPath string) ([]EvalCase, error)
}

// @consumer internal/eval/harness/eval.go (Trajectory Recorder/Replayer)
type TrajectoryRecorder interface {
	Record(ctx context.Context, sessionID string) (*TrajectoryTrace, error)
}

// @consumer internal/eval/harness/eval.go (Trajectory Recorder/Replayer)
type TrajectoryReplayer interface {
	Replay(ctx context.Context, trace *TrajectoryTrace) (*EvalResult, error)
}

// EvalRunner 执行评测套件。
// safety case 一票否决: newly_failing safety → reject（无视整体 pass_rate）。
type EvalRunner interface {
	RunSuite(ctx context.Context, suite string, candidateID string) (*types.EvalRunReport, error)
	RunReplay(ctx context.Context, sessionID string) (*types.ReplayReport, error)
	Cancel(ctx context.Context, runID string) error
}

// EvalAPI 暴露给自进化引擎的内部只读数据接口
type EvalAPI interface {
	// GetTrainingCases 获取用于训练和优化的评测用例。
	// signature 必须是用 agentRole 对应 Ed25519 私钥对请求参数及时间戳的签名。
	GetTrainingCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase

	// GetValidationCases 获取用于泛化验证的评测用例。
	GetValidationCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase
}
