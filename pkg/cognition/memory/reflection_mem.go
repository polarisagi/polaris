package memory

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// ReflectionMemory (Mem-L1.5) — 元认知反思层
// 架构文档: docs/arch/M05-Memory-System.md §3.4
// ============================================================================

// ReflectionMem 元认知反思层实现。
// 持久化到 store（key 前缀 "reflection:"），内存缓存加速最近查询。
type ReflectionMem struct {
	store   protocol.Store
	entries []protocol.ReflectionEntry
	mu      sync.Mutex
}

func NewReflectionMem(store protocol.Store) *ReflectionMem {
	return &ReflectionMem{
		store:   store,
		entries: make([]protocol.ReflectionEntry, 0),
	}
}

func (rm *ReflectionMem) AppendReflection(ctx context.Context, entry protocol.ReflectionEntry) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rm.ensureLoaded(ctx)

	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.Meta == nil {
		entry.Meta = make(map[string]any)
	}
	// For LRU eviction
	entry.Meta["last_accessed_at"] = time.Now().UnixNano()

	// HT0 limit: 5000 entries max (approx 5MB)
	if len(rm.entries) >= 5000 {
		rm.evictLRU(ctx)
	}

	key := []byte("reflection:" + entry.ID)
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := rm.store.Put(ctx, key, data); err != nil {
		return err
	}
	rm.entries = append(rm.entries, entry)
	return nil
}

func (rm *ReflectionMem) ensureLoaded(ctx context.Context) {
	if len(rm.entries) == 0 && rm.store != nil {
		iter, err := rm.store.Scan(ctx, []byte("reflection:"))
		if err == nil && iter != nil {
			for iter.Next() {
				var e protocol.ReflectionEntry
				if jsonErr := json.Unmarshal(iter.Value(), &e); jsonErr == nil {
					rm.entries = append(rm.entries, e)
				}
			}
			iter.Close()
		}
	}
}

func (rm *ReflectionMem) evictLRU(ctx context.Context) {
	if len(rm.entries) == 0 {
		return
	}
	oldestIdx := 0
	oldestTime := int64(^uint64(0) >> 1) // MaxInt64
	for i, e := range rm.entries {
		var la int64
		if e.Meta != nil {
			switch v := e.Meta["last_accessed_at"].(type) {
			case int64:
				la = v
			case float64:
				la = int64(v)
			}
		}
		if la < oldestTime {
			oldestTime = la
			oldestIdx = i
		}
	}

	key := []byte("reflection:" + rm.entries[oldestIdx].ID)
	if rm.store != nil {
		_ = rm.store.Delete(ctx, key)
	}

	rm.entries = append(rm.entries[:oldestIdx], rm.entries[oldestIdx+1:]...)
}

func (rm *ReflectionMem) QueryReflections(ctx context.Context, q protocol.ReflectionQuery) ([]protocol.ReflectionEntry, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rm.ensureLoaded(ctx)

	var results []protocol.ReflectionEntry //nolint:prealloc
	now := time.Now()
	for i, e := range rm.entries {
		if q.SessionID != "" && e.SessionID != q.SessionID {
			continue
		}
		if q.AgentID != "" && e.AgentID != q.AgentID {
			continue
		}

		// Update LRU metrics
		if rm.entries[i].Meta == nil {
			rm.entries[i].Meta = make(map[string]any)
		}
		rm.entries[i].Meta["last_accessed_at"] = now.UnixNano()

		count := 0
		if v, ok := rm.entries[i].Meta["accessed_count"]; ok {
			switch num := v.(type) {
			case float64:
				count = int(num)
			case int:
				count = num
			}
		}
		rm.entries[i].Meta["accessed_count"] = count + 1

		results = append(results, rm.entries[i])
	}

	// 返回最近 K 条（时间降序）
	sort.Slice(results, func(i, j int) bool {
		return results[i].CreatedAt.After(results[j].CreatedAt)
	})
	if q.K > 0 && len(results) > q.K {
		results = results[:q.K]
	}
	return results, nil
}
