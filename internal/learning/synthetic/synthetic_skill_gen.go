package synthetic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SyntheticSkillGen 实现 M6 Logic-Collapse：
// 任务轨迹 → LLM 蒸馏 → SkillMeta → SkillRegistry 持久化。
// 架构文档: docs/arch/M06-Skill-Library.md §6.3
type SyntheticSkillGen struct {
	provider protocol.Provider
	skillReg protocol.SkillRegistry // 写入目标（State-in-DB，HE-6）；nil 时降级仅返回 Tool
}

// NewSyntheticSkillGen 构造生成器。
// skillReg 为 nil 时技能仍生成但不持久化（Tier-0 降级场景）。
func NewSyntheticSkillGen(provider protocol.Provider, skillReg protocol.SkillRegistry) *SyntheticSkillGen {
	return &SyntheticSkillGen{provider: provider, skillReg: skillReg}
}

// Generate 调用 LLM 生成技能 Schema，并将结果注册到 SkillRegistry。
// 返回的 types.Tool 供即时工具调用使用；持久化通过 skillReg 完成。
func (g *SyntheticSkillGen) Generate(ctx context.Context, name, description string) (types.Tool, error) {
	if g.provider == nil {
		return types.Tool{}, apperr.New(apperr.CodeInternal, "provider is required for synthesis")
	}

	// Step 1: LLM 生成 JSON Schema
	schema, tool, err := g.generateSchema(ctx, name, description)
	if err != nil {
		return types.Tool{}, err
	}

	// Step 2: 持久化到 SkillRegistry（HE-6 State-in-DB）
	if g.skillReg != nil {
		if regErr := g.registerSkill(ctx, name, description, schema); regErr != nil {
			// 重名视为幂等（已注册过），其余错误仅记录不中断
			if !apperr.IsCode(regErr, apperr.CodeAlreadyExists) {
				slog.Warn("synthetic_skill_gen: register failed", "err", regErr)
			}
		}
	}

	return tool, nil
}

func (g *SyntheticSkillGen) generateSchema(ctx context.Context, name, description string) (map[string]any, types.Tool, error) {
	prompt := fmt.Sprintf(`You are an AI generating a tool schema.
Generate a strictly valid JSON object for a tool named "%s".
Description: "%s"
The JSON object must have the following keys:
- name (string)
- description (string)
- version (string, e.g., "1.0.0")
- input_schema (object with JSON Schema for parameters)
- instructions (string, concise Go/pseudocode showing what this skill should do)

Output ONLY valid JSON. No markdown formatting or extra text.`, name, description)

	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "system", Content: "You are a helpful coding assistant that outputs strictly valid JSON without markdown wrapping."},
			{Role: "user", Content: prompt},
		},
	}

	resp, err := safecall.Infer(ctx, g.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, types.Tool{}, apperr.Wrap(apperr.CodeInternal, "llm infer failed", err)
	}

	content := strings.TrimSpace(resp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var raw struct {
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		Version      string         `json:"version"`
		InputSchema  map[string]any `json:"input_schema"`
		Instructions string         `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, types.Tool{}, apperr.Wrap(apperr.CodeInternal, "failed to parse synthesized JSON", err)
	}

	if raw.Version == "" {
		raw.Version = "1.0.0"
	}

	tool := types.Tool{
		Name:        raw.Name,
		Description: raw.Description,
		Version:     raw.Version,
		Capability:  types.CapReadOnly,
		SideEffects: []types.SideEffect{types.SideNone},
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		Source:      types.ToolLLMGenerated,
		InputSchema: raw.InputSchema,
	}
	return raw.InputSchema, tool, nil
}

// registerSkill 将生成结果写入 SkillRegistry。
// 技能名格式：skill:{name}（SkillRegistry 强制要求此前缀）。
func (g *SyntheticSkillGen) registerSkill(ctx context.Context, name, description string, inputSchema map[string]any) error {
	skillName := "skill:" + name
	if !strings.HasPrefix(name, "skill:") {
		skillName = "skill:" + name
	}

	schemaBytes, _ := json.Marshal(inputSchema)

	meta := types.SkillMeta{
		Name:         skillName,
		Version:      "1.0.0",
		Runtime:      "script",
		RiskLevel:    "low",
		Sandbox:      1,
		Capabilities: []string{"read_only"}, // SkillMeta.Capabilities 存字符串标签，非整数枚举
		ExecMode:     "tool",
		Trust:        types.TrustLocal, // 合成技能视为本地可信（用实例密钥）
		Instructions: fmt.Sprintf("Synthetic skill: %s\nInput schema: %s", description, string(schemaBytes)),
		Deprecated:   false,
	}

	return g.skillReg.Register(ctx, meta)
}
