package skill

import (
	"context"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ProviderLLMClient 把 protocol.Provider 适配成 SkillCreator 需要的 LLMClient
// 接口（2026-07-21 deadcode 审查补齐：SkillCreator 此前功能完整但从未接线，
// 缺的正是这一层"用哪个 Provider 生成"的适配器）。调用方式与
// internal/learning/synthetic/synthetic_skill_gen.go 的既有生产用法一致
// （safecall.Infer 包一层 system+user 两条消息，取 resp.Content）。
type ProviderLLMClient struct {
	Provider protocol.Provider
}

var _ LLMClient = (*ProviderLLMClient)(nil)

// Generate 实现 LLMClient。Provider 为 nil 时返回明确错误（fail-closed），
// 不静默退化。
func (a *ProviderLLMClient) Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if a.Provider == nil {
		return "", apperr.New(apperr.CodeInternal, "skill_creator: no LLM provider available to generate skill")
	}
	resp, err := safecall.Infer(ctx, a.Provider, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: LLM infer failed", err)
	}
	return resp.Content, nil
}

// GenerateJSON 实现 LLMClient（阶段03 R-06）。语义同 Generate，但附带
// ResponseFormat=json_object，让支持结构化输出的 Provider（走
// internal/llm/adapter 的 OpenAICompatibleClient 一族：openai/deepseek/
// ollama）尽力强制返回合法 JSON。Anthropic/Google 当前不读取该字段（透传
// 无副作用），由上层 StructuredGenerator 的 extractJSON 兜底 + 有界重试
// 兜底覆盖，不需要在此处按 Provider 能力位分支。
func (a *ProviderLLMClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if a.Provider == nil {
		return "", apperr.New(apperr.CodeInternal, "skill_creator: no LLM provider available to generate skill")
	}
	resp, err := safecall.Infer(ctx, a.Provider, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, types.WithResponseFormat(&types.ResponseFormat{Type: "json_object"}))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: LLM infer failed", err)
	}
	return resp.Content, nil
}
