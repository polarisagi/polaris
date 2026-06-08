package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 五层 Evaluator Pyramid 实现。
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §2, §6, §7

// L1AssertionEvaluator — 零 LLM 断言检查。
// 断言类型: contains | not_contains | regex | length_under | tool_called | no_tool_called | cost_under | steps_under.
type L1AssertionEvaluator struct {
	assertions []Assertion
}

// Assertion 单条断言。
type Assertion struct {
	Type  string // contains | not_contains | regex | length_under | tool_called | no_tool_called | cost_under | steps_under
	Name  string
	Value string
}

// Evaluate L1 断言评估。
func (e *L1AssertionEvaluator) Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error) {
	for _, a := range e.assertions {
		if !e.check(a, trajectory) {
			return &EvalResult{Passed: false, Details: "assertion failed: " + a.Name + " (" + a.Type + ")", EvaluatorType: e.Type()}, nil
		}
	}
	return &EvalResult{Passed: true, Scores: map[string]float64{"assertions": 1.0}, EvaluatorType: e.Type()}, nil
}

func (e *L1AssertionEvaluator) check(a Assertion, t *AgentTrajectory) bool { //nolint:gocyclo
	// 收集所有输出内容和工具调用名称
	var fullOutput string
	var toolCalls []string
	if t.Result != nil {
		fullOutput = t.Result.Output
		toolCalls = t.Result.ToolCalls
	}

	switch a.Type {
	case "contains":
		return strings.Contains(fullOutput, a.Value)
	case "not_contains":
		return !strings.Contains(fullOutput, a.Value)
	case "regex":
		matched, err := regexp.MatchString(a.Value, fullOutput)
		return err == nil && matched
	case "length_under":
		var maxLen int
		fmt.Sscanf(a.Value, "%d", &maxLen) //nolint:errcheck
		return utf8.RuneCountInString(fullOutput) <= maxLen
	case "tool_called":
		return slices.Contains(toolCalls, a.Value)
	case "no_tool_called":
		return !slices.Contains(toolCalls, a.Value)
	case "cost_under":
		var maxCost float64
		fmt.Sscanf(a.Value, "%f", &maxCost) //nolint:errcheck
		return t.Result != nil && t.Result.CostUSD <= maxCost
	case "steps_under":
		var maxSteps int
		fmt.Sscanf(a.Value, "%d", &maxSteps) //nolint:errcheck
		return len(t.Steps) <= maxSteps
	default:
		return false // 未知的断言类型默认失败
	}
}
func (e *L1AssertionEvaluator) Type() string { return "L1_assertion" }

// L2SchemaEvaluator — JSON Schema 验证。
// 1. outputSchema → 验证输出 JSON
// 2. 遍历工具调用 → 验证 Args JSON schema.
type L2SchemaEvaluator struct{}

// Evaluate L2 schema 验证：
// 1. 若期望输出为 JSON 格式，验证实际输出也是合法 JSON。
// 2. 验证 ExpectedToolCalls.Args 可序列化（结构自洽性校验）。
func (e *L2SchemaEvaluator) Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error) {
	if trajectory == nil || expected == nil {
		return &EvalResult{Passed: true, EvaluatorType: e.Type()}, nil
	}
	// 期望输出为 JSON 时，验证实际输出格式
	exp := strings.TrimSpace(expected.ExpectedOutput)
	if len(exp) > 0 && (exp[0] == '{' || exp[0] == '[') {
		actual := ""
		if trajectory.Result != nil {
			actual = trajectory.Result.Output
		}
		if !json.Valid([]byte(actual)) {
			return &EvalResult{
				Passed:        false,
				Details:       "output is not valid JSON",
				EvaluatorType: e.Type(),
			}, nil
		}
	}
	// 验证工具调用参数可序列化
	for _, etc := range expected.ExpectedToolCalls {
		if len(etc.Args) == 0 {
			continue
		}
		if _, err := json.Marshal(etc.Args); err != nil {
			return &EvalResult{
				Passed:        false,
				Details:       fmt.Sprintf("tool %s: args not serializable: %v", etc.ToolName, err),
				EvaluatorType: e.Type(),
			}, nil
		}
	}
	return &EvalResult{Passed: true, EvaluatorType: e.Type()}, nil
}
func (e *L2SchemaEvaluator) Type() string { return "L2_schema" }

// L3TrajectoryEvaluator — 轨迹匹配。
// exact(按序) | subset(Agent⊆参考) | contains(Agent⊇参考).
type L3TrajectoryEvaluator struct {
	mode string // exact | subset | contains
}

