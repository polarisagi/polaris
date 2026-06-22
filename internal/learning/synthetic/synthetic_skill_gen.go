package synthetic

import (
	"context"

	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SyntheticSkillGen 提供 M6 级别的逻辑坍缩，将任务轨迹生成 Wasm Skill。
type SyntheticSkillGen struct {
	provider protocol.Provider
}

func NewSyntheticSkillGen(provider protocol.Provider) *SyntheticSkillGen {
	return &SyntheticSkillGen{provider: provider}
}

func (g *SyntheticSkillGen) Generate(ctx context.Context, name, description string) (types.Tool, error) {
	if g.provider == nil {
		return types.Tool{}, apperr.New(apperr.CodeInternal, "provider is required for synthesis")
	}

	prompt := fmt.Sprintf(`You are an AI generating a tool schema.
Generate a strictly valid JSON object for a tool named "%s".
Description: "%s"
The JSON object must have the following keys:
- name (string)
- description (string)
- version (string, e.g., "1.0.0")
- input_schema (object with JSON Schema for parameters)

Output ONLY valid JSON. No markdown formatting or extra text.`, name, description)

	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "system", Content: "You are a helpful coding assistant that outputs strictly valid JSON without markdown wrapping."},
			{Role: "user", Content: prompt},
		},
	}

	resp, err := g.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal, "llm infer failed", err)
	}

	content := strings.TrimSpace(resp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var schema struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Version     string         `json:"version"`
		InputSchema map[string]any `json:"input_schema"`
	}

	if err := json.Unmarshal([]byte(content), &schema); err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal, "failed to parse synthesized JSON", err)
	}

	return types.Tool{
		Name:        schema.Name,
		Description: schema.Description,
		Version:     schema.Version,
		Capability:  types.CapReadOnly,
		SideEffects: []types.SideEffect{types.SideNone},
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		Source:      types.ToolLLMGenerated,
		InputSchema: schema.InputSchema,
	}, nil
}
