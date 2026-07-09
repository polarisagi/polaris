package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// splitMessages/calcSummaryBudget/summarize/buildTranscript/persistCompacted/
// injectTaskCanvas/offloadLargeToolResults（R7 拆分自 compressor.go；
// Compressor 核心结构与 Compact/ForceCompact/compact 编排逻辑见 compressor.go）。

// splitMessages 从尾部向前积累，返回 (middle, tail)。
// tail 保留约 tailTokens 个 token 的原始消息；middle 为其余部分。
func splitMessages(msgs []apptypes.Message, tailTokens int) (middle, tail []apptypes.Message) {
	tailBudget := tailTokens * charsPerToken
	splitIdx := len(msgs)
	cumChars := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		cumChars += len(msgs[i].Content)
		if cumChars > tailBudget {
			break
		}
		splitIdx = i
	}
	if splitIdx <= 0 {
		return nil, msgs
	}
	return msgs[:splitIdx], msgs[splitIdx:]
}

// calcSummaryBudget 根据被压缩内容长度计算 LLM 摘要 token 预算。
func calcSummaryBudget(middle []apptypes.Message) int {
	middleChars := 0
	for _, m := range middle {
		middleChars += len(m.Content)
	}
	budget := int(float64(middleChars/charsPerToken) * summaryRatio)
	budget = max(budget, minSummaryTokens)
	budget = min(budget, maxSummaryTokens)
	return budget
}

// summarize 调用 provider 对 middle 消息生成结构化摘要。
func (c *Compressor) summarize(ctx context.Context, msgs []apptypes.Message, maxTokens int, provider protocol.Provider) (string, error) {
	transcript := buildTranscript(msgs)
	inferReq := &apptypes.InferRequest{
		Messages: []apptypes.Message{
			{Role: "system", Content: compactSummarizePrompt},
			{Role: "user", Content: "请为以下对话生成摘要：\n\n" + transcript},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.3,
	}

	//custom-nolint:bare-infer // 历史代码暂留，后续重构替换
	ch, err := provider.StreamInfer(ctx, inferReq.Messages)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Compressor.summarize", err)
	}

	var result strings.Builder
	for ev := range ch {
		switch ev.Type {
		case apptypes.StreamTextDelta:
			if ev.Content != "" {
				result.WriteString(ev.Content)
			}
		case apptypes.StreamError:
			if ev.Content != "" {
				return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("summarize stream: %s", ev.Content))
			}
		}
	}
	return strings.TrimSpace(result.String()), nil
}

// buildTranscript 拼接消息序列为文本摘要输入，单条消息截断防超限。
func buildTranscript(msgs []apptypes.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString("[")
		sb.WriteString(m.Role)
		sb.WriteString("]: ")
		content := m.Content
		if len(content) > 8000 {
			content = content[:8000] + "…(truncated)"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	transcript := sb.String()
	if len(transcript) > 120000 {
		transcript = transcript[:120000]
	}
	return transcript
}

// persistCompacted 原子替换 chat_messages：删除旧消息，写入摘要 + tail。
// 在事务内完成，保证 SQLite 单连接安全。
func (c *Compressor) persistCompacted(ctx context.Context, sessionID string, summary apptypes.Message, tail []apptypes.Message) error {
	msgs := make([]apptypes.ChatMessageRow, 0, len(tail)+1)
	msgs = append(msgs, apptypes.ChatMessageRow{Role: summary.Role, Content: summary.Content})
	for _, m := range tail {
		msgs = append(msgs, apptypes.ChatMessageRow{Role: m.Role, Content: m.Content})
	}
	if err := c.chatRepo.ReplaceSessionMessages(ctx, sessionID, msgs); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Compressor.persistCompacted", err)
	}
	return nil
}

// 注：工具输出预裁剪（Symbolic Offloading）暂未在 chat 压缩路径实现。
// 原 Gemini 实现存在三处致命缺陷已被移除：
//  1. 写 CWD 相对路径 "vfs/"，绕过 internal/vfs 工作区隔离；
//  2. 未写 workspace_vfs 表，SemanticCompressHandler 按 vfs_id 查表必然落空；
//  3. 原文被替换为存根后无 read_tool_ref 可回读，聊天历史不可逆损毁。
// 正确实现需注入 VFS Offloader + OutboxWriter（见 M05 §6 Symbolic Offloading），另行排期。

// injectTaskCanvas 按 M05 §11.3 Stage 3 格式将 TaskMermaidCanvas 渲染结果注入摘要。
// mmd 为空字符串时原样返回 summary（跳过注入）。
func injectTaskCanvas(mmd, summary string) string {
	if mmd == "" {
		return summary
	}
	return "## Task State (node_id → read_tool_ref)\n" + mmd + "\n## Summary\n" + summary
}

const toolOffloadThreshold = 10 * 1024 // 10KB，对齐 M05 §11.3 Stage 1 描述

// offloadLargeToolResults 将 middle 中超限的 tool 角色消息卸载到 offloader，
// 原地替换为可回读存根。offloader 为 nil 或单条卸载失败时保留原文，不阻断压缩流程。
func offloadLargeToolResults(ctx context.Context, sessionID string, middle []apptypes.Message, offloader ToolRefOffloader) []apptypes.Message {
	if offloader == nil {
		return middle
	}
	out := make([]apptypes.Message, len(middle))
	copy(out, middle)
	for i, m := range out {
		if m.Role != "tool" || len(m.Content) <= toolOffloadThreshold {
			continue
		}
		id, err := offloader.Offload(ctx, sessionID, []byte(m.Content))
		if err != nil {
			slog.Warn("compressor: tool ref offload failed, keeping original", "session", sessionID, "err", err)
			continue // 失败兜底：保留原文，绝不允许丢数据
		}
		out[i].Content = fmt.Sprintf("[offloaded: %d bytes → read_tool_ref(task_id=\"%s\", id=\"%s\")]",
			len(m.Content), sessionID, id)
	}
	return out
}
