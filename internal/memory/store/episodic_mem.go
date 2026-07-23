package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxEpisodicEvents Tier0 内存事件容量上限（防止 8GB 场景 OOM）。
// 超出时 FIFO 淘汰最旧内存条目；SQLite 侧保留完整历史，不受此限制。
const maxEpisodicEvents = 2000

// maxEpisodicPayloadBytes kv_store 单条 episodic 事件 Payload 的最大字节数。
// 超限部分落盘到 ~/.polarisagi/polaris/logs/events/ 并替换为 log_ref 占位符，
// 保留前 512 字节作为 BM25 可搜索摘要。
const maxEpisodicPayloadBytes = 8192

type EpisodicIndexer interface {
	Index(ctx context.Context, ev types.Event)
}

// EpisodicMem (L1) — 事件表 + 向量投影。
type EpisodicMem struct {
	store     protocol.Store
	events    []types.Event
	mu        sync.RWMutex
	indexer   EpisodicIndexer            // Tier1+：图索引器，nil 时跳过
	cognitive protocol.CognitiveSearcher // Tier1+：SurrealDB FTS 索引写入，nil 时跳过
	maxEvents int                        // 内存事件容量上限，0 表示不限制
	vfsWriter BlobOverflowWriter         // 可选注入；nil 时降级（见 episodic_mem_overflow.go）
}

func NewEpisodicMem(store protocol.Store) *EpisodicMem {
	return &EpisodicMem{
		store:     store,
		events:    make([]types.Event, 0, 256),
		maxEvents: maxEpisodicEvents,
	}
}

// 2026-07-14（ADR-0051）：NewEpisodicMemWithGraph 删除——全仓零生产调用点。
// 唯一调用方 NewMemImplWithGraph 已同批删除（graph-without-cognitive 是幽灵
// Tier 档位，见 memory.go）。生产唯一使用 NewEpisodicMem（Tier0）/
// NewEpisodicMemWithCognitive（Tier1+，indexer+cognitive 同时注入）。

// NewEpisodicMemWithCognitive 创建含 SurrealDB FTS 索引路径的 EpisodicMem（Tier1+）。
// 每次 Append 同步写入 SurrealDB FTS 倒排索引；VecUpsert 由 OnlineReindexer 异步完成。
func NewEpisodicMemWithCognitive(store protocol.Store, indexer EpisodicIndexer, cognitive protocol.CognitiveSearcher) *EpisodicMem {
	return &EpisodicMem{
		store:     store,
		events:    make([]types.Event, 0, 256),
		indexer:   indexer,
		cognitive: cognitive,
		maxEvents: maxEpisodicEvents,
	}
}

func (em *EpisodicMem) Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error {
	ev.TaintLevel = types.PropagateTaint(ev.TaintLevel, taint) // only-up：取 max，禁降级
	em.mu.Lock()
	defer em.mu.Unlock()

	// Payload 门控：超限落盘 + log_ref 替换
	if len(ev.Payload) > maxEpisodicPayloadBytes {
		ev.Payload = em.truncateEpisodicPayload(ev.ID, ev.Payload)
	}

	key := []byte("episodic:" + ev.ID)
	data, err := json.Marshal(ev)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EpisodicMem.Append", err)
	}
	if err := em.store.Put(ctx, key, data); err != nil {
		// 存储层写入失败（非序列化失败）单独归类为 CodeStorageUnavailable，
		// 供 Agent.writeEpisodicWithExtract 识别为熔断信号（GD-13-003）。
		return apperr.Wrap(apperr.CodeStorageUnavailable, "EpisodicMem.Append", err)
	}

	// 容量门控：超过 maxEvents 时 FIFO 淘汰最旧内存条目（SQLite 侧不受影响）
	em.events = append(em.events, ev)
	if em.maxEvents > 0 && len(em.events) > em.maxEvents {
		em.events = em.events[len(em.events)-em.maxEvents:]
	}

	// 图索引：将事件节点与代理/会话建立关联边（Tier1+，nil 时跳过）
	if em.indexer != nil {
		em.indexer.Index(ctx, ev)
	}
	// SurrealDB FTS 同步索引（Tier1+）；失败不阻断写入，仅降级到 Tier0 BM25 路径
	if em.cognitive != nil {
		payload := string(ev.Payload)
		if payload != "" {
			_ = em.cognitive.FTSIndex(ev.ID, payload)
		}
	}
	return nil
}

