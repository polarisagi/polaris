package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
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

// 2026-07-14（ADR-0062）：NewEpisodicMemWithGraph 删除——全仓零生产调用点。
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

	em.mu.Lock()
	// 容量门控：超过 maxEvents 时 FIFO 淘汰最旧内存条目（SQLite 侧不受影响）
	em.events = append(em.events, ev)
	if em.maxEvents > 0 && len(em.events) > em.maxEvents {
		em.events = em.events[len(em.events)-em.maxEvents:]
	}
	em.mu.Unlock()

	if em.indexer != nil || em.cognitive != nil {
		concurrent.SafeGo(context.WithoutCancel(ctx), "episodic_mem.async_index", func(gctx context.Context) {
			// 图索引：将事件节点与代理/会话建立关联边（Tier1+，nil 时跳过）
			if em.indexer != nil {
				em.indexer.Index(gctx, ev)
			}
			em.ftsIndexAsync(gctx, ev)
		})
	}
	return nil
}

// ftsIndexAsync 执行 SurrealDB FTS 同步索引（Tier1+）；失败不阻断写入，仅
// 降级到 Tier0 BM25 路径。从 Append 的异步闭包中抽出以降低嵌套深度
// （golangci-lint nestif 阈值），语义与原内联逻辑完全一致。
// L2：语义正确（不阻断）但此前无可观测性——索引持续丢失是渐进性检索质量
// 退化，不是一次性事件，Debug 级不够，必须 Warn + counter。
func (em *EpisodicMem) ftsIndexAsync(gctx context.Context, ev types.Event) {
	if em.cognitive == nil {
		return
	}
	payload := string(ev.Payload)
	if payload == "" {
		return
	}
	if err := em.cognitive.FTSIndex(ev.ID, payload); err != nil {
		slog.WarnContext(gctx, "memory/episodic_mem: FTS 索引写入失败，降级至 Tier0 BM25 检索", "event_id", ev.ID, "err", err)
		metrics.GlobalMemoryFTSIndexFailuresTotal.Add(1)
	}
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
