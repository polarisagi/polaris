package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/prompt/templates"
	"github.com/polarisagi/polaris/pkg/types"
)

// extractEntitiesAndRelations 从 Episodic 事件中提取实体与关系。
// 主路径: LLM 提取（summarizer 非 nil）。回退: 正则/共现规则。
func (p *ConsolidationPipeline) extractEntitiesAndRelations(
	ctx context.Context,
	sessionID string,
	events []types.ScoredEvent,
) ([]*types.Entity, []*types.Relation, error) {
	// 拼接事件文本供 LLM 或规则引擎处理
	var sb strings.Builder
	for _, se := range events {
		if len((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload) > 0 {
			sb.Write((func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).Payload)
			sb.WriteByte('\n')
		}
	}
	text := sb.String()
	if len(text) > 8000 {
		text = text[:8000]
	}

	// D3（ADR-0077）：统一实体/关系抽取优先走 SharedEntityExtractor（复用
	// internal/knowledge/graphrag 的唯一 LLM 抽取实现，与 RAG 文档摄取共享同一
	// 套 Prompt/LLM 调用）。未注入时（如未装配 graphrag 依赖的部署形态）退化到
	// 本包历史上自带的 llmExtract 路径，再退化到 ruleExtract，三级降级链保持
	// 向后兼容，不引入硬依赖。
	if p.entityExtractor != nil {
		if p.writeFilter != nil {
			eval := p.writeFilter.Evaluate(ctx, text, 0, 0)
			if eval.ShouldSkip {
				slog.Debug("consolidation: retrieval.WriteFilter skipped content", "reason", eval.Reason, "score", eval.Value)
				return nil, nil, nil
			}
		}
		entities, relations, err := p.entityExtractor.ExtractEntitiesAndRelations(ctx, sessionID, text)
		if err != nil {
			slog.Warn("consolidation: SharedEntityExtractor failed, fallback to rule extract", "err", err)
			return p.ruleExtract(sessionID, text)
		}
		return entities, relations, nil
	}
	if p.summarizer != nil {
		return p.llmExtract(ctx, sessionID, text)
	}
	return p.ruleExtract(sessionID, text)
}

// llmExtract 调用 LLM 提取实体/关系，返回 JSON 解析结果。
//
// D3（ADR-0077）：本方法是 SharedEntityExtractor 未注入时的历史兼容回退路径
// （不再是主路径）——新部署应通过 cmd/polaris 装配 graphrag.GraphBuildPipeline
// 并调用 WithEntityExtractor 注入，使实体/关系抽取与 RAG 文档摄取共享同一实现。
// 保留本方法只是为了不强制要求装配 graphrag 依赖（如极简 Tier-0 部署）。
func (p *ConsolidationPipeline) llmExtract(
	ctx context.Context,
	sessionID string,
	text string,
) ([]*types.Entity, []*types.Relation, error) {
	// 写入前主动价值评估 (retrieval.WriteFilter)
	if p.writeFilter != nil {
		eval := p.writeFilter.Evaluate(ctx, text, 0, 0)
		if eval.ShouldSkip {
			slog.Debug("consolidation: retrieval.WriteFilter skipped content", "reason", eval.Reason, "score", eval.Value)
			return nil, nil, nil
		}
	}

	promptText, err := templates.Render("entity_extraction.tmpl", map[string]any{"Text": text})
	if err != nil {
		slog.Warn("consolidation: render entity_extraction template failed, fallback to rule extract", "err", err)
		return p.ruleExtract(sessionID, text)
	}
	respContent, err := p.summarizer.InferRaw(ctx, promptText, 1024)
	if err != nil {
		return p.ruleExtract(sessionID, text)
	}

	// 解析 JSON
	content := strings.TrimSpace(respContent)
	// 去掉可能的 markdown 包裹
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var result struct {
		Entities []struct {
			Name       string  `json:"name"`
			Type       string  `json:"type"`
			Confidence float64 `json:"confidence"`
		} `json:"entities"`
		Relations []struct {
			From       string  `json:"from"`
			To         string  `json:"to"`
			Type       string  `json:"type"`
			Confidence float64 `json:"confidence"`
		} `json:"relations"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return p.ruleExtract(sessionID, text)
	}

	now := time.Now().UnixNano()
	entities := make([]*types.Entity, 0, len(result.Entities))
	entityIdx := make(map[string]string) // name → ID

	for i, e := range result.Entities {
		if e.Name == "" {
			continue
		}
		id := fmt.Sprintf("ent_%s_%d_%d", sessionID, now, i)
		entities = append(entities, &types.Entity{
			ID:          id,
			Name:        e.Name,
			Type:        e.Type,
			SourceDocID: sessionID,
			TaintLevel:  types.TaintLevel(0),
			SyncVersion: now,
			Confidence:  e.Confidence,
		})
		entityIdx[e.Name] = id
	}

	relations := make([]*types.Relation, 0, len(result.Relations))
	for _, r := range result.Relations {
		fromID, okFrom := entityIdx[r.From]
		toID, okTo := entityIdx[r.To]
		if !okFrom || !okTo {
			continue
		}
		relations = append(relations, &types.Relation{
			FromEntityID: fromID,
			ToEntityID:   toID,
			RelationType: r.Type,
			SourceDocID:  sessionID,
			Confidence:   r.Confidence,
			TaintLevel:   types.TaintLevel(0),
		})
	}

	return entities, relations, nil
}

//nolint:gochecknoglobals
var getEntityExtractPatterns = sync.OnceValue(func() []struct {
	re      *regexp.Regexp
	entType string
} {
	return []struct {
		re      *regexp.Regexp
		entType string
	}{
		{regexp.MustCompile(`(?i)\b(tool|skill|func|function)[\s_-]+(\w+)`), "tool"},
		{regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`), "concept"}, // CamelCase 词
		{regexp.MustCompile(`(?:^|\s)([\w./\\-]+\.\w{2,5})\b`), "file"},      // 文件路径
		{regexp.MustCompile(`https?://[^\s"'>]+`), "url"},                    // URL
		{regexp.MustCompile(`\b([A-Z]{2,}(?:_[A-Z]+)*)\b`), "constant"},      // 常量/枚举
	}
})

