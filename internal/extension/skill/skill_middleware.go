package skill

import (
	"sync"
	"time"
)

// ── skillLRUCache ─────────────────────────────────────────────────────────────
// P1-8：Skill 专用幂等缓存，行为对标 internal/tool 包的 lruCache。
// 上限 200 条 + TTL 5min（Skill 执行频率低于 Tool，上限设小一点）。

type skillCacheEntry struct {
	value    []byte
	expireAt time.Time
}

type skillLRUCache struct {
	maxSize int
	ttl     time.Duration
	items   map[string]*skillCacheEntry
	keys    []string // 插入顺序，用于简单 FIFO 淘汰
}

func newSkillLRUCache(maxSize int, ttl time.Duration) *skillLRUCache {
	return &skillLRUCache{
		maxSize: maxSize,
		ttl:     ttl,
		items:   make(map[string]*skillCacheEntry),
		keys:    make([]string, 0, maxSize),
	}
}

func (c *skillLRUCache) get(key string) ([]byte, bool) {
	entry, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expireAt) {
		delete(c.items, key)
		return nil, false
	}
	return entry.value, true
}

func (c *skillLRUCache) set(key string, value []byte) {
	if _, exists := c.items[key]; !exists {
		if len(c.keys) >= c.maxSize {
			// 淘汰最早插入的 key
			oldest := c.keys[0]
			c.keys = c.keys[1:]
			delete(c.items, oldest)
		}
		c.keys = append(c.keys, key)
	}
	c.items[key] = &skillCacheEntry{value: value, expireAt: time.Now().Add(c.ttl)}
}

// ── skillRateLimiter ──────────────────────────────────────────────────────────
// P1-8：Skill 专用令牌桶限流器，行为对标 internal/tool 包的 rateLimiter。

type skillRateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	maxQPS   float64
	lastTime time.Time
}

func newSkillRateLimiter(qps float64) *skillRateLimiter {
	return &skillRateLimiter{
		tokens:   qps,
		maxQPS:   qps,
		lastTime: time.Now(),
	}
}

func (r *skillRateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(r.lastTime).Seconds()
	r.lastTime = now
	r.tokens += elapsed * r.maxQPS
	if r.tokens > r.maxQPS {
		r.tokens = r.maxQPS
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
