package consolidation

import (
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// Consolidation — Episodic → Semantic 记忆压缩管线。
// 架构文档: docs/arch/M05-Memory-System.md §4

// ConsolidationPipeline 4 阶段压缩管线。
// 触发: 主题转换 shift → 立即触发 | eventCount ≥ 50 → 触发 | sessionClosed → 强制触发.
//
// 依赖注入:
//   - episodic: 读取待压缩的 Episodic 事件
//   - semantic: 写入提取出的实体/关系/摘要
//   - skills:   Stage 4 Logic Collapse 注册新 Skill（nil 时跳过）
//   - provider: LLM 提取实体/摘要（nil 时走规则 fallback）
type ConsolidationPipeline struct {
	episodic    protocol.EpisodicMemory
	semantic    protocol.SemanticMemory
	skills      protocol.SkillRegistry
	provider    protocol.Provider
	writeFilter *retrieval.WriteFilter
	cascadeInv  *retrieval.CascadeInvalidator
	db          protocol.SQLQuerier
	gate        backgroundGate
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

func (p *ConsolidationPipeline) WithBackgroundGate(g backgroundGate) { p.gate = g }

// NewConsolidationPipeline 创建压缩管线，episodic 和 semantic 必须非 nil。
func NewConsolidationPipeline(
	episodic protocol.EpisodicMemory,
	semantic protocol.SemanticMemory,
	skills protocol.SkillRegistry,
	provider protocol.Provider,
) *ConsolidationPipeline {
	return &ConsolidationPipeline{
		episodic: episodic,
		semantic: semantic,
		skills:   skills,
		provider: provider,
	}
}

// NewConsolidationPipelineFull 创建完整的固化流水线。
func NewConsolidationPipelineFull(
	e protocol.EpisodicMemory,
	s protocol.SemanticMemory,
	skills protocol.SkillRegistry,
	provider protocol.Provider,
	wf *retrieval.WriteFilter,
	ci *retrieval.CascadeInvalidator,
	db protocol.SQLQuerier,
) *ConsolidationPipeline {
	return &ConsolidationPipeline{
		episodic:    e,
		semantic:    s,
		skills:      skills,
		provider:    provider,
		writeFilter: wf,
		cascadeInv:  ci,
		db:          db,
	}
}

// consolidationTimeout Consolidation 管线最大运行时间（兜底防止阻塞 M9 调度器）
const consolidationTimeout = 5 * time.Minute

// Run 执行完整 4 阶段压缩管线。
// 约束: version++ 不可变版本 + source_event_id provenance + 信念修正 + Prospective Indexing.
// 超时: 整体 5 分钟超时（独立于 ctx 父超时），防止 LLM 调用长时间阻塞调度器。
func (p *ConsolidationPipeline) Run(ctx context.Context, sessionID string) error {
	if p.gate != nil && !p.gate.BackgroundPermit(2) {
		return nil // skip if not permitted
	}
	if p.episodic == nil || p.semantic == nil {
		return apperr.New(apperr.CodeInternal, "consolidation: episodic and semantic memory required")
	}

	// 整体超时保护：Consolidation 为后台任务，不应无限阻塞
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(ctx, consolidationTimeout)
		defer cancel()
	}

	// 查询该 Session 的所有 Episodic 事件
	events, err := p.episodic.Query(ctx, types.EpisodicQuery{
		SessionID:     sessionID,
		K:             200,
		MaxTaintLevel: types.TaintHigh, // 允许读取带污点的事件，并在巩固时向上传播
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: query episodic events", err)
	}
	if len(events) == 0 {
		return nil
	}

	// Stage 1 — 实体/关系提取
	entities, relations, err := p.extractEntitiesAndRelations(ctx, sessionID, events)
	if err != nil {
		// 非阻断：Stage 1 失败不中止后续阶段
		entities = nil
		relations = nil
	}

	// Stage 2 — Upsert Semantic Memory (传播污点)
	maxTaint := computeMaxTaint(events)
	if err := p.upsertSemantic(ctx, entities, relations, maxTaint); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: stage2 upsert", err)
	}

	// Stage 3 — 会话摘要生成
	if err := p.summarizeSession(ctx, sessionID, events); err != nil {
		slog.Warn("consolidation: stage3 summarizeSession failed", "err", err)
	}

	// Stage 3.5 — 用户画像合成（L3 Persona）
	// 触发条件: events ≥ 10（保证最低信号量）。异步友好，失败不阻断。
	// 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
	if len(events) >= 10 {
		if err := p.synthesizeUserProfile(ctx, events); err != nil {
			slog.Warn("consolidation: stage3.5 synthesizeUserProfile failed", "err", err)
		}
	}

	// Stage 4 — Logic Collapse → Skill Library
	if p.skills != nil {
		if err := p.updateSkills(ctx, sessionID, events); err != nil {
			slog.Warn("consolidation: stage4 updateSkills failed", "err", err)
		}
	}

	return nil
}

// MarkColdEpisodicEvents 滑动窗口算法：找出 1 小时以前且未被固化的事件，打上 cold 标签。
// 这是短程记忆降维 (C2) 的实现。
func (p *ConsolidationPipeline) MarkColdEpisodicEvents(ctx context.Context, sessionID string) error {
	if p.episodic == nil {
		return nil
	}

	// 1 小时前的事件
	before := time.Now().Add(-1 * time.Hour)
	_, err := p.episodic.MarkCold(ctx, sessionID, before)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: failed to mark cold episodic events", err)
	}

	return nil
}

