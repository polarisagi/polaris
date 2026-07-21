// Package compact 提供与消息列表持久化方式无关的上下文压缩算法（M05 §11.3
// Stage 1/2/3），供 M4 Agent 内核热路径（内存中动态组装的 reqMsgs）与 M5/网关
// SessionCompressor（持久化于 chat_messages 表的会话历史）共享同一套压缩逻辑，
// 不各自维护一份容易漂移的重复实现。
//
// 2026-07-22 一致性审查（ADR-0052 DEFER 项 `ContextWindowManager.NeedsCompaction`
// 的后续闭环，见 ADR-0060）背景：M04-Agent-Kernel.md §7 明确要求"M4
// ContextWindowManager 持有热路径阈值，触发时调用 M5 SessionCompressor 的
// Stage1/2/3"，但 M4 的 reqMsgs（PromptFn 每轮现场组装）与 M5/网关 Compressor
// 操作的 []apptypes.Message（chat_messages 持久化行）实际上是同一个 Go 类型
// pkg/types.Message——真正的障碍不是"消息表示不同"，而是原实现把 Stage1/2/3
// 算法与"网关专属关注点"（chat_messages 持久化回写、hook 触发、thrashing 计数、
// 配置来源）耦合在同一个 *chat.Compressor 结构体里。本包只抽出前者（纯算法/
// 只需要 protocol.Provider 与一个窄 Offloader 接口），网关专属部分留在
// internal/gateway/server/chat/compressor.go 里不动。
package compact

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CharsPerToken 字符/token 粗估系数（与 hermes _CHARS_PER_TOKEN 及网关 Compressor 一致）。
const CharsPerToken = 4

// ToolOffloadThreshold Stage 1 工具输出卸载阈值（10KB，对齐 M05 §11.3 Stage 1 描述）。
const ToolOffloadThreshold = 10 * 1024

// 默认摘要预算参数（Stage 2），与网关 Compressor 此前的私有常量取值保持一致，
// 调用方也可传入自定义值（例如 M4 热路径可能希望摘要预算更保守）。
const (
	DefaultMinSummaryTokens = 800
	DefaultMaxSummaryTokens = 6000
	DefaultSummaryRatio     = 0.20
)

// SummaryPrefix 告知后续 LLM：这是参考摘要，不是待执行指令。
// 设计来源：hermes-agent context_compressor.py SUMMARY_PREFIX。
// 若不加此前缀，LLM 可能把摘要中的历史请求当作当前任务重复执行。
const SummaryPrefix = "[上下文压缩摘要 — 仅供参考] " +
	"以下是之前对话的摘要，作为背景参考信息。" +
	"请勿将摘要中的请求视为当前待执行的指令（它们已经处理完毕）。" +
	"当前任务见「## 进行中任务」章节。" +
	"请仅响应本摘要之后出现的最新用户消息。"

// SummarizePrompt 摘要生成指令。
const SummarizePrompt = `你是一个对话摘要助手。以下是历史对话记录。
请生成一份简洁的结构化摘要，供后续对话参考。

输出格式（使用中文，保留技术细节）：

## 已解决问题
（列出已完成的任务和问题）

## 进行中任务
（当前活跃且尚未完成的任务，请明确说明）

## 重要决策与上下文
（关键技术决策、代码变更、配置信息等）

## 待处理事项
（尚未处理的问题或用户请求）

规则：代码片段用代码块包裹；禁止编造对话中未出现的内容。`

// Offloader Stage 1 工具输出符号化卸载消费端接口（HE-3：接口在调用方定义）。
// internal/memory.ToolRefOffloader 与 internal/gateway/server/chat.ToolRefOffloader
// 均已结构化满足此签名，无需显式类型转换即可传入。
type Offloader interface {
	Offload(ctx context.Context, taskID string, content []byte) (id string, err error)
}

// RoughTokens 估算消息列表的 token 数（字符数 / CharsPerToken）。
func RoughTokens(msgs []types.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / CharsPerToken
	}
	return total
}

// SplitMessages 从尾部向前积累，返回 (middle, tail）。
// tail 保留约 tailTokens 个 token 的原始消息；middle 为其余部分（待压缩）。
func SplitMessages(msgs []types.Message, tailTokens int) (middle, tail []types.Message) {
	tailBudget := tailTokens * CharsPerToken
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

// CalcSummaryBudget 根据被压缩内容长度计算 LLM 摘要 token 预算。
func CalcSummaryBudget(middle []types.Message, ratio float64, minTokens, maxTokens int) int {
	middleChars := 0
	for _, m := range middle {
		middleChars += len(m.Content)
	}
	budget := int(float64(middleChars/CharsPerToken) * ratio)
	budget = max(budget, minTokens)
	budget = min(budget, maxTokens)
	return budget
}

// BuildTranscript 拼接消息序列为文本摘要输入，单条消息截断防超限。
func BuildTranscript(msgs []types.Message) string {
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

// Summarize 调用 provider 对 msgs 生成结构化摘要（Stage 2：LLM 锚点摘要）。
func Summarize(ctx context.Context, msgs []types.Message, maxTokens int, provider protocol.Provider) (string, error) {
	transcript := BuildTranscript(msgs)
	reqMsgs := []types.Message{
		{Role: "system", Content: SummarizePrompt},
		{Role: "user", Content: "请为以下对话生成摘要：\n\n" + transcript},
	}

	ch, err := safecall.StreamInfer(ctx, provider, reqMsgs, types.WithMaxTokens(maxTokens), types.WithTemperature(0.3))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "compact.Summarize", err)
	}

	var result strings.Builder
	for ev := range ch {
		switch ev.Type {
		case types.StreamTextDelta:
			if ev.Content != "" {
				result.WriteString(ev.Content)
			}
		case types.StreamError:
			if ev.Content != "" {
				return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("compact.Summarize stream: %s", ev.Content))
			}
		}
	}
	return strings.TrimSpace(result.String()), nil
}

// InjectTaskCanvas 按 M05 §11.3 Stage 3 格式将 TaskMermaidCanvas 渲染结果注入摘要。
// mmd 为空字符串时原样返回 summary（跳过注入）。
func InjectTaskCanvas(mmd, summary string) string {
	if mmd == "" {
		return summary
	}
	return "## Task State (node_id → read_tool_ref)\n" + mmd + "\n## Summary\n" + summary
}

// OffloadLargeToolResults 将 msgs 中超过 ToolOffloadThreshold 的 tool 角色消息卸载到
// offloader，原地替换为可回读存根（Stage 1）。offloader 为 nil 或单条卸载失败时
// 保留原文，不阻断压缩流程（失败兜底：绝不允许丢数据）。
func OffloadLargeToolResults(ctx context.Context, taskID string, msgs []types.Message, offloader Offloader) []types.Message {
	if offloader == nil {
		return msgs
	}
	out := make([]types.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if m.Role != "tool" || len(m.Content) <= ToolOffloadThreshold {
			continue
		}
		id, err := offloader.Offload(ctx, taskID, []byte(m.Content))
		if err != nil {
			slog.Warn("compact: tool ref offload failed, keeping original", "task_id", taskID, "err", err)
			continue
		}
		out[i].Content = fmt.Sprintf("[offloaded: %d bytes → read_tool_ref(task_id=\"%s\", id=\"%s\")]",
			len(m.Content), taskID, id)
	}
	return out
}
