package consolidation

import (
	"bytes"
	"context"
	"strings"
	"text/template"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/memory"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// DefaultSummarizer 实现了 memory.LLMSummarizer，用于将会话事件压缩为摘要。
type DefaultSummarizer struct {
	provider  protocol.Provider
	promptMgr *prompt.Manager
}

func NewDefaultSummarizer(provider protocol.Provider, promptMgr *prompt.Manager) memory.LLMSummarizer {
	return &DefaultSummarizer{
		provider:  provider,
		promptMgr: promptMgr,
	}
}

func (s *DefaultSummarizer) Summarize(ctx context.Context, text string, maxTokens int) (string, error) {
	if s.provider == nil {
		return "", nil
	}

	p := s.promptMgr.ReadPrompt("session_summary.tmpl", "")
	if p == "" {
		p = "Summarize the following AI agent session in 3-5 concise sentences. Focus on: what was accomplished, what tools were used, and key outcomes.\n\n{{.Text}}"
	}

	tmpl, err := template.New("session_summary").Parse(p)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to parse summary template", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Text": text}); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to execute summary template", err)
	}

	resp, err := safecall.Infer(ctx, s.provider, []types.Message{{Role: "user", Content: buf.String()}}, types.WithMaxTokens(maxTokens))
	if err == nil && resp != nil {
		return strings.TrimSpace(resp.Content), nil
	}
	return "", apperr.Wrap(apperr.CodeInternal, "failed to infer session summary", err)
}

// InferRaw 透传调用方已渲染好的 prompt，返回 LLM 原始文本响应（去首尾空白）。
// 供实体抽取 / 用户画像合成等自定义 prompt 结构场景使用，见 memory.LLMSummarizer 注释。
func (s *DefaultSummarizer) InferRaw(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if s.provider == nil {
		return "", nil
	}
	resp, err := safecall.Infer(ctx, s.provider, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(maxTokens))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to infer", err)
	}
	if resp == nil {
		return "", apperr.New(apperr.CodeInternal, "nil response from provider")
	}
	return strings.TrimSpace(resp.Content), nil
}
