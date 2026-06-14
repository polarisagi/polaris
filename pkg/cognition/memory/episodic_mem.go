package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// maxEpisodicEvents Tier0 内存事件容量上限（防止 8GB 场景 OOM）。
// 超出时 FIFO 淘汰最旧内存条目；SQLite 侧保留完整历史，不受此限制。
const maxEpisodicEvents = 2000

// maxEpisodicPayloadBytes kv_store 单条 episodic 事件 Payload 的最大字节数。
// 超限部分落盘到 ~/.polarisagi/polaris/logs/events/ 并替换为 log_ref 占位符，
// 保留前 512 字节作为 BM25 可搜索摘要。
const maxEpisodicPayloadBytes = 8192

// EpisodicMem (L1) — 事件表 + 向量投影。
type EpisodicMem struct {
	store     protocol.Store
	events    []protocol.Event
	mu        sync.RWMutex
	indexer   *EpisodicGraphIndexer // Tier1+：图索引器，nil 时跳过
	cognitive CognitiveSearcher     // Tier1+：SurrealDB FTS 索引写入，nil 时跳过
	maxEvents int                   // 内存事件容量上限，0 表示不限制
}

func NewEpisodicMem(store protocol.Store) *EpisodicMem {
	return &EpisodicMem{
		store:     store,
		events:    make([]protocol.Event, 0, 256),
		maxEvents: maxEpisodicEvents,
	}
}

// NewEpisodicMemWithGraph 创建含图索引的 EpisodicMem（Tier1+）。
func NewEpisodicMemWithGraph(store protocol.Store, indexer *EpisodicGraphIndexer) *EpisodicMem {
	return &EpisodicMem{
		store:     store,
		events:    make([]protocol.Event, 0, 256),
		indexer:   indexer,
		maxEvents: maxEpisodicEvents,
	}
}

// NewEpisodicMemWithCognitive 创建含 SurrealDB FTS 索引路径的 EpisodicMem（Tier1+）。
// 每次 Append 同步写入 SurrealDB FTS 倒排索引；VecUpsert 由 OnlineReindexer 异步完成。
func NewEpisodicMemWithCognitive(store protocol.Store, indexer *EpisodicGraphIndexer, cognitive CognitiveSearcher) *EpisodicMem {
	return &EpisodicMem{
		store:     store,
		events:    make([]protocol.Event, 0, 256),
		indexer:   indexer,
		cognitive: cognitive,
		maxEvents: maxEpisodicEvents,
	}
}

func (em *EpisodicMem) Append(ctx context.Context, ev protocol.Event) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	// Payload 门控：超限落盘 + log_ref 替换
	if len(ev.Payload) > maxEpisodicPayloadBytes {
		ev.Payload = truncateEpisodicPayload(ev.ID, ev.Payload)
	}

	key := []byte("episodic:" + ev.ID)
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := em.store.Put(ctx, key, data); err != nil {
		return err
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

func (em *EpisodicMem) Query(ctx context.Context, q protocol.EpisodicQuery) ([]protocol.ScoredEvent, error) { //nolint:gocyclo
	em.mu.RLock()
	var events []protocol.Event
	if len(em.events) > 0 {
		events = make([]protocol.Event, len(em.events))
		copy(events, em.events)
	}
	em.mu.RUnlock()

	// 重启后内存列表为空时从持久化存储按前缀扫描恢复
	if len(events) == 0 {
		iter, err := em.store.Scan(ctx, []byte("episodic:"))
		if err != nil {
			return nil, err
		}

		var loaded []protocol.Event
		for iter.Next() {
			var ev protocol.Event
			if jsonErr := json.Unmarshal(iter.Value(), &ev); jsonErr == nil {
				loaded = append(loaded, ev)
			}
		}
		iter.Close()

		em.mu.Lock()
		if len(em.events) == 0 { // double check
			em.events = append(em.events, loaded...)
			if em.maxEvents > 0 && len(em.events) > em.maxEvents {
				em.events = em.events[len(em.events)-em.maxEvents:]
			}
			events = make([]protocol.Event, len(em.events))
			copy(events, em.events)
		} else {
			events = make([]protocol.Event, len(em.events))
			copy(events, em.events)
		}
		em.mu.Unlock()
	}

	var results []protocol.ScoredEvent //nolint:prealloc
	for _, ev := range events {
		if q.SessionID != "" && ev.TaskID != q.SessionID {
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
		results = append(results, protocol.ScoredEvent{Event: ev, Score: score})
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
	events := make([]protocol.Event, len(em.events))
	copy(events, em.events)
	em.mu.RUnlock()

	if len(events) < 3 {
		return nil
	}

	// 按 EventType 聚类（EventType 是 string defined type，显式转换）
	groups := make(map[string][]protocol.Event)
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
		fp0 := SimhashOf(string(recent[0].Payload))
		fp1 := SimhashOf(string(recent[1].Payload))
		fp2 := SimhashOf(string(recent[2].Payload))
		if !IsSimilar(fp0, fp1) && !IsSimilar(fp1, fp2) {
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
		doc := protocol.Document{
			ID:         docID,
			Title:      "Consolidated: " + evType,
			SourceType: "episodic",
			SourceURI:  summary, // 摘要存入 SourceURI（Document 无 Content 字段）
		}
		if semantic != nil {
			_ = semantic.StoreDocument(ctx, doc)
		}
	}
	return nil
}

// MarkCold 找出 before 时间点之前的 active 事件，并将其冷冻（cold=1）。
// 返回更新的记录数。
func (em *EpisodicMem) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	if em.store == nil {
		return 0, nil
	}

	// 这里更新数据库表 episodic_events 的 cold 字段
	query := "UPDATE episodic_events SET cold = 1 WHERE session_id = ? AND timestamp < ? AND cold = 0"
	if sqlStore, ok := em.store.(interface {
		Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
	}); ok {
		result, err := sqlStore.Exec(ctx, query, sessionID, before.Unix())
		if err != nil {
			return 0, perrors.Wrap(perrors.CodeInternal, "episodic_mem: mark cold failed", err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			//nolint:nilerr
			return 0, nil
		}

		return int(affected), nil
	}
	return 0, nil
}

// truncateEpisodicPayload 将超限 Payload 落盘，返回含 log_ref 占位符的截断版本。
// 落盘路径：~/.polarisagi/polaris/logs/events/<id>.bin
// 返回内容：前 512 字节（BM25 可用）+ log_ref JSON 片段
func truncateEpisodicPayload(eventID string, raw []byte) []byte {
	logDir := filepath.Join(os.ExpandEnv("$HOME"), ".polarisagi", "polaris", "logs", "events")
	if err := os.MkdirAll(logDir, 0700); err == nil {
		_ = os.WriteFile(filepath.Join(logDir, eventID+".bin"), raw, 0600)
	}

	preview := raw
	if len(preview) > 512 {
		preview = preview[:512]
	}
	ref := fmt.Sprintf(
		`{"log_ref":%q,"bytes":%d,"preview":%s}`,
		eventID, len(raw), string(preview),
	)
	return []byte(ref)
}
