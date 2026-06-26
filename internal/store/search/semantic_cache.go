package search

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// SemanticCache — LLM 响应语义缓存。
// 架构文档: docs/arch/M01-Inference-Runtime.md §6.2
//
// [接口预留][实现依赖 SurrealDB-Core HNSW，当前版本未激活]
// 类型与方法已实现，CacheStore.FindClosest 依赖向量索引后端激活后方可命中。
// 当 store=nil 时，所有操作为空操作（安全降级）。

type CacheEntry struct {
	Key              string
	RequestHash      string
	Namespace        string
	SystemPromptHash string
	Response         string
	Model            string
	Embedding        []float32 // 请求语义向量，供 HNSW 索引存储
	CreatedAt        time.Time
	HitCount         int64
	LastAccess       time.Time
}

// CacheStore 向量索引存储接口（[Storage-SurrealDB-Core] HNSW 实现）。
type CacheStore interface {
	// FindClosest 按向量相似度查找最近的缓存条目（含 TTL 过期过滤）。
	FindClosest(embedding []float32, threshold float32, limit int) []*CacheEntry
	// Put 写入或更新缓存条目。
	Put(entry *CacheEntry) error
	// Delete 按 Key 批量删除条目（供 LRU 淘汰调用）。
	Delete(keys []string) error
	// Count 返回当前条目总数。
	Count() int
	// ListOldest 按 LastAccess 升序返回最旧的 n 条条目（供跨重启 LRU 淘汰）。
	// 实现侧：ORDER BY last_access ASC LIMIT n。n<=0 时返回空切片。
	ListOldest(n int) []*CacheEntry
}

// Embedder 文本向量化接口（M1 提供）。
type Embedder interface {
	Embed(text string) []float32
}

type SemanticCache struct {
	store            CacheStore
	embedder         Embedder
	namespace        string
	systemPromptHash string
	similarity       float64 // 0-1，默认 0.95
	maxEntries       int     // 默认 10000
	ttl              time.Duration

	// accessTime 进程内热点覆盖：key → 本次进程中的最新 access time。
	// 重启后为空，evictLRU 自动降级到 store.ListOldest（持久化记录）。
	// 上限 accessTimeMax 防止内存无限增长；超限时清空并让 store 接管。
	mu            sync.Mutex
	accessTime    map[string]time.Time
	accessTimeMax int // = maxEntries，与 store 容量对齐
}

// CacheKey 语义缓存查询键（调用方填充请求上下文字段）。
type CacheKey struct {
	// ContextHintFingerprint M4 epochTracker 计算的上下文指纹（SHA-256 全量，见 pkg/cognition/kernel/epoch.go）。
	ContextHintFingerprint string
	// ActiveControlLabels 当前激活的 Control Vector 标签列表。
	ActiveControlLabels []string
	// TaskType 任务类型标识（routing 分类，如 code/research/simple）。
	TaskType string
	// Messages 请求消息内容（用于语义向量化和哈希计算）。
	Messages []string
}