func (em *EpisodicMem) Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) { //nolint:gocyclo
	em.mu.RLock()
	var events []types.Event
	if len(em.events) > 0 {
		events = make([]types.Event, len(em.events))
		copy(events, em.events)
	}
	em.mu.RUnlock()

	// 重启后内存列表为空时从持久化存储按前缀扫描恢复
	if len(events) == 0 {
		var err error
		events, err = em.loadEventsFromStore(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "EpisodicMem.Query", err)
		}
	}

	var results []types.ScoredEvent //nolint:prealloc
	for _, ev := range events {
		if q.SessionID != "" && ev.TaskID != q.SessionID {
			continue
		}
		if ev.TaintLevel > q.MaxTaintLevel { // 超过请求上限 → 过滤
			continue
		}
		score := 1.0
		// 语义文本匹配（Topics 或 Semantic 关键词）
		payload := string(ev.Payload)
		if len(q.Topics) > 0 {
			match := false
			for _, topic := range q.Topics {
				if strings.Contains(payload, topic) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		if q.Semantic != "" && !strings.Contains(payload, q.Semantic) {
			continue
		}
		// 深拷贝 Payload/ReasoningState：ev 来自 em.events 内部切片浅拷贝
		// （Query 顶部 copy(events, em.events)），[]byte 字段仍与内部缓存共享
		// 底层数组。调用方若修改返回的 Event.Payload，会直接污染内部缓存并
		// 引发并发数据竞争（GR-5-002）。
		evCopy := ev
		if ev.Payload != nil {
			evCopy.Payload = append([]byte(nil), ev.Payload...)
		}
		if ev.ReasoningState != nil {
			evCopy.ReasoningState = append([]byte(nil), ev.ReasoningState...)
		}
		results = append(results, types.ScoredEvent{Event: &evCopy, Score: score})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if q.K > 0 && len(results) > q.K {
		results = results[:q.K]
	}
	return results, nil
}

// Consolidate 将高频相似事件压缩蒸馏到 SemanticMem。
// 触发条件: EpisodicMem 事件数 >= consolidateThreshold（当前: 20）。
// 算法:
//  1. 按 TaskType(EventType) 聚类
//  2. 同类事件 >= 3 条，且两两 Simhash 距离 <= 8
//  3. 取最新 3 条合并摘要写入 SemanticMem
//  4. 原始事件打 consolidated=true 标记（不删除，保留审计链）
func (em *EpisodicMem) Consolidate(ctx context.Context, semantic *SemanticMem) error {
	em.mu.RLock()
	events := make([]types.Event, len(em.events))
	copy(events, em.events)
	em.mu.RUnlock()

	if len(events) < 3 {
		return nil
	}

	// 按 EventType 聚类（EventType 是 string defined type，显式转换）
	groups := make(map[string][]types.Event)
	for _, ev := range events {
		groups[string(ev.Type)] = append(groups[string(ev.Type)], ev)
	}

	for evType, evs := range groups {
		if len(evs) < 3 {
			continue
		}
		// 取最新 3 条做 Simhash 相似验证
		recent := evs
		if len(recent) > 3 {
			recent = recent[len(recent)-3:]
		}
		fp0 := util.SimhashOf(string(recent[0].Payload))
		fp1 := util.SimhashOf(string(recent[1].Payload))
		fp2 := util.SimhashOf(string(recent[2].Payload))
		if !util.IsSimilar(fp0, fp1) && !util.IsSimilar(fp1, fp2) {
			continue // 不够相似，跳过合并
		}

		// 构造合并摘要
		summary := ""
		for _, ev := range recent {
			payload := string(ev.Payload)
			if len(payload) > 200 {
				payload = payload[:200]
			}
			summary += payload + " | "
		}
		docID := "consolidated_" + evType + "_" + recent[len(recent)-1].ID
		doc := types.Document{
			ID:         docID,
			Title:      "Consolidated: " + evType,
			SourceType: "episodic",
			SourceURI:  summary, // 摘要存入 SourceURI（Document 无 Content 字段）
		}
		if semantic != nil {
			_ = semantic.StoreDocument(ctx, doc, types.TaintNone)
		}
	}
	return nil
}

// MarkCold 找出 before 时间点之前的 active 事件，并将其冷冻（archived=1）。
// 返回更新的记录数。
func (em *EpisodicMem) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	if em.store == nil {
		return 0, nil
	}

	sqlStore, ok := em.store.(protocol.SQLQuerier)
	if !ok {
		return 0, nil
	}

	query := "UPDATE episodic_events SET archived = 1 WHERE session_id = ? AND timestamp < ? AND archived = 0"
	result, err := sqlStore.ExecContext(ctx, query, sessionID, before.Unix())
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "episodic_mem: mark cold failed", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		slog.WarnContext(ctx, "episodic_mem: RowsAffected failed", "error", err)
		return 0, nil
	}

	// 同步清理 KV 中仍未被 OutboxWorker 投影但已过期的事件。
	// 避免 SQL-only UPDATE 遗漏仍在 KV 的新近事件。
	if affected >= 0 { // affected >= 0 保证 SQL 步骤已执行
		em.mu.Lock()
		filtered := em.events[:0]
		var toDelete []string
		for _, ev := range em.events {
			if !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(before) && ev.TaskID == sessionID {
				toDelete = append(toDelete, ev.ID)
			} else {
				filtered = append(filtered, ev)
			}
		}
		em.events = filtered
		em.mu.Unlock()

		for _, id := range toDelete {
			// 从 KV 删除该事件（best-effort，失败不阻断）
			_ = em.store.Delete(ctx, []byte("episodic:"+id))
		}
	}

	if affected > 0 {
		insertLog := `INSERT INTO episodic_events_change_log
			(session_id, changed_at, change_type, affected_count)
			VALUES (?, ?, 'mark_cold', ?)`
		if _, err := sqlStore.ExecContext(ctx, insertLog, sessionID, time.Now().Unix(), affected); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "episodic_mem: write change_log failed", err)
		}
	}

	return int(affected), nil
}