// ruleExtract 规则回退：正则模式 + 共现关系推断。
func (p *ConsolidationPipeline) ruleExtract(
	_ string, // sessionID 用于 ID 前缀，通过 now 时间戳区分即可
	text string,
) ([]*types.Entity, []*types.Relation, error) {
	now := time.Now().UnixNano()
	var entities []*types.Entity

	patterns := getEntityExtractPatterns()

	seen := make(map[string]bool)
	for i, pat := range patterns {
		matches := pat.re.FindAllString(text, 20)
		for j, m := range matches {
			m = strings.TrimSpace(m)
			if len(m) < 2 || seen[m] {
				continue
			}
			seen[m] = true
			id := fmt.Sprintf("ent_%d_%d_%d", now, i, j)
			entities = append(entities, &types.Entity{
				ID:          id,
				Name:        m,
				Type:        pat.entType,
				TaintLevel:  types.TaintLevel(0),
				SyncVersion: now,
				Confidence:  0.5,
			})
		}
	}

	// 共现关系：相邻实体对
	var relations []*types.Relation
	for i := 0; i < len(entities)-1 && i < 10; i++ {
		relations = append(relations, &types.Relation{
			FromEntityID: entities[i].ID,
			ToEntityID:   entities[i+1].ID,
			RelationType: "co_occurs",
			Confidence:   0.5,
			TaintLevel:   types.TaintLevel(0),
		})
	}

	return entities, relations, nil
}

// ─── Stage 2 ─────────────────────────────────────────────────────────────────

// upsertSemantic 将实体和关系批量写入 store.SemanticMemory，含生命周期信念修正。
//
// 信念修正策略（来源: supermemory + PruneMem 收敛）:
//  1. 精确碰撞（相同 entity_type + name）: 写入新实体前将旧实体标记 superseded
//  2. Jaccard 近似碰撞（相同 type，name 相似度 > 0.6）: 检测语义近似实体并标记 superseded
//     主要用于 user_preference 类型（最易产生"language"/"prog_lang" 之类语义重复）
//
// 关系写入：UpsertRelation 需要 DB 主键（source_id/target_id），因此在 UpsertFact 后
// 立即 GetEntity 查回 DBID，建立 ephemeralID → DBID 映射，再批量写关系。
func (p *ConsolidationPipeline) upsertSemantic(
	ctx context.Context,
	entities []*types.Entity,
	relations []*types.Relation,
	maxTaint types.TaintLevel,
) error {
	// ephemeralID（llmExtract 分配的内存 ID）→ 数据库自增 DBID
	ephemeralToDBID := make(map[string]int64, len(entities))

	// 使用抽取出的排他写入器，复用其精确碰撞、Jaccard 近似碰撞与级联失效逻辑
	exclusiveWriter := retrieval.NewExclusiveWriter(p.semantic, p.cascadeInv, p.db)

	for _, e := range entities {
		if err := exclusiveWriter.UpsertFactExclusive(ctx, e, maxTaint); err != nil {
			slog.Warn("consolidation: semantic.UpsertFactExclusive failed", "err", err)
			continue
		}

		// UpsertFact 成功后查回 DBID，供关系写入使用
		if e.ID != "" {
			if fetched, err := p.semantic.GetEntity(ctx, e.Type, e.Name); err == nil && fetched != nil {
				ephemeralToDBID[e.ID] = fetched.DBID
			} else if p.graphFetcher != nil {
				// B2 复核（本轮审查）：此分支此前会查询 graphFetcher 但取到结果后仅打印
				// debug 日志、不做任何事——注释曾暗示"用作 fallback"，实际从未消费查询
				// 结果，是误导性的死分支。保留检测本身用于诊断日志（GetEntity 在刚写入
				// 后失败极罕见，多半意味着 UpsertFactExclusive 把该实体判定为
				// superseded/合并进了别的实体，而非真的丢失），但不再暗示存在真实回退路径：
				// GraphRAG 侧实体的 DBID 映射完全依赖 GraphWriter.UpsertEntity 写入期已将
				// 其落入同一张 semantic_entities 表（B2 写入期桥接），本函数无需、也没有
				// 另一套从 graphFetcher 取 DBID 的机制。
				if _, gErr := p.graphFetcher.GetEntityByName(ctx, e.Name); gErr == nil {
					slog.Debug("consolidation: entity exists in graphFetcher but GetEntity(semantic) missed it; likely superseded/merged, not a genuine cross-pipeline DBID gap", "name", e.Name)
				}
			}
		}
	}

	for _, r := range relations {
		rc := *r
		// 将 ephemeral 字符串 ID 解析为 DB 整数 ID
		if dbid, ok := ephemeralToDBID[r.FromEntityID]; ok {
			rc.FromDBID = dbid
		}
		if dbid, ok := ephemeralToDBID[r.ToEntityID]; ok {
			rc.ToDBID = dbid
		}
		if err := p.semantic.UpsertRelation(ctx, rc, maxTaint); err != nil {
			slog.Warn("consolidation: semantic.UpsertRelation failed", "err", err)
		}
	}
	return nil
}