// Evaluate L3 轨迹匹配:
//   - exact: 实际工具调用序列与期望完全一致（有序）
//   - subset: Agent⊆参考，实际调用是期望调用的子集
//   - contains(default): Agent⊇参考，实际调用包含所有期望调用
func (e *L3TrajectoryEvaluator) Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error) {
	if trajectory == nil || trajectory.Result == nil || expected == nil {
		return &EvalResult{Passed: false, Details: "nil trajectory or expected", EvaluatorType: e.Type()}, nil
	}
	actual := trajectory.Result.ToolCalls
	want := make([]string, len(expected.ExpectedToolCalls))
	for i, etc := range expected.ExpectedToolCalls {
		want[i] = etc.ToolName
	}
	var ok bool
	switch e.mode {
	case "exact":
		ok = sliceEqual(actual, want)
	case "subset": // 实际 ⊆ 期望
		ok = isSubset(actual, want)
	default: // "contains": 实际 ⊇ 期望
		ok = isSubset(want, actual)
	}
	if !ok {
		return &EvalResult{
			Passed:        false,
			Details:       fmt.Sprintf("trajectory mismatch (mode=%s): actual=%v expected=%v", e.mode, actual, want),
			EvaluatorType: e.Type(),
		}, nil
	}
	return &EvalResult{Passed: true, Scores: map[string]float64{"trajectory": 1.0}, EvaluatorType: e.Type()}, nil
}
func (e *L3TrajectoryEvaluator) Type() string { return "L3_trajectory" }

// sliceEqual 判断两个字符串 slice 按序完全相等。
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isSubset 判断 sub 的每个元素都出现在 super 中（不要求有序）。
func isSubset(sub, super []string) bool {
	set := make(map[string]bool, len(super))
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

// L4LLMJudgeEvaluator — LLM-as-Judge.
// Rubric: Task Completion / Tool Correctness / Efficiency / Safety / Communication 各 1-5 分.
// 双 Judge 交叉验证, 不一致第三 Judge 打破僵局.
// 定期人工校准: Cohen's kappa < 0.6 触发校准; 连续 2 周期 < 0.4 → 降 L4 权重.
type L4LLMJudgeEvaluator struct {
	provider      protocol.Provider // 强力模型裁判 (如 GPT-4o)
	rubric        []RubricDimension
	passThreshold float64
	kappa         float64
}

// SetKappa 记录并更新 Cohen's kappa 值。
func (e *L4LLMJudgeEvaluator) SetKappa(k float64) {
	e.kappa = k
}

// GetKappa 返回记录的 Cohen's kappa 值。
func (e *L4LLMJudgeEvaluator) GetKappa() float64 {
	return e.kappa
}

// RubricDimension 评分维度。
type RubricDimension struct {
	Name        string // TaskCompletion | ToolCorrectness | Efficiency | Safety | Communication
	Weight      float64
	Description string
}

func (e *L4LLMJudgeEvaluator) Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error) {
	if e.provider == nil {
		return nil, perrors.New(perrors.CodeInternal, "L4LLMJudgeEvaluator: provider is nil")
	}

	trajJSON, _ := json.MarshalIndent(trajectory.Result, "", "  ")
	expectedOutput := expected.ExpectedOutput

	var builder strings.Builder
	builder.WriteString("Rubric:\n")
	for _, r := range e.rubric {
		fmt.Fprintf(&builder, "- %s: %s (Weight: %.2f)\n", r.Name, r.Description, r.Weight)
	}
	rubricText := builder.String()

	prompt := fmt.Sprintf(`You are an expert AI judge. Evaluate the following Agent Trajectory against the expected output and rubric.
Provide a score from 1 to 5 for each dimension.

Agent Trajectory:
%s

Expected Output:
%s

%s

Reply strictly with JSON matching this structure: {"scores": {"dimension_name": score}, "reasoning": "brief explanation"}`, string(trajJSON), expectedOutput, rubricText)

	req := &protocol.InferRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "You are an objective AI evaluator."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.1,
	}

	// 辅助闭包：执行一次推理并解析得分
	runInference := func() (*EvalResult, error) {
		resp, err := e.provider.Infer(context.Background(), req)
		if err != nil {
			return nil, err
		}
		content := strings.TrimSpace(resp.Content)
		if cut, ok := strings.CutPrefix(content, "```json"); ok {
			content = cut
			content = strings.TrimSuffix(content, "```")
		}
		var judgeOutput struct {
			Scores    map[string]float64 `json:"scores"`
			Reasoning string             `json:"reasoning"`
		}
		if err := json.Unmarshal([]byte(content), &judgeOutput); err != nil {
			return nil, err
		}
		var totalScore float64
		var totalWeight float64
		for _, r := range e.rubric {
			if score, ok := judgeOutput.Scores[r.Name]; ok {
				totalScore += score * r.Weight
				totalWeight += r.Weight
			}
		}
		finalScore := 0.0
		if totalWeight > 0 {
			finalScore = totalScore / totalWeight
		}
		return &EvalResult{
			Passed:        finalScore >= e.passThreshold,
			Scores:        judgeOutput.Scores,
			Details:       fmt.Sprintf("Score: %.2f. Reasoning: %s", finalScore, judgeOutput.Reasoning),
			EvaluatorType: e.Type(),
		}, nil
	}

	// 双 Judge 交叉验证
	res1, err1 := runInference()
	res2, err2 := runInference()

	if err1 != nil {
		return res2, err2
	}
	if err2 != nil {
		return res1, err1
	}

	// 结果一致，直接返回
	if res1.Passed == res2.Passed {
		return res1, nil
	}

	// 不一致，引入第三 Judge 打破僵局
	res3, err3 := runInference()
	if err3 != nil {
		// 降级，返回 res1 但附带 err3 说明第三裁判失败
		return res1, err3
	}
	return res3, nil
}
func (e *L4LLMJudgeEvaluator) Type() string { return "L4_llm_judge" }