// ScanHighSalience 扫描 episodic_events 物化表中的高显著性事件（archived=0 且 salience >= 阈值）。
// sinceID 为高水位标记，只返回 id > sinceID 的事件，按 id 升序、limit 截断。
// 供后台维护 Agent（swarm.MemoryAgent）生成耳语提示，取代其对本包的直接 SQL 访问。
// store 未实现 protocol.SQLQuerier（无 SQLite 后端）时静默返回空结果。
func (em *EpisodicMem) ScanHighSalience(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error) {
	if em.store == nil {
		return nil, nil
	}
	sqlStore, ok := em.store.(protocol.SQLQuerier)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := sqlStore.QueryContext(ctx, `
		SELECT id, session_id, content, salience, COALESCE(occurred_at, timestamp)
		FROM episodic_events
		WHERE archived = 0 AND salience >= ? AND id > ?
		ORDER BY id ASC LIMIT ?
	`, minSalience, sinceID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "episodic_mem: scan high salience failed", err)
	}
	defer rows.Close()

	var results []types.SalienceEvent //nolint:prealloc
	for rows.Next() {
		var e types.SalienceEvent
		if scanErr := rows.Scan(&e.ID, &e.SessionID, &e.Content, &e.Salience, &e.OccurredAt); scanErr != nil {
			continue
		}
		results = append(results, e)
	}
	return results, nil
}

func (em *EpisodicMem) loadEventsFromStore(ctx context.Context) ([]types.Event, error) {
	iter, err := em.store.Scan(ctx, []byte("episodic:"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "EpisodicMem.loadEventsFromStore", err)
	}

	var loaded []types.Event
	for iter.Next() {
		var ev types.Event
		if jsonErr := json.Unmarshal(iter.Value(), &ev); jsonErr == nil {
			loaded = append(loaded, ev)
		}
	}
	iter.Close()

	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.events) == 0 { // double check
		em.events = append(em.events, loaded...)
		if em.maxEvents > 0 && len(em.events) > em.maxEvents {
			em.events = em.events[len(em.events)-em.maxEvents:]
		}
	}
	events := make([]types.Event, len(em.events))
	copy(events, em.events)
	return events, nil
}

func (em *EpisodicMem) Forget(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	
	em.mu.Lock()
	var kept []types.Event
	var toDelete []string
	for _, ev := range em.events {
		if !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(cutoff) {
			toDelete = append(toDelete, ev.ID)
		} else {
			kept = append(kept, ev)
		}
	}
	em.events = kept
	em.mu.Unlock()

	for _, id := range toDelete {
		_ = em.store.Delete(ctx, []byte("episodic:"+id))
	}
	return len(toDelete), nil
}
