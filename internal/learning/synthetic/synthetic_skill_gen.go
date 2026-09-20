package synthetic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SyntheticSkillGen 实现 M6 Logic-Collapse：
// 任务轨迹 → LLM 蒸馏 → SkillMeta → SkillRegistry 持久化。
// 架构文档: docs/arch/M06-Skill-Library.md §2.2
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

	// Step 2: 以"待审候选"持久化到 SkillRegistry（HE-6 State-in-DB）
	if g.skillReg != nil {
		if regErr := g.registerSkill(ctx, tool.Name, description, schema); regErr != nil {
			// 重名视为幂等（已注册过），其余错误仅记录不中断
			if !apperr.IsCode(regErr, apperr.CodeAlreadyExists) {
				slog.Warn("synthetic_skill_gen: register failed", "err", regErr)
				metrics.GlobalLearningSkillRegisterFailuresTotal.Add(1)
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

	// LLM 输出必须过必填字段校验再驱动逻辑（09-LLM-Agent-Production H 维度）：
	// json.Unmarshal 只保证"是合法 JSON"，不保证字段存在。缺 name 会注册出一个
	// 名为 "skill:" 的技能，缺 input_schema 会让 S_VALIDATE 的 hasStrictSchema
	// 永远返回 false（M11 §2.5 降级前置条件），两者都是在下游很远的地方才炸。
	// fail-fast 在这里，错误信息才指得回"是这次合成的产物有问题"。
	if strings.TrimSpace(raw.Name) == "" {
		return nil, types.Tool{}, apperr.New(apperr.CodeInvalidInput, "synthesized skill: 缺少必填字段 name")
	}
	if strings.TrimSpace(raw.Description) == "" {
		return nil, types.Tool{}, apperr.New(apperr.CodeInvalidInput, "synthesized skill: 缺少必填字段 description")
	}
	if len(raw.InputSchema) == 0 {
		return nil, types.Tool{}, apperr.New(apperr.CodeInvalidInput, "synthesized skill: 缺少必填字段 input_schema")
	}
	if raw.Version == "" {
		raw.Version = "1.0.0"
	}

	// 工具名取调用方请求的名字而非 LLM 自拟的 raw.Name：LLM 输出不可信，若它
	// 返回 "read_file" 之类的内置工具名，下游按名注册会覆盖真实工具（GR-7.1-004）。
	tool := types.Tool{
		Name:        name,
		Description: raw.Description,
		Version:     raw.Version,
		Capability:  types.CapReadOnly,
		SideEffects: []types.SideEffect{types.SideNone},
		RiskLevel:   types.RiskLow,
		// 合成产物只有 schema 没有实现，也未经审查：标最低信任，路由时由
		// AssignSandboxTier 按 Source/TrustTier 决定隔离级别，不在此处硬编码进程内。
		TrustTier:   types.TrustUntrusted,
		Source:      types.ToolLLMGenerated,
		InputSchema: raw.InputSchema,
	}
	return raw.InputSchema, tool, nil
}

// registerSkill 将生成结果以待审候选写入 SkillRegistry。
// 技能名格式：skill:{name}（SkillRegistry 强制要求此前缀）。
//
// GR-7.1-004 / learning CLAUDE.md [MUST NOT]"未经 M11 安全审查的 Logic Collapse
// 输出不得部署为活跃技能"：
//   - Deprecated=true：候选不进入 List/SkillSelector/SkillExecutor 的活跃集合，
//     经审查后由人工/审查流程重新 Register 为非 deprecated 版本才生效；
//   - 同名技能已存在时不写：Register 是 upsert，LLM 可控的名字若与用户已安装
//     的技能同名，会把受信技能覆盖成未审查的合成内容。
func (g *SyntheticSkillGen) registerSkill(ctx context.Context, name, description string, inputSchema map[string]any) error {
	skillName := name
	if !strings.HasPrefix(name, "skill:") {
		skillName = "skill:" + name
	}
	if existing, err := g.skillReg.Get(ctx, skillName, ""); err == nil && existing != nil {
		return apperr.New(apperr.CodeAlreadyExists, "synthetic_skill_gen: skill already exists, not overwriting")
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
		Trust:        types.TrustLocal, // Registry 拒收 < TrustLocal；是否可用由 Deprecated 候选态控制
		Instructions: fmt.Sprintf("Synthetic skill (pending review): %s\nInput schema: %s", description, string(schemaBytes)),
		Deprecated:   true, // 待审候选，不进入活跃集合
	}

	if err := g.skillReg.Register(ctx, meta); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "synthetic_skill_gen: SkillRegistry.Register 失败", err)
	}
	return nil
}
