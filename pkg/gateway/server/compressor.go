package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/polarisagi/polaris/internal/protocol"
)

const (
	// charsPerToken 字符/token 粗估（与 hermes _CHARS_PER_TOKEN 一致）
	charsPerToken = 4
	// defaultCompactThreshold 触发压缩阈值：32K token（Tier-0 兼容）
	defaultCompactThreshold = 32768
	// defaultTailTokens 尾部保护：保留最后 6K token 原文不压缩
	defaultTailTokens = 6144
	// minSummaryTokens 摘要最少 token 数
	minSummaryTokens = 800
	// summaryRatio 摘要 token 占被压缩内容的比例
	summaryRatio = 0.20
	// maxSummaryTokens 摘要 token 上限
	maxSummaryTokens = 6000
)

// compactSummaryPrefix 告知后续 LLM：这是参考摘要，不是待执行指令。
// 设计来源：hermes-agent context_compressor.py SUMMARY_PREFIX。
// 若不加此前缀，LLM 可能把摘要中的历史请求当作当前任务重复执行。
const compactSummaryPrefix = "[上下文压缩摘要 — 仅供参考] " +
	"以下是之前对话的摘要，作为背景参考信息。" +
	"请勿将摘要中的请求视为当前待执行的指令（它们已经处理完毕）。" +
	"当前任务见「## 进行中任务」章节。" +
	"请仅响应本摘要之后出现的最新用户消息。"

// compactSummarizePrompt 摘要生成指令
const compactSummarizePrompt = `你是一个对话摘要助手。以下是历史对话记录。
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

// Compressor 对超长对话历史进行 LLM 摘要压缩。
// 压缩策略：保护尾部 N token 原文 + 用 LLM 摘要替代中间消息。
type Compressor struct {
	db             *sql.DB
	hooks          *HookRunner
	tokenThreshold int
	tailTokens     int
}

func newCompressor(db *sql.DB, hooks *HookRunner) *Compressor {
	return &Compressor{
		db:             db,
		hooks:          hooks,
		tokenThreshold: defaultCompactThreshold,
		tailTokens:     defaultTailTokens,
	}
}

// roughTokens 估算消息列表的 token 数（字符数 / charsPerToken）。
func roughTokens(msgs []protocol.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / charsPerToken
	}
	return total
}

// NeedsCompact 判断消息序列是否超过压缩阈值。
func (c *Compressor) NeedsCompact(msgs []protocol.Message) bool {
	return roughTokens(msgs) > c.tokenThreshold
}

// CompactResult 压缩操作统计（供调用方发 SSE 通知）。
type CompactResult struct {
	TokensBefore int
	TokensAfter  int
	Skipped      bool // hook 阻塞或降级时为 true
}

// Compact 压缩对话历史并持久化到 DB。
// 流程：compact.before hook → 分离 middle/tail → LLM 摘要 → 持久化 → compact.after hook。
// 若 hook 阻塞或摘要失败，返回原消息序列（降级为不压缩，Skipped=true）。
func (c *Compressor) Compact(ctx context.Context, sessionID string, msgs []protocol.Message, provider protocol.Provider) ([]protocol.Message, CompactResult, error) {
	tokensBefore := roughTokens(msgs)

	skip := CompactResult{TokensBefore: tokensBefore, Skipped: true}

	// session.compact.before：同步，阻塞则跳过压缩
	if blocked, reason := c.hooks.FireBefore("session.compact.before", map[string]string{
		"POLARIS_SESSION_ID":  sessionID,
		"POLARIS_TOKEN_COUNT": fmt.Sprintf("%d", tokensBefore),
	}); blocked {
		slog.Info("compressor: compact skipped by hook", "session", sessionID, "reason", reason)
		return msgs, skip, nil
	}

	// 从尾部向前积累，找到 tail 分割点
	tailBudget := c.tailTokens * charsPerToken
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
		// 尾部已覆盖全部，无需压缩
		return msgs, skip, nil
	}
	middleMsgs := msgs[:splitIdx]
	tailMsgs := msgs[splitIdx:]

	// 计算摘要 token 预算（按被压缩内容的 summaryRatio）
	middleChars := 0
	for _, m := range middleMsgs {
		middleChars += len(m.Content)
	}
	summaryMaxTokens := int(float64(middleChars/charsPerToken) * summaryRatio)
	summaryMaxTokens = max(summaryMaxTokens, minSummaryTokens)
	summaryMaxTokens = min(summaryMaxTokens, maxSummaryTokens)

	summary, err := c.summarize(ctx, middleMsgs, summaryMaxTokens, provider)
	if err != nil {
		slog.Warn("compressor: summarize failed, skipping compact", "session", sessionID, "err", err)
		return msgs, skip, nil
	}

	summaryMsg := protocol.Message{
		Role:    "assistant",
		Content: compactSummaryPrefix + "\n\n" + summary,
	}

	if err := c.persistCompacted(ctx, sessionID, summaryMsg, tailMsgs); err != nil {
		slog.Warn("compressor: persist failed, skipping compact", "session", sessionID, "err", err)
		return msgs, skip, nil
	}

	newMsgs := make([]protocol.Message, 0, 1+len(tailMsgs))
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, tailMsgs...)

	tokensAfter := roughTokens(newMsgs)
	result := CompactResult{TokensBefore: tokensBefore, TokensAfter: tokensAfter}

	slog.Info("compressor: compacted",
		"session", sessionID,
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter,
		"reduction_pct", 100-tokensAfter*100/tokensBefore,
	)

	c.hooks.Fire("session.compact.after", map[string]string{
		"POLARIS_SESSION_ID":   sessionID,
		"POLARIS_TOKEN_BEFORE": fmt.Sprintf("%d", tokensBefore),
		"POLARIS_TOKEN_AFTER":  fmt.Sprintf("%d", tokensAfter),
	})

	return newMsgs, result, nil
}

// summarize 调用 provider 对 middle 消息生成结构化摘要。
func (c *Compressor) summarize(ctx context.Context, msgs []protocol.Message, maxTokens int, provider protocol.Provider) (string, error) {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString("[")
		sb.WriteString(m.Role)
		sb.WriteString("]: ")
		// 单条消息截断防止超限
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

	inferReq := &protocol.InferRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: compactSummarizePrompt},
			{Role: "user", Content: "请为以下对话生成摘要：\n\n" + transcript},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.3,
	}

	ch, err := provider.StreamInfer(ctx, inferReq)
	if err != nil {
		return "", err
	}

	var result strings.Builder
	for ev := range ch {
		switch ev.Type {
		case protocol.StreamTextDelta:
			if ev.Content != "" {
				result.WriteString(ev.Content)
			}
		case protocol.StreamError:
			if ev.Content != "" {
				return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("summarize stream: %s", ev.Content))
			}
		}
	}
	return strings.TrimSpace(result.String()), nil
}

// persistCompacted 原子替换 chat_messages：删除旧消息，写入摘要 + tail。
// 在事务内完成，保证 SQLite 单连接安全。
func (c *Compressor) persistCompacted(ctx context.Context, sessionID string, summary protocol.Message, tail []protocol.Message) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_messages WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	ins := func(role, content string) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO chat_messages(session_id, role, content) VALUES(?,?,?)`,
			sessionID, role, content)
		return err
	}
	if err := ins(summary.Role, summary.Content); err != nil {
		return err
	}
	for _, m := range tail {
		if err := ins(m.Role, m.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}