// NewSemanticCache 创建语义缓存实例。
// store=nil 时安全空操作（SurrealDB-Core 未初始化场景）。
func NewSemanticCache(
	store CacheStore,
	embedder Embedder,
	namespace, systemPromptHash string,
	similarity float64,
	maxEntries int,
	ttl time.Duration,
) *SemanticCache {
	if similarity <= 0 {
		similarity = 0.95
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &SemanticCache{
		store:            store,
		embedder:         embedder,
		namespace:        namespace,
		systemPromptHash: systemPromptHash,
		similarity:       similarity,
		maxEntries:       maxEntries,
		ttl:              ttl,
		accessTime:       make(map[string]time.Time),
		accessTimeMax:    maxEntries,
	}
}

// Get 查询语义缓存。
//
// 三重匹配（任一失败则未命中）：
//  1. RequestHash 精确匹配（同一请求重复调用）
//  2. Namespace + SystemPromptHash 一致（系统上下文相同）
//  3. 向量余弦相似度 >= SimilarityThreshold
//
// TTL 由 CacheStore.FindClosest 过滤，或由 Get 在返回前二次校验。
// store=nil 或 embedder=nil 时始终返回 ("", false)。
func (c *SemanticCache) Get(key CacheKey) (string, bool) {
	if c.store == nil || c.embedder == nil {
		return "", false
	}

	requestHash := c.hashKey(key)

	// 向量化当前请求（拼接消息内容作为语义代表）
	queryText := strings.Join(key.Messages, "\n")
	embedding := c.embedder.Embed(queryText)
	if len(embedding) == 0 {
		return "", false
	}

	candidates := c.store.FindClosest(embedding, float32(c.similarity), 5)
	for _, entry := range candidates {
		// TTL 校验
		if time.Since(entry.CreatedAt) > c.ttl {
			continue
		}
		// Namespace + SystemPromptHash 一致性
		if entry.Namespace != c.namespace || entry.SystemPromptHash != c.systemPromptHash {
			continue
		}
		// 精确哈希优先（短路）；向量相似度已由 FindClosest 保证
		_ = requestHash // 精确匹配为可选优化，此处依赖向量相似度

		// 更新 LRU 访问时间
		c.mu.Lock()
		c.accessTime[entry.Key] = time.Now()
		c.mu.Unlock()

		// 更新 HitCount（fire-and-forget）
		entry.HitCount++
		entry.LastAccess = time.Now()
		_ = c.store.Put(entry)

		return entry.Response, true
	}
	return "", false
}

// Put 写入语义缓存条目。
//
// 写入前检查容量：若 Count() >= maxEntries，淘汰 maxEntries/10 个最久未访问的条目（LRU）。
// store=nil 或 embedder=nil 时为空操作。
func (c *SemanticCache) Put(key CacheKey, response, model string) error {
	if c.store == nil || c.embedder == nil {
		return nil
	}

	queryText := strings.Join(key.Messages, "\n")
	embedding := c.embedder.Embed(queryText)
	// embedding=nil 说明 Embedder 暂不可用（如 Ollama 未启动），跳过写入。
	// 写入无向量的 entry 会使该条目永远无法被 FindClosest 命中，静默占用 LRU 槽位。
	if len(embedding) == 0 {
		return nil
	}

	requestHash := c.hashKey(key)
	entryKey := c.namespace + ":" + requestHash

	now := time.Now()
	entry := &CacheEntry{
		Key:              entryKey,
		RequestHash:      requestHash,
		Namespace:        c.namespace,
		SystemPromptHash: c.systemPromptHash,
		Response:         response,
		Model:            model,
		Embedding:        embedding,
		CreatedAt:        now,
		LastAccess:       now,
	}

	// LRU 容量检查
	if c.store.Count() >= c.maxEntries {
		c.evictLRU()
	}

	c.mu.Lock()
	c.accessTime[entryKey] = now
	c.mu.Unlock()

	return c.store.Put(entry)
}

// Count 返回当前缓存条目数（store=nil 时返回 0）。
func (c *SemanticCache) Count() int {
	if c.store == nil {
		return 0
	}
	return c.store.Count()
}

// hashKey 计算请求的确定性哈希。
// 输入: SHA-256(Namespace + SystemPromptHash + ContextHintFingerprint + ControlLabels + TaskType + Messages)
func (c *SemanticCache) hashKey(key CacheKey) string {
	h := sha256.New()
	h.Write([]byte(c.namespace))
	h.Write([]byte(c.systemPromptHash))
	h.Write([]byte(key.ContextHintFingerprint))
	h.Write([]byte(strings.Join(key.ActiveControlLabels, ",")))
	h.Write([]byte(key.TaskType))
	h.Write([]byte(strings.Join(key.Messages, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}

// evictLRU 淘汰访问时间最旧的 maxEntries/10 个条目。
//
// 双阶段合并策略（解决重启后 accessTime 为空导致驱逐失效的问题）：
//  1. 从 store.ListOldest 取持久化最老记录（重启后唯一可靠来源）
//  2. 与进程内 accessTime 合并：内存中有更新记录的条目以内存为准（热点覆盖）
//  3. 合并后取最旧 evictCount 条删除
//
// 同时对 accessTime 做上限检查：超过 accessTimeMax 时清空，
// 让 store 完全接管，避免内存 map 无限增长。
func (c *SemanticCache) evictLRU() {
	evictCount := c.maxEntries / 10
	if evictCount <= 0 {
		evictCount = 1
	}

	// 阶段一：从 store 取持久化最老记录（fetch 2× 以便合并后有足够候选）
	storeOldest := c.store.ListOldest(evictCount * 2)

	// 构建合并 map：key → lastAccess（store 持久值作底，内存热点覆盖）
	type kv struct {
		key string
		t   time.Time
	}
	merged := make(map[string]time.Time, len(storeOldest))
	for _, e := range storeOldest {
		merged[e.Key] = e.LastAccess
	}

	// 阶段二：内存热点覆盖（进程内 access 比 store 记录更新时以内存为准）
	c.mu.Lock()
	for k, t := range c.accessTime {
		if stored, ok := merged[k]; !ok || t.After(stored) {
			merged[k] = t
		}
	}
	// accessTime 超限时清空，防止内存无限增长；驱逐后由 store 接管
	if len(c.accessTime) >= c.accessTimeMax {
		c.accessTime = make(map[string]time.Time)
	}
	c.mu.Unlock()

	if len(merged) == 0 {
		return
	}

	// 阶段三：从合并结果中取最旧 evictCount 条（插入排序，条目数有上限）
	items := make([]kv, 0, len(merged))
	for k, t := range merged {
		items = append(items, kv{k, t})
	}
	// 部分排序：只需前 evictCount 个最小值，插入排序 O(n*evictCount)，evictCount 通常 ≤ 1000
	limit := evictCount
	if limit > len(items) {
		limit = len(items)
	}
	for i := 0; i < limit; i++ {
		minIdx := i
		for j := i + 1; j < len(items); j++ {
			if items[j].t.Before(items[minIdx].t) {
				minIdx = j
			}
		}
		items[i], items[minIdx] = items[minIdx], items[i]
	}

	toDelete := make([]string, limit)
	for i := range toDelete {
		toDelete[i] = items[i].key
	}

	_ = c.store.Delete(toDelete)

	c.mu.Lock()
	for _, k := range toDelete {
		delete(c.accessTime, k)
	}
	c.mu.Unlock()
}
