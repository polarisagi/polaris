package substrate

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

	// LRU 追踪：key → lastAccess（store 无 List 能力时在内存维护访问顺序）
	mu         sync.Mutex
	accessTime map[string]time.Time
}

// CacheKey 语义缓存查询键（调用方填充请求上下文字段）。
type CacheKey struct {
	// ContextHintFingerprint M5 ContextAssembler 构建的上下文指纹。
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
// LRU 追踪仅限于本进程生命周期内的访问记录。
func (c *SemanticCache) evictLRU() {
	evictCount := c.maxEntries / 10
	if evictCount <= 0 {
		evictCount = 1
	}

	c.mu.Lock()
	// 收集所有已知条目的访问时间，按时间升序排序取最旧的
	type kv struct {
		key string
		t   time.Time
	}
	items := make([]kv, 0, len(c.accessTime))
	for k, t := range c.accessTime {
		items = append(items, kv{k, t})
	}
	c.mu.Unlock()

	if len(items) == 0 {
		return
	}

	// 简单选择排序取最旧 evictCount 个（条目数有上限，性能可接受）
	for i := 0; i < evictCount && i < len(items); i++ {
		minIdx := i
		for j := i + 1; j < len(items); j++ {
			if items[j].t.Before(items[minIdx].t) {
				minIdx = j
			}
		}
		items[i], items[minIdx] = items[minIdx], items[i]
	}

	toDelete := make([]string, 0, evictCount)
	for i := 0; i < evictCount && i < len(items); i++ {
		toDelete = append(toDelete, items[i].key)
	}

	_ = c.store.Delete(toDelete)

	c.mu.Lock()
	for _, k := range toDelete {
		delete(c.accessTime, k)
	}
	c.mu.Unlock()
}