// ─── Stage 1 ─────────────────────────────────────────────────────────────────

// extractEntitiesAndRelations 从 Episodic 事件中提取实体与关系。
// 主路径: LLM 提取（provider 非 nil）。回退: 正则/共现规则。
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

	if p.provider != nil {
		return p.llmExtract(ctx, sessionID, text)
	}
	return p.ruleExtract(sessionID, text)
}

// llmExtract 调用 LLM 提取实体/关系，返回 JSON 解析结果。
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

	prompt := fmt.Sprintf(
		"Analyze the following AI agent session log and extract:\n"+
			"1. Named entities (type MUST be one of: user_preference, constraint, temporary_conclusion, entity)\n"+
			"2. Relations between entities (type MUST be one of: depends_on, configures, conflicts_with, relates_to)\n\n"+
			"Return ONLY valid JSON in this format:\n"+
			`{"entities":[{"name":"...","type":"...","confidence":0.9}],"relations":[{"from":"...","to":"...","type":"...","confidence":0.8}]}`+
			"\n\nSession log:\n%s",
		text,
	)
	resp, err := p.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(1024))
	if err != nil {
		return p.ruleExtract(sessionID, text)
	}

	// 解析 JSON
	content := strings.TrimSpace(resp.Content)
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

// ruleExtract 规则回退：正则模式 + 共现关系推断。
func (p *ConsolidationPipeline) ruleExtract(
	_ string, // sessionID 用于 ID 前缀，通过 now 时间戳区分即可
	text string,
) ([]*types.Entity, []*types.Relation, error) {
	now := time.Now().UnixNano()
	var entities []*types.Entity

	patterns := []struct {
		re      *regexp.Regexp
		entType string
	}{
		{regexp.MustCompile(`(?i)\b(tool|skill|func|function)[\s_-]+(\w+)`), "tool"},
		{regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`), "concept"}, // CamelCase 词
		{regexp.MustCompile(`(?:^|\s)([\w./\\-]+\.\w{2,5})\b`), "file"},      // 文件路径
		{regexp.MustCompile(`https?://[^\s"'>]+`), "url"},                    // URL
		{regexp.MustCompile(`\b([A-Z]{2,}(?:_[A-Z]+)*)\b`), "constant"},      // 常量/枚举
	}

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

	for _, e := range entities {
		// 精确碰撞检测：同名同类型已存在 active 实体 → 标记旧版本 superseded
		if existing, err := p.semantic.GetEntity(ctx, e.Type, e.Name); err == nil && existing != nil {
			p.handleExistingEntity(ctx, existing)
		}

		// Jaccard 近似碰撞检测：仅对 user_preference 类型启用（性能敏感，范围受控）
		if e.Type == "user_preference" {
			p.supersedeSimilarPreferences(ctx, e.Name)
		}

		if err := p.semantic.UpsertFact(ctx, *e, maxTaint); err != nil {
			slog.Warn("consolidation: semantic.UpsertFact failed", "err", err)
			continue
		}

		// UpsertFact 成功后查回 DBID，供关系写入使用
		if e.ID != "" {
			if fetched, err := p.semantic.GetEntity(ctx, e.Type, e.Name); err == nil && fetched != nil {
				ephemeralToDBID[e.ID] = fetched.DBID
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

func (p *ConsolidationPipeline) handleExistingEntity(ctx context.Context, existing *types.Entity) {
	if existing.Status != "" && existing.Status != "active" {
		return
	}
	_ = p.semantic.MarkEntitySuperseded(ctx, existing.DBID, 0)
	if existing.DBID <= 0 {
		return
	}
	if p.cascadeInv != nil {
		affected, err := p.cascadeInv.Invalidate(ctx, existing.DBID)
		if err != nil {
			slog.Warn("cascade invalidation failed", "entity_id", existing.DBID, "err", err)
		} else if len(affected) > 0 {
			slog.Info("cascade invalidation triggered", "source", existing.DBID, "affected_count", len(affected))
		}
	}
	if p.db != nil {
		_, _ = p.db.ExecContext(ctx,
			`INSERT INTO episodic_events_change_log(session_id, changed_at, change_type, affected_count)
			 VALUES ('belief_revision', ?, 'superseded', 1)`,
			time.Now().UnixMilli())
	}
}

// supersedeSimilarPreferences 将与 newName Jaccard > 0.6 的活跃 user_preference 标记 superseded。
func (p *ConsolidationPipeline) supersedeSimilarPreferences(ctx context.Context, newName string) {
	actives, err := p.semantic.ListActiveEntities(ctx, "user_preference", 30)
	if err != nil {
		return
	}
	for _, act := range actives {
		if act.Name == newName {
			continue // 精确碰撞已在调用方处理
		}
		if jaccardSimilarity(act.Name, newName) > 0.6 {
			_ = p.semantic.MarkEntitySuperseded(ctx, act.DBID, 0)
		}
	}
}

// jaccardSimilarity 计算两个字符串的 token 级 Jaccard 相似度 [0,1]。
// 分词: 小写化 + 按空格/下划线/驼峰分割。来源: PruneMem curator-apply 近似去重逻辑。
func jaccardSimilarity(a, b string) float64 {
	tokA := jaccardTokenize(a)
	tokB := jaccardTokenize(b)
	if len(tokA) == 0 || len(tokB) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(tokA))
	for _, t := range tokA {
		setA[t] = true
	}
	setB := make(map[string]bool, len(tokB))
	for _, t := range tokB {
		setB[t] = true
	}
	intersection := 0
	for t := range setA {
		if setB[t] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

// jaccardTokenize 将字符串分割为小写 token 集合。
// 按空格、下划线、连字符分割，同时拆分驼峰命名。
func jaccardTokenize(s string) []string {
	s = strings.ToLower(s)
	// 驼峰拆分: 在大写字母前插入空格（先处理原始 camelCase 再转小写无效，但上面已转小写）
	// 简化处理: 按非字母数字字符分割
	var tokens []string
	cur := strings.Builder{}
	for _, r := range s {
		if r == ' ' || r == '_' || r == '-' || r == '.' || r == '/' {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		} else {
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// ─── Stage 3 ─────────────────────────────────────────────────────────────────

// summarizeSession 为会话生成 3-5 句摘要，写入 store.SemanticMemory 作为 compaction 文档。
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
	return p.semantic.StoreDocument(ctx, doc, types.TaintNone)
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

	if p.provider != nil {
		prompt := fmt.Sprintf(
			"Summarize the following AI agent session in 3-5 concise sentences. "+
				"Focus on: what was accomplished, what tools were used, and key outcomes.\n\n%s",
			text,
		)
		resp, err := p.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(256))
		if err == nil && resp != nil {
			return strings.TrimSpace(resp.Content)
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
func (p *ConsolidationPipeline) updateSkills(
	ctx context.Context,
	_ string, // sessionID 保留用于未来的溯源追踪
	events []types.ScoredEvent,
) error {
	if p.skills == nil {
		return nil
	}

	// 统计 tool_call 类型事件的工具名出现次数
	toolCounts := make(map[string]int)
	for _, se := range events {
		if (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type != "tool_result" && (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type != "tool_call" {
			continue
		}
		// 从 payload 中提取 tool_name
		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
			Success  bool   `json:"success"`
		}
		if err := json.Unmarshal((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload, &payload); err != nil {
			continue
		}
		name := payload.ToolName
		if name == "" {
			name = payload.Name
		}
		if name == "" || !payload.Success {
			continue
		}
		toolCounts[name]++
	}

	// 出现 ≥ 3 次的工具提炼为 Skill
	for toolName, count := range toolCounts {
		if count < 3 {
			continue
		}
		meta := types.SkillMeta{
			Name:         "auto_" + toolName,
			Version:      fmt.Sprintf("1.0.%d", time.Now().Unix()),
			Runtime:      "builtin",
			RiskLevel:    "low",
			Sandbox:      1,
			Capabilities: []string{toolName},
			ExecMode:     "tool",
			Trust:        types.TrustTier(1),
			Idempotent:   true,
		}
		if err := p.skills.Register(ctx, meta); err != nil {
			slog.Warn("consolidation: skills.Register failed", "err", err)
		}
	}
	return nil
}

// ─── Stage 3.5 ───────────────────────────────────────────────────────────────

// synthesizeUserProfile 从 Episodic 事件合成用户画像（L3 Persona）。
//
// 触发策略: events ≥ 10 且距上次合成 > 50 事件（由 LastEventTS 间接判断）。
// 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
//
// LLM 路径: provider 非 nil → 用 100 token prompt 让 LLM 归纳 JSON。
// 规则 fallback: provider 为 nil → 统计工具频率 + 收集近期摘要。
func (p *ConsolidationPipeline) synthesizeUserProfile(
	ctx context.Context,
	events []types.ScoredEvent,
) error {
	if p.semantic == nil {
		return nil
	}

	// 读取现有画像（确定是否需要更新）
	current, _ := p.semantic.GetUserProfile(ctx, "default")

	// 若 events 最新时间戳距上次合成 < 1 分钟，跳过（防重复合成）
	if current != nil && len(events) > 0 {
		newestTS := (func() *types.Event {
			if e, _ := events[0].Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).CreatedAt.UnixMilli()
		for _, se := range events {
			if ts := (func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).CreatedAt.UnixMilli(); ts > newestTS {
				newestTS = ts
			}
		}
		if newestTS-current.LastEventTS < 60_000 {
			return nil
		}
	}

	// 收集最新 event 时间戳
	var latestTS int64
	for _, se := range events {
		if ts := (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).CreatedAt.UnixMilli(); ts > latestTS {
			latestTS = ts
		}
	}

	profile := types.UserProfile{
		ProfileKey:         "default",
		StableFacts:        make(map[string]any),
		BehavioralPatterns: make(map[string]any),
		LastEventTS:        latestTS,
	}
	if current != nil {
		profile.SynthesisCount = current.SynthesisCount + 1
		// 保留已有稳定事实（不被规则覆盖）
		maps.Copy(profile.StableFacts, current.StableFacts)
	}

	if p.provider != nil {
		p.llmSynthesizeProfile(ctx, current, events, &profile)
	} else {
		p.ruleSynthesizeProfile(events, &profile)
	}

	return p.semantic.UpsertUserProfile(ctx, profile)
}

// llmSynthesizeProfile 通过 LLM 合成用户画像（100 token prompt，输出 JSON）。
//
//nolint:gocyclo
func (p *ConsolidationPipeline) llmSynthesizeProfile(
	ctx context.Context,
	current *types.UserProfile,
	events []types.ScoredEvent,
	out *types.UserProfile,
) {
	// 组装最近 15 条事件文本
	var sb strings.Builder
	limit := min(15, len(events))
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
		if len(payload) > 100 {
			payload = payload[:100]
		}
		sb.WriteString(payload)
		sb.WriteByte('\n')
	}

	currentJSON := "{}"
	if current != nil {
		if b, err := json.Marshal(current); err == nil {
			currentJSON = string(b)
		}
	}

	prompt := fmt.Sprintf(
		"Based on these agent session events, update the user profile. "+
			"Return ONLY valid JSON with keys: stable_facts (object), recent_activity (string array ≤10 items), behavioral_patterns (object).\n\n"+
			"Current profile: %s\n\nNew events:\n%s",
		currentJSON, sb.String(),
	)

	resp, err := p.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(512))
	if err != nil || resp == nil {
		p.ruleSynthesizeProfile(events, out)
		return
	}

	content := strings.TrimSpace(resp.Content)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var parsed struct {
		StableFacts        map[string]any `json:"stable_facts"`
		RecentActivity     []string       `json:"recent_activity"`
		BehavioralPatterns map[string]any `json:"behavioral_patterns"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		p.ruleSynthesizeProfile(events, out)
		return
	}
	if parsed.StableFacts != nil {
		out.StableFacts = parsed.StableFacts
	}
	if len(parsed.RecentActivity) > 0 {
		out.RecentActivity = parsed.RecentActivity
	}
	if parsed.BehavioralPatterns != nil {
		out.BehavioralPatterns = parsed.BehavioralPatterns
	}
}

// ruleSynthesizeProfile 规则 fallback：统计工具频率 + 收集近期事件摘要。
func (p *ConsolidationPipeline) ruleSynthesizeProfile(
	events []types.ScoredEvent,
	out *types.UserProfile,
) {
	toolFreq := make(map[string]int)
	eventTypFreq := make(map[string]int)
	var recentSummaries []string

	for _, se := range events {
		eventTypFreq[string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type)]++

		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
		}
		if err := json.Unmarshal((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload, &payload); err == nil {
			name := payload.ToolName
			if name == "" {
				name = payload.Name
			}
			if name != "" {
				toolFreq[name]++
			}
		}

		// 收集近期摘要（最多 20 条）
		if len(recentSummaries) < 20 && len((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload) > 0 {
			summary := string((func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).Payload)
			if len(summary) > 80 {
				summary = summary[:80]
			}
			recentSummaries = append(recentSummaries, string((func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).Type)+": "+summary)
		}
	}

	out.BehavioralPatterns["tool_frequency"] = toolFreq
	out.BehavioralPatterns["event_type_frequency"] = eventTypFreq
	out.RecentActivity = recentSummaries
}

// ============================================================================
// Forgetting — 双层策略（热删除 + 冷归档）
// 架构文档: docs/arch/M05-Memory-System.md §5

// ForgettingManager 遗忘管理器。
// 热删除: Q-Learning 效用衰减 → DecayWeight < salienceThreshold → Forgettable.
// 冷归档: Forgettable + age > 30d → 归档 + tombstone.
// store 用于持久化操作（扫描事件、写入归档标记）。
type ForgettingManager struct {
	store             protocol.Store
	decayRate         float64 // 0.01/日
	salienceThreshold float64
	qLearner          *QLearner
	archiver          *ColdArchiver
}

// NewForgettingManager 创建遗忘管理器，注入 Store 依赖。
func NewForgettingManager(store protocol.Store, decayRate float64) *ForgettingManager {
	return &ForgettingManager{
		store:             store,
		decayRate:         decayRate,
		salienceThreshold: 0.15,
		qLearner:          NewQLearner(0.1, 0.9),
		archiver:          NewColdArchiver(store),
	}
}

// UpdateDecay 更新衰减权重。
// ageHours = now - timestamp; DecayWeight = salience × exp(-decayRate × ageHours/24).
func (fm *ForgettingManager) UpdateDecay(salience float64, ageHours float64) float64 {
	decay := salience * exp(-fm.decayRate*ageHours/24.0)
	return decay
}

// PeriodicCleanup 扫描 Episodic 事件，将低于 salienceThreshold 的条目标记为可遗忘，
// 超过 30 天且低 salience 的条目移入冷归档。
// 不物理删除——仅写入 tombstone 标记，由 ColdArchiver.PhysicalCompact 负责最终清理。
func (fm *ForgettingManager) PeriodicCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// P1: Optimization - Use SQL native query if possible
	if sqlStore, ok := fm.store.(protocol.SQLQuerier); ok {
		if err := fm.cleanupWithSQL(ctx, sqlStore); err == nil {
			return nil
		}
	}

	return fm.cleanupWithKV(ctx)
}

func (fm *ForgettingManager) cleanupWithSQL(ctx context.Context, db protocol.SQLQuerier) error {
	rows, err := db.QueryContext(ctx, "SELECT id, salience, occurred_at FROM events WHERE topic IN ('memory.openclaw', 'memory')")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ForgettingManager.cleanupWithSQL", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var salience float64
		var occurredAt int64
		if err := rows.Scan(&id, &salience, &occurredAt); err != nil {
			continue
		}

		ageHours := float64(time.Now().UnixMilli()-occurredAt) / 3600000.0
		decayWeight := fm.UpdateDecay(salience, ageHours)

		if decayWeight < fm.salienceThreshold {
			fm.processForgettableItem(ctx, id, decayWeight, ageHours)
		}
	}
	return nil
}

func (fm *ForgettingManager) cleanupWithKV(ctx context.Context) error {
	iter, err := fm.store.Scan(ctx, []byte("events:"))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PeriodicCleanup: scan events 失败", err)
	}
	defer iter.Close()

	for iter.Next() {
		key := iter.Key()
		val := iter.Value()

		var ev struct {
			ID         string  `json:"id"`
			Topic      string  `json:"topic"`
			Salience   float64 `json:"salience"`
			OccurredAt int64   `json:"occurred_at"`
		}
		if err := json.Unmarshal(val, &ev); err != nil {
			continue
		}

		if ev.Topic != "memory.openclaw" && ev.Topic != "memory" {
			continue
		}

		ageHours := float64(time.Now().UnixMilli()-ev.OccurredAt) / 3600000.0
		decayWeight := fm.UpdateDecay(ev.Salience, ageHours)

		if decayWeight < fm.salienceThreshold {
			fm.processForgettableItemKV(ctx, ev.ID, decayWeight, ageHours, key, val)
		}
	}

	if iter.Err() != nil {
		return apperr.Wrap(apperr.CodeInternal, "PeriodicCleanup: 迭代失败", iter.Err())
	}
	return nil
}

func (fm *ForgettingManager) processForgettableItem(ctx context.Context, id string, decayWeight float64, ageHours float64) {
	tombstoneKey := fmt.Appendf(nil, "forgettable:%s", id)
	tombstoneVal := fmt.Appendf(nil, `{"id":"%s","decay_weight":%.4f,"marked_at":%d}`, id, decayWeight, time.Now().UnixMilli())
	_ = fm.store.Put(ctx, tombstoneKey, tombstoneVal)

	if ageHours > 30*24 {
		if val, getErr := fm.store.Get(ctx, fmt.Appendf(nil, "events:%s", id)); getErr == nil {
			archiveKey := fmt.Appendf(nil, "archive:episodic:%s", id)
			_ = fm.store.Put(ctx, archiveKey, val)
			_ = fm.store.Delete(ctx, fmt.Appendf(nil, "events:%s", id))
			_ = fm.store.Delete(ctx, tombstoneKey)
		}
	}
}

func (fm *ForgettingManager) processForgettableItemKV(ctx context.Context, id string, decayWeight float64, ageHours float64, key, val []byte) {
	tombstoneKey := fmt.Appendf(nil, "forgettable:%s", id)
	tombstoneVal := fmt.Appendf(nil, `{"id":"%s","decay_weight":%.4f,"marked_at":%d}`, id, decayWeight, time.Now().UnixMilli())
	_ = fm.store.Put(ctx, tombstoneKey, tombstoneVal)

	if ageHours > 30*24 {
		archiveKey := fmt.Appendf(nil, "archive:episodic:%s", id)
		_ = fm.store.Put(ctx, archiveKey, val)
		_ = fm.store.Delete(ctx, key)
		_ = fm.store.Delete(ctx, tombstoneKey)
	}
}

// QLearner Q-Learning 熵门控效用衰减。
// 用于自适应调整 salienceThreshold——高熵环境下更积极遗忘。
type QLearner struct {
	states map[string]float64
	alpha  float64 // 学习率
	gamma  float64 // 折扣因子
}

func NewQLearner(alpha, gamma float64) *QLearner {
	return &QLearner{
		states: make(map[string]float64),
		alpha:  alpha,
		gamma:  gamma,
	}
}

// Update 更新状态值。
func (ql *QLearner) Update(state string, reward float64) {
	ql.states[state] += ql.alpha * (reward - ql.states[state])
}

// ColdArchiver 冷归档器。
// 将超期低价值事件从热存储移到归档前缀，SQLite 物理 VACUUM 回收磁盘。
// store 通过协议抽象访问持久化层。
type ColdArchiver struct {
	store         protocol.Store
	archivePath   string // ~/.polarisagi/polaris/archive/
	retentionDays int    // 热库 30d, 冷库无限
}

func NewColdArchiver(store protocol.Store) *ColdArchiver {
	return &ColdArchiver{
		store:         store,
		archivePath:   "archive/",
		retentionDays: 30,
	}
}

// PhysicalCompact 扫描 tombstone 标记（forgettable:*），
// 将对应的原事件 key 物理删除并清理 tombstone 自身。
// 对支持 SQL 的引擎委托 DB 级 VACUUM；对纯 KV 引擎仅做 key 级清理。
func (ca *ColdArchiver) PhysicalCompact() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deleted := 0

	// 扫描所有 forgettable tombstone
	iter, err := ca.store.Scan(ctx, []byte("forgettable:"))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PhysicalCompact: scan tombstones 失败", err)
	}
	defer iter.Close()

	var keysToDelete [][]byte

	for iter.Next() {
		var tombstone struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iter.Value(), &tombstone); err != nil || tombstone.ID == "" {
			continue
		}

		// 删除原事件（可能已被归档，Delete 幂等）
		eventKey := fmt.Appendf(nil, "events:%s", tombstone.ID)
		keysToDelete = append(keysToDelete, eventKey)
		// 删除 tombstone 自身
		keysToDelete = append(keysToDelete, iter.Key())
		deleted++
	}

	if iter.Err() != nil {
		return apperr.Wrap(apperr.CodeInternal, "PhysicalCompact: 迭代失败", iter.Err())
	}

	// 批量删除
	for _, key := range keysToDelete {
		_ = ca.store.Delete(ctx, key)
	}

	// 对支持 SQL 的引擎触发 VACUUM——通过 Txn 内的 Raw SQL 能力
	if ca.store.Capabilities().SupportsSQL {
		_ = ca.store.Txn(ctx, func(tx protocol.Transaction) error {
			// 尝试在 Txn 内执行 VACUUM-like 操作（引擎特定）
			// SQLite 引擎可通过额外接口执行；纯 KV 引擎忽略
			return nil
		})
	}

	_ = deleted
	return nil
}

func exp(x float64) float64 {
	result := 1.0
	term := 1.0
	for i := 1; i < 20; i++ {
		term *= x / float64(i)
		result += term
	}
	return result
}

func computeMaxTaint(events []types.ScoredEvent) types.TaintLevel {
	maxTaint := types.TaintNone
	for _, ev := range events {
		if event, _ := ev.Event.(*types.Event); event != nil {
			if event.TaintLevel > maxTaint {
				maxTaint = event.TaintLevel
			}
		}
	}
	if maxTaint < types.TaintMedium {
		maxTaint = types.TaintMedium
	}
	return maxTaint
}