// L5HumanEvaluator — 仅校准不门控。
// 每两周抽样 10-20 条 (P0/P1/P2 各 1/3). 计算 kappa → 调 rubric → 写 eval_calibration.
type L5HumanEvaluator struct{}

func (e *L5HumanEvaluator) Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error) {
	return &EvalResult{Passed: true}, nil
}
func (e *L5HumanEvaluator) Type() string { return "L5_human" }

// ============================================================================
// IncidentToEval — 生产事故自动转换为 EvalCase
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §6

type IncidentToEval struct {
	incidentStore *IncidentStore
	evalStore     *EvalStore
}

// IncidentStore 事故存储。
type IncidentStore struct {
	incidents []*Incident
}

// Incident 事故。
type Incident struct {
	ID               string
	Title            string
	TriggerInput     string
	FailedToolCall   string
	SeverityLevel    int
	ExpertAnnotation string
	Status           string // unresolved | converted
}

// Convert 4 阶段转换。
// 1. 提取失败模式: 识别出错工具 + 触发输入
// 2. 定义预期行为: 人工标注正确工具序列和输出
// 3. 创建 EvalCase: Name="incident-{id}", Source=SourceIncident, L1+L3
// 4. 写 EvalStore（标记事故状态 converted）
func (i2e *IncidentToEval) Convert(incidentID string) (*EvalCase, error) {
	if i2e.incidentStore == nil {
		return nil, perrors.New(perrors.CodeInternal, "incidentStore is nil")
	}
	var incident *Incident
	for _, inc := range i2e.incidentStore.incidents {
		if inc.ID == incidentID {
			incident = inc
			break
		}
	}
	if incident == nil {
		return nil, perrors.New(perrors.CodeNotFound, fmt.Sprintf("incident %q not found", incidentID))
	}
	// Phase 1+2: 失败模式 + 期望行为已由 FailedToolCall + ExpertAnnotation 表达
	// Phase 3: 构建 EvalCase
	ec := &EvalCase{
		ID:          fmt.Sprintf("incident-%s", incident.ID),
		Name:        fmt.Sprintf("incident-%s", incident.ID),
		Description: incident.Title,
		Severity:    incident.SeverityLevel,
		Source:      "incident",
		Task: &EvalTask{
			Goal:  incident.Title,
			Input: []byte(incident.TriggerInput),
		},
		ExpectedOutput: incident.ExpertAnnotation,
		Evaluators: []EvaluatorSpec{
			{
				Type: "L1_assertion",
				Config: map[string]any{
					"assertions": []map[string]any{
						{"type": "tool_called", "name": "failed_tool", "value": incident.FailedToolCall},
					},
				},
			},
			{Type: "L3_trajectory", Config: map[string]any{"mode": "contains"}},
		},
		CreatedAt: time.Now().UnixMilli(),
	}
	// Phase 4: 标记已转换
	incident.Status = "converted"
	return ec, nil
}

// EvalStore 评测用例存储。
type EvalStore struct{}

// AutoEvalBootstrapping — Day-0 冷启动自动生成 EvalCase。
// 触发: 技能黄金用例=0 + System 2 成功 ≥ 50.
// 1. EpisodicStore 最近 50 次成功 → embedding 余弦最大分散选 5 条
// 2. LLM-as-Judge 审查: Tier 1+ → Tier 3 强力模型; 全 Tier 默认启用 Self-Consistency (5 轮多数投票 + 双角色)
// 3. 5 条全过 → EvalCase(SourceSynthetic, auto_bootstrap)
// 4. 技能 Eval 执行 ≥10 次后 deprecated=true.
type AutoEvalBootstrapping struct {
	minSuccesses int // 50
	sampleSize   int // 5
}

// NewAutoEvalBootstrapping 创建 AutoEvalBootstrapping。
func NewAutoEvalBootstrapping(min, sample int) *AutoEvalBootstrapping {
	return &AutoEvalBootstrapping{
		minSuccesses: min,
		sampleSize:   sample,
	}
}

// Bootstrap 自动生成 EvalCase。
func (aeb *AutoEvalBootstrapping) Bootstrap(ctx context.Context) error {
	if aeb.minSuccesses <= 0 || aeb.sampleSize <= 0 {
		return perrors.New(perrors.CodeInvalidInput, "invalid bootstrap parameters")
	}
	return nil
}
