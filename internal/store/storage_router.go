package store

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// StorageRouter — 统一存储路由（引擎选择 + SQLite 兜底）。
// 三轴架构: [Storage-SQLite](控制轴) + [Storage-SurrealDB-Core](认知轴) + [Storage-Native](热缓存)
// 架构文档: docs/arch/M02-Storage-Fabric.md §1.2
type StorageRouter struct {
	stores   map[string]protocol.Store
	rules    []RouteRule
	fallback protocol.Store // 默认 [Storage-SQLite]
}

// RouteRule 路由规则（按优先级排序）。
type RouteRule struct {
	Match       func(req *StorageRequest) bool
	TargetStore string
	Priority    int
}

// StorageRequest 存储请求。
type StorageRequest struct {
	DataType   string // session_state | embedding | event_log | skill_cache | graph | fulltext | metadata
	AccessMode string // random_rw | batch_write | append_only | high_freq_read | knn_read | adhoc_query | graph_traverse
	Key        []byte
}

// Route 按优先级遍历规则 → Match 命中 → stores[rule.TargetStore]。
// 全部未命中 → fallback ([Storage-SQLite]).
func (sr *StorageRouter) Route(ctx context.Context, req *StorageRequest) protocol.Store {
	for _, rule := range sr.rules {
		if rule.Match(req) {
			if store, ok := sr.stores[rule.TargetStore]; ok {
				return store
			}
		}
	}
	return sr.fallback
}

// NewStorageRouter 构造路由器。surreal 通常非 nil（kv-mem 任意机器可用）；
// 仅在 FFI 加载失败时为 nil，此时所有请求回落 SQLite。
func NewStorageRouter(sqlite protocol.Store, surreal protocol.Store) *StorageRouter {
	stores := map[string]protocol.Store{"sqlite": sqlite}
	if surreal != nil {
		stores["surreal"] = surreal
	}
	rules := BuildRouteTable()
	if surreal == nil {
		rules = nil
	}
	return &StorageRouter{
		stores:   stores,
		rules:    rules,
		fallback: sqlite,
	}
}

// GetPrimary returns the raw *sql.DB handle of the primary (fallback) store, if available.
func (sr *StorageRouter) GetPrimary() (*sql.DB, error) {
	type dbProvider interface {
		DB() *sql.DB
	}
	if p, ok := sr.fallback.(dbProvider); ok {
		return p.DB(), nil
	}
	return nil, apperr.New(apperr.CodeInternal, "primary store does not support direct DB access")
}

// BuildRouteTable 生成路由规则表。
// 向量/图/全文 → [Storage-SurrealDB-Core]；其余 → [Storage-SQLite]。
// 路由对齐 docs/arch/M02-Storage-Fabric.md §1.3 路由矩阵。
func BuildRouteTable() []RouteRule {
	return []RouteRule{
		{
			// HNSW 向量近邻检索 → SurrealDB-Core
			Match:       func(req *StorageRequest) bool { return req.AccessMode == "knn_read" },
			TargetStore: "surreal",
			Priority:    1,
		},
		{
			// 知识图谱遍历 → SurrealDB-Core
			Match:       func(req *StorageRequest) bool { return req.AccessMode == "graph_traverse" || req.DataType == "graph" },
			TargetStore: "surreal",
			Priority:    2,
		},
		{
			// 知识片段存储 (BM25 / Vector) → SurrealDB-Core
			Match: func(req *StorageRequest) bool {
				return req.AccessMode == "adhoc_query" || req.DataType == "fulltext" || req.DataType == "knowledge"
			},
			TargetStore: "surreal",
			Priority:    3,
		},
		{
			// Embedding 存储 → SurrealDB-Core
			Match:       func(req *StorageRequest) bool { return req.DataType == "embedding" },
			TargetStore: "surreal",
			Priority:    4,
		},
	}
}
