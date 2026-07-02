package chat

import (
	"os"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"

	"github.com/polarisagi/polaris/internal/gateway/types"

	"context"

	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

const (
	// charsPerToken 字符/token 粗估（与 hermes _CHARS_PER_TOKEN 一致）
	charsPerToken = 4
	// defaultContextWindow 默认上下文窗口大小（token 数）；Tier-0 保守值
	defaultContextWindow = 32768
	// defaultAutoCompactPct 自动压缩触发百分比，对齐 Claude Code 默认值
	defaultAutoCompactPct = 95.0
	// defaultWarnPct 上下文使用率警告阈值（黄色 Banner 触发点）
	defaultWarnPct = 80.0
	// defaultTailTokens 尾部保护：保留最后 6K token 原文不压缩
	defaultTailTokens = 6144
	// minSummaryTokens 摘要最少 token 数
	minSummaryTokens = 800
	// summaryRatio 摘要 token 占被压缩内容的比例
	summaryRatio = 0.20
	// maxSummaryTokens 摘要 token 上限
	maxSummaryTokens = 6000
	// defaultMaxThrashCount 连续 thrashing 判定次数；超过后停止自动压缩
	defaultMaxThrashCount = 3
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

// types.ContextStats 会话上下文使用统计，由 Stats() 返回。

// Compressor 对超长对话历史进行 LLM 摘要压缩。
// 压缩策略：保护尾部 N token 原文 + 用 LLM 摘要替代中间消息。
// 阈值模型对齐 Claude Code：contextWindow × autoCompactPct%（默认 95%）。
type Compressor struct {
	db             protocol.SQLQuerier
	chatRepo       protocol.ChatRepository
	hooks          *sysadmin.HookRunner
	contextWindow  int     // 上下文窗口大小（token）
	autoCompactPct float64 // 自动压缩触发百分比
	warnPct        float64 // 警告触发百分比
	maxThrashCount int     // 连续 thrashing 上限
	tailTokens     int

	mu            sync.Mutex
	lastCompactAt time.Time
	thrashedCount int // 连续压缩后仍超阈值的次数
}

func NewCompressor(db protocol.SQLQuerier, chatRepo protocol.ChatRepository, hooks *sysadmin.HookRunner, cfg config.CompressorConfig) *Compressor {
	contextWindow := cfg.ContextWindow
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	autoCompactPct := cfg.AutoCompactPct
	if autoCompactPct <= 0 {
		autoCompactPct = defaultAutoCompactPct
	}
	warnPct := cfg.WarnPct
	if warnPct <= 0 {
		warnPct = defaultWarnPct
	}
	maxThrashCount := cfg.MaxThrashCount
	if maxThrashCount <= 0 {
		maxThrashCount = defaultMaxThrashCount
	}
	return &Compressor{
		db:             db,
		chatRepo:       chatRepo,
		hooks:          hooks,
		contextWindow:  contextWindow,
		autoCompactPct: autoCompactPct,
		warnPct:        warnPct,
		maxThrashCount: maxThrashCount,
		tailTokens:     defaultTailTokens,
	}
}

// roughTokens 估算消息列表的 token 数（字符数 / charsPerToken）。
func roughTokens(msgs []apptypes.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / charsPerToken
	}
	return total
}

// autoCompactThreshold 返回自动压缩触发 token 数（contextWindow × autoCompactPct%）。
func (c *Compressor) autoCompactThreshold() int {
	return int(float64(c.contextWindow) * c.autoCompactPct / 100.0)
}

// warnThreshold 返回警告触发 token 数（contextWindow × warnPct%）。
func (c *Compressor) warnThreshold() int {
	return int(float64(c.contextWindow) * c.warnPct / 100.0)
}

// WarnPct 返回当前警告触发百分比（供 sse.go 直接比较 UsagePercent）。
func (c *Compressor) WarnPct() float64 { return c.warnPct }

// IsThrashin 返回当前是否处于 thrashing 状态。
func (c *Compressor) IsThrashin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thrashedCount >= c.maxThrashCount
}

// Stats 返回当前上下文使用统计，不修改任何状态（纯读操作）。
func (c *Compressor) Stats(msgs []apptypes.Message) types.ContextStats {
	tokens := roughTokens(msgs)
	usagePct := 0.0
	if c.contextWindow > 0 {
		usagePct = float64(tokens) * 100.0 / float64(c.contextWindow)
	}
	c.mu.Lock()
	lastAt := c.lastCompactAt
	thrashed := c.thrashedCount >= c.maxThrashCount
	c.mu.Unlock()
	return types.ContextStats{
		TokenCount:    tokens,
		Threshold:     c.autoCompactThreshold(),
		WarnThreshold: c.warnThreshold(),
		UsagePercent:  usagePct,
		LastCompactAt: lastAt,
		MessageCount:  len(msgs),
		Thrashing:     thrashed,
	}
}

// NeedsCompact 判断是否需要自动压缩。thrashing 状态下始终返回 false。
func (c *Compressor) NeedsCompact(msgs []apptypes.Message) bool {
	if c.IsThrashin() {
		return false
	}
	return roughTokens(msgs) >= c.autoCompactThreshold()
}

// types.CompactResult 压缩操作统计（供调用方发 SSE 通知）。

