package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ProcessRewardModel 在 S_PLAN 阶段对 N 个候选 DAG 方案并发打分，返回最优方案。
// 调用时机：Planner 产出多个候选方案后，Kernel 调用 SelectBest 选优。
// N<=1 或 Enabled=false 时直接返回 candidates[0]，零额外 LLM 开销。
type ProcessRewardModel interface {
	SelectBest(ctx context.Context, goal string, candidates []*types.DAGModel) (*types.DAGModel, error)
}

// PRMConfig 过程奖励模型配置。
type PRMConfig struct {
	Enabled        bool
	ScorerModel    string  // budget 层模型，e.g. "deepseek-chat"
	MinThreshold   float64 // 低于此分数则回退 candidates[0]，e.g. 0.4
	MaxCandidates  int     // 候选上限，超出截断；建议 3
	ComplexityGate float64 // TaskModel.Complexity 低于此值跳过 PRM，e.g. 0.5
}

// DefaultPRM 通过 Provider 调用 budget 层大模型对 DAG 候选方案打分。
type DefaultPRM struct {
	config   PRMConfig
	provider protocol.Provider
}

func NewDefaultPRM(cfg PRMConfig, provider protocol.Provider) *DefaultPRM {
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 3
	}
	if cfg.MinThreshold <= 0 {
		cfg.MinThreshold = 0.4
	}
	return &DefaultPRM{config: cfg, provider: provider}
}

// SelectBest 对候选方案并发打分，返回得分最高且超过 MinThreshold 的方案。
// complexity 来自 TaskModel.Complexity，低于 ComplexityGate 时跳过打分直接返回第一个候选。
func (p *DefaultPRM) SelectBest(ctx context.Context, goal string, complexity float64, candidates []*types.DAGModel) (*types.DAGModel, error) {
	if len(candidates) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "prm: no candidates provided")
	}
	// 简单任务、单候选、或未启用——零开销返回
	if !p.config.Enabled || len(candidates) == 1 || complexity < p.config.ComplexityGate {
		return candidates[0], nil
	}
	if len(candidates) > p.config.MaxCandidates {
		candidates = candidates[:p.config.MaxCandidates]
	}

	type result struct {
		idx   int
		score float64
	}
	ch := make(chan result, len(candidates))

	for i, c := range candidates {
		//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
		go func(i int, c *types.DAGModel) {
			score, err := p.scoreCandidate(ctx, goal, c)
			if err != nil {
				score = 0 // 打分失败降为最低，不阻断其他候选
			}
			ch <- result{idx: i, score: score}
		}(i, c)
	}

	best := candidates[0]
	bestScore := -1.0
	for range candidates {
		r := <-ch
		if r.score > bestScore {
			bestScore = r.score
			best = candidates[r.idx]
		}
	}

	// 所有候选均低于阈值时兜底返回第一个，避免 Planner 陷入死循环
	if bestScore < p.config.MinThreshold {
		return candidates[0], nil
	}
	return best, nil
}

// scoringOutput 是打分模型的结构化输出。
type scoringOutput struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

func (p *DefaultPRM) scoreCandidate(ctx context.Context, goal string, plan *types.DAGModel) (float64, error) {
	prompt := fmt.Sprintf(
		"你是执行方案评估器。根据任务目标和执行方案，评估方案质量。\n\n"+
			"任务目标：%s\n\n"+
			"执行方案：\n%s\n\n"+
			"评估标准（按权重）：\n"+
			"1. 步骤能否达成目标（权重最高）\n"+
			"2. 步骤数量精简程度（越少越好）\n"+
			"3. 步骤顺序合理性\n"+
			"4. 是否存在冗余步骤\n\n"+
			"以 JSON 输出，score 为 0.0~1.0：",
		goal, planToText(plan),
	)

	req := &types.InferRequest{
		Model:       p.config.ScorerModel,
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   128,
		Temperature: 0, // 打分需要确定性
		ResponseFormat: &types.ResponseFormat{
			Type: "json_schema",
			JSONSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"score":  map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"reason": map[string]any{"type": "string"},
				},
				"required": []string{"score", "reason"},
			},
		},
	}

	//custom-nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := p.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("prm: infer failed: %v", err), err)
	}

	var out scoringOutput
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("prm: parse score: %v", err), err)
	}
	if out.Score < 0 || out.Score > 1 {
		return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("prm: score out of range: %f", out.Score))
	}
	return out.Score, nil
}

// MaxCandidates 返回候选数量上限，供 Kernel 决定并发 Infer 次数。
func (p *DefaultPRM) MaxCandidates() int { return p.config.MaxCandidates }

// ShouldActivate 根据任务复杂度判断是否需要启动 PRM。
// complexity 来自 TaskModel.Complexity（0~1），低于 ComplexityGate 的简单任务跳过。
func (p *DefaultPRM) ShouldActivate(complexity float64) bool {
	return p.config.Enabled && complexity >= p.config.ComplexityGate
}

// planToText 将 DAGModel 序列化为可读文本，供打分 prompt 使用。
func planToText(plan *types.DAGModel) string {
	if plan == nil || len(plan.Nodes) == 0 {
		return "(空方案)"
	}
	var b strings.Builder
	for i, n := range plan.Nodes {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, n.ID, n.Action)
	}
	return b.String()
}
