package consolidation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func (p *ConsolidationPipeline) summarizeSession(
	ctx context.Context,
	sessionID string,
	events []types.ScoredEvent,
) error {
	summary := p.buildSummary(ctx, sessionID, events)
	if summary == "" {
		return nil
	}

	doc := types.Document{
		ID:         "summary_" + sessionID,
		SourceType: "compaction",
		SourceURI:  summary,
		Title:      "Session summary: " + sessionID,
		Version:    fmt.Sprintf("%d", time.Now().Unix()),
	}
	return p.semantic.StoreDocument(ctx, doc, types.TaintNone) //nolint:wrapcheck
}

// buildSummary 调用 LLM 或规则引擎生成摘要文本。
func (p *ConsolidationPipeline) buildSummary(
	ctx context.Context,
	_ string, // sessionID 仅用于兜底文本，已嵌入 events
	events []types.ScoredEvent,
) string {
	// 组装最近 20 条事件作为摘要输入
	var sb strings.Builder
	limit := min(20, len(events))
	for _, se := range events[:limit] {
		sb.WriteString(string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type))
		sb.WriteString(": ")
		payload := string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload)
		if len(payload) > 200 {
			payload = payload[:200]
		}
		sb.WriteString(payload)
		sb.WriteByte('\n')
	}
	text := sb.String()

	if p.summarizer != nil {
		summary, err := p.summarizer.Summarize(ctx, text, 256)
		if err == nil && summary != "" {
			return summary
		}
		if err != nil {
			slog.Warn("consolidation_summary: LLM inference failed, falling back to rule-based summary", "err", err)
		}
	}

	// 规则 fallback：拼接前 5 条事件类型作为简要摘要
	eventTypes := make(map[string]int)
	for _, se := range events {
		eventTypes[string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type)]++
	}
	parts := make([]string, 0, min(len(eventTypes), 5))
	for t, n := range eventTypes {
		parts = append(parts, fmt.Sprintf("%s×%d", t, n))
		if len(parts) >= 5 {
			break
		}
	}
	return fmt.Sprintf("Session consolidated: %d events. Types: %s.", len(events), strings.Join(parts, ", "))
}

// ─── Stage 4 ─────────────────────────────────────────────────────────────────

// updateSkills 从成功的工具调用事件中提炼并注册技能（Logic Collapse）。
// 触发条件: 同一 tool_name 在 session 中成功调用 ≥ 3 次。