// Compact 自动触发路径：超过阈值时压缩对话历史。
// 若未达阈值、处于 thrashing 状态或 hook 阻塞，返回原消息序列（Skipped=true）。
func (c *Compressor) Compact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider) ([]apptypes.Message, types.CompactResult, error) {
	return c.compact(ctx, sessionID, msgs, provider, false)
}

// ForceCompact 用户主动触发路径：跳过阈值检查，强制压缩，并重置 thrashing 计数。
// 若消息不足以分段（tail 已覆盖全部），返回 Skipped=true。
func (c *Compressor) ForceCompact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider) ([]apptypes.Message, types.CompactResult, error) {
	// 用户手动触发：重置 thrashing 状态，给自动压缩一次新的机会
	c.mu.Lock()
	c.thrashedCount = 0
	c.mu.Unlock()
	return c.compact(ctx, sessionID, msgs, provider, true)
}

// compact 核心压缩逻辑。force=true 跳过 NeedsCompact 阈值检查。
func (c *Compressor) compact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider, force bool) ([]apptypes.Message, types.CompactResult, error) {
	tokensBefore := roughTokens(msgs)
	skip := types.CompactResult{TokensBefore: tokensBefore, Skipped: true}

	if !force && !c.NeedsCompact(msgs) {
		return msgs, skip, nil
	}

	// session.compact.before：同步，阻塞则跳过压缩
	if blocked, reason := c.hooks.FireBefore("session.compact.before", map[string]string{
		"POLARIS_SESSION_ID":  sessionID,
		"POLARIS_TOKEN_COUNT": fmt.Sprintf("%d", tokensBefore),
	}); blocked {
		slog.Info("compressor: compact skipped by hook", "session", sessionID, "reason", reason)
		return msgs, skip, nil
	}

	middle, tail := splitMessages(msgs, c.tailTokens)
	if len(middle) == 0 {
		// tail 已覆盖全部消息，无法进一步压缩
		return msgs, skip, nil
	}

	// Stage 1: tool output pre-pruning
	c.prePruneMessages(ctx, sessionID, msgs)

	summaryBudget := calcSummaryBudget(middle)
	summary, err := c.summarize(ctx, middle, summaryBudget, provider)
	if err != nil {
		slog.Warn("compressor: summarize failed, skipping compact", "session", sessionID, "err", err)
		return msgs, skip, nil
	}

	summaryMsg := apptypes.Message{
		Role:    "assistant",
		Content: compactSummaryPrefix + "\n\n" + summary,
	}
	if err := c.persistCompacted(ctx, sessionID, summaryMsg, tail); err != nil {
		slog.Warn("compressor: persist failed, skipping compact", "session", sessionID, "err", err)
		return msgs, skip, nil
	}

	newMsgs := make([]apptypes.Message, 0, 1+len(tail))
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, tail...)

	tokensAfter := roughTokens(newMsgs)
	result := types.CompactResult{TokensBefore: tokensBefore, TokensAfter: tokensAfter}

	c.mu.Lock()
	c.lastCompactAt = time.Now()
	// 防抖动：压缩后仍超阈值说明单条输出持续填满上下文
	if tokensAfter >= c.autoCompactThreshold() {
		c.thrashedCount++
	} else {
		c.thrashedCount = 0
	}
	c.mu.Unlock()

	slog.Info("compressor: compacted",
		"session", sessionID,
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter,
		"reduction_pct", 100-tokensAfter*100/tokensBefore,
		"force", force,
		"thrash_count", c.thrashedCount,
	)

	c.hooks.Fire("session.compact.after", map[string]string{
		"POLARIS_SESSION_ID":   sessionID,
		"POLARIS_TOKEN_BEFORE": fmt.Sprintf("%d", tokensBefore),
		"POLARIS_TOKEN_AFTER":  fmt.Sprintf("%d", tokensAfter),
	})

	return newMsgs, result, nil
}

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
	return c.chatRepo.ReplaceSessionMessages(ctx, sessionID, msgs)
}

func (c *Compressor) prePruneMessages(ctx context.Context, sessionID string, msgs []apptypes.Message) {
	for i := range msgs {
		msg := &msgs[i]
		if msg.Role != "tool" && msg.Role != "user" {
			continue
		}
		// Try to parse tool_result
		if strings.Contains(msg.Content, "<tool_result") || msg.Role == "tool" {
			if len(msg.Content) > 4000 && c.db != nil {
				// Offload
				id := fmt.Sprintf("tr_%s_%d", sessionID, time.Now().UnixNano())
				_ = os.MkdirAll("vfs", 0755)
				fullPath := "vfs/" + id
				if err := os.WriteFile(fullPath, []byte(msg.Content), 0644); err == nil {
					msg.Content = fmt.Sprintf("[tool output too long, offloaded to %s]", id)
					ev, _ := protocol.NewOutboxEvent(protocol.TopicSemanticCompress, "compress", map[string]string{"vfs_id": id}, "semantic_compress:error_stack:"+id)

					q := "INSERT INTO outbox (target_engine, operation, scope, payload, idempotency_key, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)"
					_, _ = c.db.ExecContext(ctx, q, ev.TargetEngine, ev.Operation, "error_stack", ev.Payload, ev.IdempotencyKey, time.Now().UnixMilli())
				}
			}
		}
	}
}
