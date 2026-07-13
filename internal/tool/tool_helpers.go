package tool

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// isFileWriteTool/isShellTool/isReversible 工具分类判定、lruCache 幂等缓存、
// rateLimiter 令牌桶限速器、ErrToolNotFound 见本文件（R7 拆分自 tool.go；
// InMemoryToolRegistry 核心注册/执行路径见 tool.go）。

// isFileWriteTool 判断是否是文件写操作工具
func isFileWriteTool(t types.Tool) bool {
	if t.Name == "write_file" || t.Name == "str_replace_editor" || t.Name == "multi_edit_file" || t.Name == "notebook_edit" {
		return true
	}
	for _, se := range t.SideEffects {
		if se == types.SideFileWrite {
			return true
		}
	}
	return false
}

// isShellTool 判断工具是否包含 shell/进程副作用（限速 2 QPS）。
func isShellTool(t types.Tool) bool {
	for _, se := range t.SideEffects {
		if se == types.SideProcessSpawn {
			return true
		}
	}
	return false
}

// hasNetworkEgressSideEffect 判断工具是否声明 SideNetworkCall 副作用
// （tool.yaml side_effects: [network-call]）——出口污点检查（M04 §3）只对
// 这类工具的入参生效，纯本地/只读工具不受影响。
func hasNetworkEgressSideEffect(t types.Tool) bool {
	for _, se := range t.SideEffects {
		if se == types.SideNetworkCall {
			return true
		}
	}
	return false
}

// isReversible 判断工具副作用是否可逆。
func isReversible(t types.Tool) bool {
	if t.Capability >= types.CapWriteNetwork {
		return false
	}
	for _, se := range t.SideEffects {
		if se == types.SideProcessSpawn || se == types.SideStateMutate {
			return false
		}
	}
	return true
}

// lruCache 极简 LRU 缓存，并发安全，cap 固定上限 + TTL 过期双控。
type lruCache struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	items map[string]*lruEntry
	order []string // 插入顺序（简化版，用 slice 模拟，不用链表）
}

type lruEntry struct {
	value     *types.ToolResult
	expiresAt time.Time
}

func newLRUCache(cap int, ttl time.Duration) *lruCache {
	return &lruCache{cap: cap, ttl: ttl, items: make(map[string]*lruEntry)}
}

func (c *lruCache) get(key string) (*types.ToolResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expiresAt) {
		delete(c.items, key)
		return nil, false
	}
	return e.value, true
}

func (c *lruCache) set(key string, value *types.ToolResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
	}
	// TTL 刷新 / 新增
	c.items[key] = &lruEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
	// 容量控制：超限时按插入顺序淘汰最老 key
	for len(c.items) > c.cap && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

// ─── 简单令牌桶限速器 ───────────────────────────────────────────────────────

type rateLimiter struct {
	tokens   atomic.Int64
	maxQPS   int64
	refillAt atomic.Int64 // unix nano
}

func newRateLimiter(qps int64) *rateLimiter {
	rl := &rateLimiter{maxQPS: qps}
	rl.tokens.Store(qps)
	rl.refillAt.Store(time.Now().Add(time.Second).UnixNano())
	return rl
}

func (rl *rateLimiter) Allow() bool {
	now := time.Now().UnixNano()
	if now >= rl.refillAt.Load() {
		// CAS 保证只有一个 goroutine 执行刷新，避免多次 Store 竞态。
		old := rl.refillAt.Load()
		if rl.refillAt.CompareAndSwap(old, now+int64(time.Second)) {
			rl.tokens.Store(rl.maxQPS)
		}
	}
	// Add(-1) 是原子操作；负值代表本窗口超限，不还回——避免多 goroutine 同时还回导致误放行。
	return rl.tokens.Add(-1) >= 0
}

// ErrToolNotFound 工具未注册时返回的哨兵错误。
var ErrToolNotFound = apperr.New(apperr.CodeInternal, "tool not found")

func (r *InMemoryToolRegistry) checkIdempotency(ctx context.Context) (*types.ToolResult, bool, string) {
	if key, ok := ctx.Value(protocol.CtxIdempotencyKey{}).(types.IdempotencyKey); ok && key != "" {
		idempotencyKey := string(key)
		if cachedResult, exists := r.idempotencyCache.get(idempotencyKey); exists {
			slog.Debug("tool_registry: returning cached result for idempotency key", "key", idempotencyKey)
			return cachedResult, true, idempotencyKey
		}
		return nil, false, idempotencyKey
	}
	return nil, false, ""
}

func (r *InMemoryToolRegistry) cacheIdempotencyResult(key string, result *types.ToolResult) {
	if key != "" && result.Success {
		r.idempotencyCache.set(key, result)
	}
}
