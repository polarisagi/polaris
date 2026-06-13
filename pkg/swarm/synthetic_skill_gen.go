package swarm

import (
	"context"

	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
)

// SyntheticSkillGen 提供 M6 级别的逻辑坍缩，将任务轨迹生成 Wasm Skill。
type SyntheticSkillGen struct {
	provider protocol.Provider
}

func NewSyntheticSkillGen(provider protocol.Provider) *SyntheticSkillGen {
	return &SyntheticSkillGen{provider: provider}
}

func (g *SyntheticSkillGen) Generate(ctx context.Context, name, description string) (protocol.Tool, error) {
	if g.provider == nil {
		return protocol.Tool{}, fmt.Errorf("provider is required for synthesis")
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

	req := &protocol.InferRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "You are a helpful coding assistant that outputs strictly valid JSON without markdown wrapping."},
			{Role: "user", Content: prompt},
		},
	}

	resp, err := g.provider.Infer(ctx, req.Messages, protocol.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return protocol.Tool{}, fmt.Errorf("llm infer failed: %w", err)
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
		return protocol.Tool{}, fmt.Errorf("failed to parse synthesized JSON: %w", err)
	}

	return protocol.Tool{
		Name:        schema.Name,
		Description: schema.Description,
		Version:     schema.Version,
		Capability:  protocol.CapReadOnly,
		SideEffects: []protocol.SideEffect{protocol.SideNone},
		RiskLevel:   protocol.RiskLow,
		SandboxTier: protocol.SandboxInProcess,
		Source:      protocol.ToolLLMGenerated,
		InputSchema: schema.InputSchema,
	}, nil
}
