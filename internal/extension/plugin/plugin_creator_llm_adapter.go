package plugin

import (
	"context"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ProviderLLMClient 把 protocol.Provider 适配成 PluginCreator 需要的 LLMClient
// 接口（2026-08-02 阶段03 R-06 死代码接线补齐：PluginCreator 本身功能完整
// [生成/落盘/注册为 MCP Server]，此前只是缺"用哪个 Provider 生成"这层适配器，
// 导致 Server.SetPluginCreator 从未在生产环境被调用）。调用方式与
// internal/extension/skill/skill_creator_llm_adapter.go 的 ProviderLLMClient
// 完全一致（safecall.Infer 包一层 system+user 两条消息，取 resp.Content），
// 仅错误消息前缀从 skill_creator 改为 plugin_creator。
type ProviderLLMClient struct {
	Provider protocol.Provider
}

var _ LLMClient = (*ProviderLLMClient)(nil)

// Generate 实现 LLMClient。Provider 为 nil 时返回明确错误（fail-closed），
// 不静默退化。
func (a *ProviderLLMClient) Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if a.Provider == nil {
		return "", apperr.New(apperr.CodeInternal, "plugin_creator: no LLM provider available to generate plugin")
	}
	resp, err := safecall.Infer(ctx, a.Provider, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "plugin_creator: LLM infer failed", err)
	}
	return resp.Content, nil
}

// GenerateJSON 实现 LLMClient。语义同 Generate，但附带
// ResponseFormat=json_object，尽力让 Provider 侧强制返回合法 JSON（阶段03 R-06，
// StructuredGenerator 的有界重试+熔断兜底仍然生效）。
func (a *ProviderLLMClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if a.Provider == nil {
		return "", apperr.New(apperr.CodeInternal, "plugin_creator: no LLM provider available to generate plugin")
	}
	resp, err := safecall.Infer(ctx, a.Provider, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, types.WithResponseFormat(&types.ResponseFormat{Type: "json_object"}))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "plugin_creator: LLM infer failed", err)
	}
	return resp.Content, nil
}
