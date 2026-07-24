package consolidation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// summarizeSession 为会话生成 3-5 句摘要，写入 store.SemanticMemory 作为 compaction 文档。
func (p *ConsolidationPipeline) summarizeSession(
	ctx context.Context,
	sessionID string,
	events []types.ScoredEvent,
) error {
	summary, err := p.buildSummary(ctx, sessionID, events)
	if err != nil {
		return err
	}
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
	return p.semantic.StoreDocument(ctx, doc, summaryTaintLevel(events)) //nolint:wrapcheck
}

// summaryTaintLevel 计算摘要文档应携带的 TaintLevel（2026-07-14 补齐，M11 §2.5
// SanitizeBySummarization 触发点）。此前硬编码 types.TaintNone，无论被摘要的
// events 是否携带外部/用户输入内容——LLM 摘要可能吸收/复述被摘要事件中的注入
// 内容，一律标记为全信任（TaintNone）等同于让 SanitizeBySummarization 的存在
// 意义落空。
//
// 规则：被摘要事件全部 TaintNone（纯系统内部生成，无外部输入）时摘要保持
// TaintNone，无需人为拉高；一旦存在任何非 TaintNone 来源事件，按 M11 §2.5 强制
// 标记 TaintMedium 硬地板（SanitizeBySummarization 对任意起始 Level 的结果恒为
// TaintMedium）。source="compaction" 享受 M4 Layer A 豁免——不参与
// ActiveContext.TaintLevel 计算，读侧现状见 memory_context.go FTSSearch 分支
// （L2 检索结果当前未纳入 GlobalTaintLevel，与文档描述的豁免语义天然一致）。
func summaryTaintLevel(events []types.ScoredEvent) types.TaintLevel {
	var maxLevel types.TaintLevel
	for _, se := range events {
		if e, ok := se.Event.(*types.Event); ok && e != nil && e.TaintLevel > maxLevel {
			maxLevel = e.TaintLevel
		}
	}
	if maxLevel == types.TaintNone {
		return types.TaintNone
	}
	downgraded := taint.SanitizeBySummarization(taint.NewTaintedString(
		"", taint.TaintSource{OriginTaintLevel: maxLevel}, "consolidation_summary"))
	return downgraded.Source.OriginTaintLevel
}

// buildSummary 调用 LLM 或规则引擎生成摘要文本。
func (p *ConsolidationPipeline) buildSummary(
	ctx context.Context,
	_ string, // sessionID 仅用于兜底文本，已嵌入 events
	events []types.ScoredEvent,
) (string, error) {
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
			return summary, nil
		}
		if err != nil {
			if aerr, ok := err.(*apperr.Error); ok && aerr.Code == apperr.CodeResourceExhausted {
				return "", err
			}
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
	return fmt.Sprintf("Session consolidated: %d events. Types: %s.", len(events), strings.Join(parts, ", ")), nil
}

// ─── Stage 4 ─────────────────────────────────────────────────────────────────
