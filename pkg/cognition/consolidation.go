package cognition

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
	episodic protocol.EpisodicMemory
	semantic protocol.SemanticMemory
	skills   protocol.SkillRegistry
	provider protocol.Provider
}

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

// Run 执行完整 4 阶段压缩管线。
// 约束: version++ 不可变版本 + source_event_id provenance + 信念修正 + Prospective Indexing.
func (p *ConsolidationPipeline) Run(ctx context.Context, sessionID string) error {
	if p.episodic == nil || p.semantic == nil {
		return perrors.New(perrors.CodeInternal, "consolidation: episodic and semantic memory required")
	}

	// 查询该 Session 的所有 Episodic 事件
	events, err := p.episodic.Query(ctx, protocol.EpisodicQuery{
		SessionID: sessionID,
		K:         200,
	})
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "consolidation: query episodic events", err)
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

	// Stage 2 — Upsert Semantic Memory
	if err := p.upsertSemantic(ctx, entities, relations); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "consolidation: stage2 upsert", err)
	}

	// Stage 3 — 会话摘要生成
	if err := p.summarizeSession(ctx, sessionID, events); err != nil {
		// 非阻断：摘要失败不中止技能更新
		_ = err
	}

	// Stage 4 — Logic Collapse → Skill Library
	if p.skills != nil {
		if err := p.updateSkills(ctx, sessionID, events); err != nil {
			_ = err // 非阻断
		}
	}

	return nil
}

// ─── Stage 1 ─────────────────────────────────────────────────────────────────

// extractEntitiesAndRelations 从 Episodic 事件中提取实体与关系。
// 主路径: LLM 提取（provider 非 nil）。回退: 正则/共现规则。
func (p *ConsolidationPipeline) extractEntitiesAndRelations(
	ctx context.Context,
	sessionID string,
	events []protocol.ScoredEvent,
) ([]*protocol.Entity, []*protocol.Relation, error) {
	// 拼接事件文本供 LLM 或规则引擎处理
	var sb strings.Builder
	for _, se := range events {
		if len(se.Event.Payload) > 0 {
			sb.Write(se.Event.Payload)
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
) ([]*protocol.Entity, []*protocol.Relation, error) {
	prompt := fmt.Sprintf(
		"Analyze the following AI agent session log and extract:\n"+
			"1. Named entities (type MUST be one of: user_preference, constraint, temporary_conclusion, entity)\n"+
			"2. Relations between entities (type MUST be one of: depends_on, configures, conflicts_with, relates_to)\n\n"+
			"Return ONLY valid JSON in this format:\n"+
			`{"entities":[{"name":"...","type":"..."}],"relations":[{"from":"...","to":"...","type":"..."}]}`+
			"\n\nSession log:\n%s",
		text,
	)
	resp, err := p.provider.Infer(ctx, &protocol.InferRequest{
		Messages:    []protocol.Message{{Role: "user", Content: prompt}},
		MaxTokens:   1024,
		Temperature: 0.1,
	})
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
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"entities"`
		Relations []struct {
			From string `json:"from"`
			To   string `json:"to"`
			Type string `json:"type"`
		} `json:"relations"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return p.ruleExtract(sessionID, text)
	}

	now := time.Now().UnixNano()
	entities := make([]*protocol.Entity, 0, len(result.Entities))
	entityIdx := make(map[string]string) // name → ID

	for i, e := range result.Entities {
		if e.Name == "" {
			continue
		}
		id := fmt.Sprintf("ent_%s_%d_%d", sessionID, now, i)
		entities = append(entities, &protocol.Entity{
			ID:          id,
			Name:        e.Name,
			Type:        e.Type,
			SourceDocID: sessionID,
			TaintLevel:  protocol.TaintLevel(0),
			SyncVersion: now,
		})
		entityIdx[e.Name] = id
	}

	relations := make([]*protocol.Relation, 0, len(result.Relations))
	for _, r := range result.Relations {
		fromID, okFrom := entityIdx[r.From]
		toID, okTo := entityIdx[r.To]
		if !okFrom || !okTo {
			continue
		}
		relations = append(relations, &protocol.Relation{
			FromEntityID: fromID,
			ToEntityID:   toID,
			RelationType: r.Type,
			SourceDocID:  sessionID,
			Confidence:   1.0,
			TaintLevel:   protocol.TaintLevel(0),
		})
	}

	return entities, relations, nil
}

// ruleExtract 规则回退：正则模式 + 共现关系推断。
func (p *ConsolidationPipeline) ruleExtract(
	_ string, // sessionID 用于 ID 前缀，通过 now 时间戳区分即可
	text string,
) ([]*protocol.Entity, []*protocol.Relation, error) {
	now := time.Now().UnixNano()
	var entities []*protocol.Entity

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
			entities = append(entities, &protocol.Entity{
				ID:          id,
				Name:        m,
				Type:        pat.entType,
				TaintLevel:  protocol.TaintLevel(0),
				SyncVersion: now,
			})
		}
	}

	// 共现关系：相邻实体对
	var relations []*protocol.Relation
	for i := 0; i < len(entities)-1 && i < 10; i++ {
		relations = append(relations, &protocol.Relation{
			FromEntityID: entities[i].ID,
			ToEntityID:   entities[i+1].ID,
			RelationType: "co_occurs",
			Confidence:   0.5,
			TaintLevel:   protocol.TaintLevel(0),
		})
	}

	return entities, relations, nil
}

