package sleeptime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// PathValidator 校验待写入路径合法性接口。
type PathValidator interface {
	ValidatePath(relPath string) error
}

// RouterInferCaller 调用 LLM 推理接口。
type RouterInferCaller interface {
	Infer(ctx context.Context, req types.InferRequest, opts types.InferOptions) (types.InferResponse, error)
}

// DefaultProposer 实现 LLMProposer。
type DefaultProposer struct {
	caller    RouterInferCaller
	validator PathValidator
}

// NewDefaultProposer 创建 DefaultProposer 实例。
func NewDefaultProposer(caller RouterInferCaller, validator PathValidator) *DefaultProposer {
	return &DefaultProposer{
		caller:    caller,
		validator: validator,
	}
}

// Propose 根据失败模式列表，使用便宜模型（default 池、关闭思考）提出 1~3 个最小改动。
func (p *DefaultProposer) Propose(ctx context.Context, weaknesses []Weakness) ([]ProposedChange, error) {
	if len(weaknesses) == 0 {
		return nil, nil
	}
	if p.caller == nil {
		return nil, apperr.New(apperr.CodeInternal, "sleeptime.Propose: nil caller")
	}

	prompt := buildProposalPrompt(weaknesses)

	req := types.InferRequest{
		Messages: []types.Message{
			{Role: "system", Content: "You are the Polaris Self-Improvement Engine. You analyze agent weaknesses and propose minimal, high-impact improvements to the harness overlay."},
			{Role: "user", Content: prompt},
		},
	}
	// T8: 便宜模型 + 关闭思考（ADR-0105 决策二）
	opts := types.InferOptions{
		Purpose:      "self_harness_propose",
		ModelPool:    "default",
		ThinkingMode: types.ThinkingDisabled,
	}

	//llmopts:exempt caller 接口接收的是 types.InferOptions 结构体且已显式声明 Purpose 与 ThinkingMode
	resp, err := p.caller.Infer(ctx, req, opts)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.Propose: infer failed", err)
	}

	raw := strings.TrimSpace(resp.Content)
	raw = cleanJSONFence(raw)

	var proposed []ProposedChange
	if err := json.Unmarshal([]byte(raw), &proposed); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.Propose: unmarshal proposals", err)
	}

	if len(proposed) == 0 {
		return nil, nil
	}
	if len(proposed) > 3 {
		proposed = proposed[:3]
	}

	valid := make([]ProposedChange, 0, len(proposed))
	for _, ch := range proposed {
		ch.Path = strings.TrimSpace(ch.Path)
		ch.Content = strings.TrimSpace(ch.Content)
		if ch.Path == "" || ch.Content == "" {
			continue
		}
		if p.validator != nil {
			if err := p.validator.ValidatePath(ch.Path); err != nil {
				continue
			}
		}
		valid = append(valid, ch)
	}

	return valid, nil
}

func buildProposalPrompt(weaknesses []Weakness) string {
	var sb strings.Builder
	sb.WriteString("Observed recurring failure patterns in recent agent executions:\n")
	for i, w := range weaknesses {
		sb.WriteString(fmt.Sprintf("%d. [%s] (occurrences: %d)\n", i+1, w.Signature, w.Count))
		if len(w.SampleErrors) > 0 {
			sb.WriteString(fmt.Sprintf("   Samples: %s\n", strings.Join(w.SampleErrors, " | ")))
		}
	}
	sb.WriteString("\nPropose 1 to 3 minimal, targeted additions or edits to the harness overlay to mitigate these weaknesses.\n")
	sb.WriteString("The ONLY permitted paths are:\n")
	sb.WriteString("- prompt_addenda/<name>.md: Add clear guidelines or rules to system prompt\n")
	sb.WriteString("- tool_notes/<tool_name>.md: Add operational hints, flags, or precautions for a tool\n")
	sb.WriteString("- skills/<name>.py: Add a self-contained helper skill script\n")
	sb.WriteString("\nOutput MUST be a valid JSON array of objects with \"path\", \"content\", and \"reason\" fields. No markdown fences around JSON.")
	return sb.String()
}

func cleanJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}
