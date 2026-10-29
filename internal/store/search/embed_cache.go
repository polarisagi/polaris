package search

import (
	"sync"
	"time"
)

// 交互路径查询向量的短时有界缓存（ADR-0105 决策四"查询 embedding 在回合内缓存"）。
//
// 一个回合里 episodic 检索与 RAG 检索经同一个 SyncBatcherAdapter 各自嵌入查询文本，
// 意图与 Goal 相同（短问题常见）或阶段重试时是同一段文本被重复计算。
// 向量是文本的纯函数，缓存不改变语义；但嵌入模型可能在线切换（Blue-Green 重嵌），
// 故 TTL 取回合时间尺度的 60s：过期即重算，切换后的陈旧窗口有界。

const (
	// embedCacheMax 最大条数。每条 ≤ 1536 维 × 4B ≈ 6KB，上限约 200KB。
	embedCacheMax = 32
	// embedCacheTTL 单条存活时长。
	embedCacheTTL = 60 * time.Second
)

type embedCacheEntry struct {
	vec      []float32
	storedAt time.Time
}

// queryEmbedCache 按文本键控的 FIFO 有界缓存。不缓存空结果（失败/放弃不应被固化）。
type queryEmbedCache struct {
	mu    sync.Mutex
	items map[string]embedCacheEntry
	order []string // 插入顺序，满时淘汰最旧
	now   func() time.Time
}

func newQueryEmbedCache() *queryEmbedCache {
	return &queryEmbedCache{items: make(map[string]embedCacheEntry, embedCacheMax), now: time.Now}
}

// get 命中返回向量副本（调用方可能就地修改，如归一化）。
func (c *queryEmbedCache) get(text string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.items[text]
	if !ok {
		return nil, false
	}
	if c.now().Sub(ent.storedAt) >= embedCacheTTL {
		delete(c.items, text)
		for i, k := range c.order { // 同步移出淘汰队列，避免陈旧键挤掉存活条目
			if k == text {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		return nil, false
	}
	return append([]float32(nil), ent.vec...), true
}

func (c *queryEmbedCache) put(text string, vec []float32) {
	if len(vec) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[text]; !exists {
		if len(c.order) >= embedCacheMax {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.items, oldest)
		}
		c.order = append(c.order, text)
	}
	c.items[text] = embedCacheEntry{vec: append([]float32(nil), vec...), storedAt: c.now()}
}