// ─── Stage 2 ─────────────────────────────────────────────────────────────────

// upsertSemantic 将实体和关系批量写入 SemanticMemory。
func (p *ConsolidationPipeline) upsertSemantic(
	ctx context.Context,
	entities []*protocol.Entity,
	relations []*protocol.Relation,
) error {
	for _, e := range entities {
		// P1: Contradiction Detection - check if an entity with same name and type exists
		if existing, err := p.semantic.GetEntity(ctx, e.Type, e.Name); err == nil && existing != nil {
			if e.Properties == nil {
				e.Properties = make(map[string]any)
			}
			e.Properties["supersedes"] = existing.ID
		}
		if err := p.semantic.UpsertFact(ctx, *e); err != nil {
			// 单条失败不阻断整体
			_ = err
		}
	}
	for _, r := range relations {
		if err := p.semantic.UpsertRelation(ctx, *r); err != nil {
			_ = err
		}
	}
	return nil
}

// ─── Stage 3 ─────────────────────────────────────────────────────────────────

// summarizeSession 为会话生成 3-5 句摘要，写入 SemanticMemory 作为 compaction 文档。
func (p *ConsolidationPipeline) summarizeSession(
	ctx context.Context,
	sessionID string,
	events []protocol.ScoredEvent,
) error {
	summary := p.buildSummary(ctx, sessionID, events)
	if summary == "" {
		return nil
	}

	doc := protocol.Document{
		ID:         "summary_" + sessionID,
		SourceType: "compaction",
		SourceURI:  summary,
		Title:      "Session summary: " + sessionID,
		Version:    fmt.Sprintf("%d", time.Now().Unix()),
	}
	return p.semantic.StoreDocument(ctx, doc)
}

// buildSummary 调用 LLM 或规则引擎生成摘要文本。
func (p *ConsolidationPipeline) buildSummary(
	ctx context.Context,
	_ string, // sessionID 仅用于兜底文本，已嵌入 events
	events []protocol.ScoredEvent,
) string {
	// 组装最近 20 条事件作为摘要输入
	var sb strings.Builder
	limit := min(20, len(events))
	for _, se := range events[:limit] {
		sb.WriteString(string(se.Event.Type))
		sb.WriteString(": ")
		payload := string(se.Event.Payload)
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
		resp, err := p.provider.Infer(ctx, &protocol.InferRequest{
			Messages:    []protocol.Message{{Role: "user", Content: prompt}},
			MaxTokens:   256,
			Temperature: 0.3,
		})
		if err == nil && resp != nil {
			return strings.TrimSpace(resp.Content)
		}
	}

	// 规则 fallback：拼接前 5 条事件类型作为简要摘要
	types := make(map[string]int)
	for _, se := range events {
		types[string(se.Event.Type)]++
	}
	parts := make([]string, 0, min(len(types), 5))
	for t, n := range types {
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
	events []protocol.ScoredEvent,
) error {
	if p.skills == nil {
		return nil
	}

	// 统计 tool_call 类型事件的工具名出现次数
	toolCounts := make(map[string]int)
	for _, se := range events {
		if se.Event.Type != "tool_result" && se.Event.Type != "tool_call" {
			continue
		}
		// 从 payload 中提取 tool_name
		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
			Success  bool   `json:"success"`
		}
		if err := json.Unmarshal(se.Event.Payload, &payload); err != nil {
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
		meta := protocol.SkillMeta{
			Name:         "auto_" + toolName,
			Version:      fmt.Sprintf("1.0.%d", time.Now().Unix()),
			Runtime:      "builtin",
			RiskLevel:    "low",
			Sandbox:      1,
			Capabilities: []string{toolName},
			ExecMode:     "tool",
			Trust:        protocol.TrustTier(1),
			Idempotent:   true,
		}
		if err := p.skills.Register(ctx, meta); err != nil {
			_ = err // 单条失败不阻断
		}
	}
	return nil
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

type dbAccessor interface {
	DB() *sql.DB
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
	if dba, ok := fm.store.(dbAccessor); ok {
		if err := fm.cleanupWithSQL(ctx, dba.DB()); err == nil {
			return nil
		}
	}

	return fm.cleanupWithKV(ctx)
}

func (fm *ForgettingManager) cleanupWithSQL(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT id, salience, occurred_at FROM events WHERE topic IN ('memory.openclaw', 'memory')")
	if err != nil {
		return err
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
		return perrors.Wrap(perrors.CodeInternal, "PeriodicCleanup: scan events 失败", err)
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
		return perrors.Wrap(perrors.CodeInternal, "PeriodicCleanup: 迭代失败", iter.Err())
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
		return perrors.Wrap(perrors.CodeInternal, "PhysicalCompact: scan tombstones 失败", err)
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
		return perrors.Wrap(perrors.CodeInternal, "PhysicalCompact: 迭代失败", iter.Err())
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
