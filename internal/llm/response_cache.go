package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// 确定性后台 LLM 调用的精确匹配响应缓存（ADR-0105 决策六）。
//
// 只做「逐字节相同的输入 → 直接复用上次的输出」，不做任何相似度匹配：GraphRAG 抽取、查询改写、
// 记忆写入过滤这类调用对同一文本反复付费，而它们的输出是输入的纯函数。
//
// 内核阶段（perceive/plan/reflect/respond/validate）硬性排除，与白名单配置无关：它们依赖会话状态、
// 工具结果与安全门，跨会话复用旧回答可能泄漏或给出过期结论（该 ADR 反例守护）。
//
// 接入点：ProviderRegistry 的记录包装（usageRecordingProvider.Infer）。路由主路径、failover、降级池、
// PickProvider 直取都经它，因此各路径行为一致；未命中时原样透传，不改变既有行为。
// StreamInfer 不缓存——白名单用途全部是非流式调用。

const (
	// 配置取值缺失/非法时的保守回落：与 DefaultThresholds 同值，避免零值配置把缓存变成无界或永不过期。
	defaultResponseCacheTTL        = 168 * time.Hour
	defaultResponseCacheMaxEntries = 20000

	responseCacheOpTimeout = 3 * time.Second
	hitQueueSize           = 256
)

// ResponseCacheConfig 响应缓存的运行参数。
type ResponseCacheConfig struct {
	Enabled    bool
	TTL        time.Duration
	MaxEntries int
	// Purposes 白名单；内核阶段 purpose 会被丢弃并告警。
	Purposes []string
}

// ResponseCacheConfigFromThresholds 从 M1 路由阈值构造运行参数。
func ResponseCacheConfigFromThresholds(t config.M1RouterThresholds) ResponseCacheConfig {
	return ResponseCacheConfig{
		Enabled:    t.ResponseCacheEnabled,
		TTL:        time.Duration(t.ResponseCacheTTLHours) * time.Hour,
		MaxEntries: t.ResponseCacheMaxEntries,
		Purposes:   t.ResponseCachePurposes,
	}
}

// cachedResponse 是落库的响应形态。不存 Usage（命中时 token 恒为 0）与 ToolCalls（缓存要求无 Tools）。
type cachedResponse struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Model            string `json:"model,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
}

type responseCache struct {
	store      repo.LLMResponseCacheRepository
	ttl        time.Duration
	maxEntries int
	purposes   map[string]struct{}
	now        func() time.Time
	hits       chan string // 命中计数异步落库队列，满则丢弃（hit_count 仅供观测）
}

func newResponseCache(store repo.LLMResponseCacheRepository, cfg ResponseCacheConfig) *responseCache {
	c := &responseCache{
		store:      store,
		ttl:        cfg.TTL,
		maxEntries: cfg.MaxEntries,
		purposes:   make(map[string]struct{}, len(cfg.Purposes)),
		now:        time.Now,
		hits:       make(chan string, hitQueueSize),
	}
	if c.ttl <= 0 {
		c.ttl = defaultResponseCacheTTL
	}
	if c.maxEntries <= 0 {
		c.maxEntries = defaultResponseCacheMaxEntries
	}
	for _, p := range cfg.Purposes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if types.IsKernelPurpose(p) {
			// 硬排除：即使被误配进白名单也不生效。
			slog.Warn("llm response cache: kernel purpose in whitelist is ignored", "purpose", p)
			continue
		}
		c.purposes[p] = struct{}{}
	}
	return c
}

// runHitWriter 消费命中队列，把 hit_count 累加异步落库；随 ctx 结束。
func (c *responseCache) runHitWriter(ctx context.Context) {
	concurrent.SafeGo(ctx, "llm.response_cache.hit_writer", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case key := <-c.hits:
				opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), responseCacheOpTimeout)
				if err := c.store.IncrHit(opCtx, key); err != nil {
					slog.Debug("llm response cache: hit count update failed", "err", err)
				}
				cancel()
			}
		}
	})
}

// cacheCall 是一次满足缓存条件的调用的键与元数据。
type cacheCall struct {
	key      string
	purpose  string
	provider string
	model    string
}

// prepare 判定本次调用是否可缓存并计算键；不可缓存返回 nil。
//
// 条件（全部满足）：Temperature==0、ThinkingMode 显式为 Disabled（未声明的空串不算——DeepSeek
// 省略即 high，输出不确定且昂贵）、无 Tools、无多模态 Parts、purpose 非内核且在白名单内。
func (c *responseCache) prepare(provider, defaultModel string, msgs []types.Message, opts []types.InferOption) *cacheCall {
	var o types.InferOptions
	for _, fn := range opts {
		fn(&o)
	}
	if o.Temperature != 0 || o.ThinkingMode != types.ThinkingDisabled || len(o.Tools) > 0 {
		return nil
	}
	if types.IsKernelPurpose(o.Purpose) {
		return nil
	}
	if _, ok := c.purposes[o.Purpose]; !ok {
		return nil
	}
	for _, m := range msgs {
		if len(m.Parts) > 0 {
			return nil
		}
	}
	model := o.Model
	if model == "" {
		model = defaultModel
	}
	key, ok := buildCacheKey(provider, model, &o, msgs)
	if !ok {
		return nil
	}
	return &cacheCall{key: key, purpose: o.Purpose, provider: provider, model: model}
}

type canonMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildCacheKey = sha256(provider | model | purpose | max_tokens | top_p | response_format JSON | messages JSON)。
//
// 在 ADR 约定的五要素之外加入 max_tokens 与 top_p：二者改变输出（截断位置/采样），
// 不纳入键会让小上限的调用复用大上限的结果。各段以 NUL 分隔，避免相邻字段拼接歧义。
// json.Marshal 对 map 键排序，response_format 的 JSONSchema 因此有确定序。
func buildCacheKey(provider, model string, o *types.InferOptions, msgs []types.Message) (string, bool) {
	rf, err := json.Marshal(o.ResponseFormat)
	if err != nil {
		return "", false
	}
	canon := make([]canonMessage, len(msgs))
	for i, m := range msgs {
		canon[i] = canonMessage{Role: m.Role, Content: m.Content}
	}
	mj, err := json.Marshal(canon)
	if err != nil {
		return "", false
	}
	h := sha256.New()
	for _, part := range []string{
		provider, model, o.Purpose,
		strconv.Itoa(o.MaxTokens), strconv.FormatFloat(o.TopP, 'g', -1, 64),
		string(rf), string(mj),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// lookup 查缓存；任何存储错误都按未命中处理（缓存是优化，不得让调用失败）。
func (c *responseCache) lookup(ctx context.Context, cc *cacheCall) (*types.ProviderResponse, bool) {
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), responseCacheOpTimeout)
	defer cancel()
	e, err := c.store.Get(opCtx, cc.key, c.now().UnixMilli())
	if err != nil {
		slog.Warn("llm response cache: lookup failed, treated as miss", "purpose", cc.purpose, "err", err)
		metrics.GlobalLLMResponseCacheMissTotal.Add(1)
		return nil, false
	}
	if e == nil {
		metrics.GlobalLLMResponseCacheMissTotal.Add(1)
		return nil, false
	}
	var cr cachedResponse
	if err := json.Unmarshal([]byte(e.Response), &cr); err != nil || strings.TrimSpace(cr.Content) == "" {
		metrics.GlobalLLMResponseCacheMissTotal.Add(1)
		return nil, false
	}
	metrics.GlobalLLMResponseCacheHitTotal.Add(1)
	select {
	case c.hits <- cc.key:
	default:
	}
	return &types.ProviderResponse{
		Content:          cr.Content,
		ReasoningContent: cr.ReasoningContent,
		Model:            cr.Model,
		FinishReason:     cr.FinishReason,
		// Usage 恒为零值：未调用 Provider，不产生 token 与费用。
	}, true
}

// cacheable 只缓存「完整、非空、非工具调用」的响应：被截断（length）或被内容过滤的输出复用即传播缺陷。
func cacheable(resp *types.ProviderResponse) bool {
	if resp == nil || strings.TrimSpace(resp.Content) == "" || len(resp.ToolCalls) > 0 {
		return false
	}
	switch strings.ToLower(resp.FinishReason) {
	case "length", "max_tokens", "content_filter", "tool_calls", "tool_use":
		return false
	}
	return true
}

// put 写入一条并做有界淘汰；失败只告警，不影响本次调用结果。
func (c *responseCache) put(ctx context.Context, cc *cacheCall, resp *types.ProviderResponse) {
	if !cacheable(resp) {
		return
	}
	body, err := json.Marshal(cachedResponse{
		Content: resp.Content, ReasoningContent: resp.ReasoningContent,
		Model: resp.Model, FinishReason: resp.FinishReason,
	})
	if err != nil {
		return
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), responseCacheOpTimeout)
	defer cancel()
	now := c.now()
	if err := c.store.Put(opCtx, repo.LLMCacheEntry{
		Key: cc.key, Purpose: cc.purpose, Provider: cc.provider, Model: cc.model, Response: string(body),
		CreatedAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(c.ttl).UnixMilli(),
	}); err != nil {
		slog.Warn("llm response cache: put failed", "purpose", cc.purpose, "err", err)
		return
	}
	if _, err := c.store.EvictOverflow(opCtx, c.maxEntries); err != nil {
		slog.Warn("llm response cache: evict failed", "err", err)
	}
}

// prune 删除过期行，供周期清理调用。
func (c *responseCache) prune(ctx context.Context) (int64, error) {
	return c.store.DeleteExpired(ctx, c.now().UnixMilli()) //nolint:wrapcheck // 仓库层已包装 apperr
}

// probeResponseCache 判定本次调用是否满足缓存条件并查缓存。
// 返回 cc==nil 表示不可缓存；hit!=nil 表示命中（此时不得再调用 Provider）。
func (p *usageRecordingProvider) probeResponseCache(ctx context.Context, msgs []types.Message, opts []types.InferOption) (*responseCache, *cacheCall, *types.ProviderResponse) {
	if p.cache == nil {
		return nil, nil, nil
	}
	rc := p.cache.Load()
	if rc == nil {
		return nil, nil, nil
	}
	cc := rc.prepare(p.name, p.ModelID(), msgs, opts)
	if cc == nil {
		return rc, nil, nil
	}
	if resp, ok := rc.lookup(ctx, cc); ok {
		return rc, cc, resp
	}
	return rc, cc, nil
}

// InjectResponseCache 启用确定性后台调用的精确响应缓存。cfg.Enabled 为 false 或 store 为 nil 时
// 撤销缓存（幂等）。命中计数写协程随 ctx 结束。未注入时调用照常进行，只是不缓存。
func (r *ProviderRegistry) InjectResponseCache(ctx context.Context, store repo.LLMResponseCacheRepository, cfg ResponseCacheConfig) {
	if store == nil || !cfg.Enabled {
		r.cache.Store(nil)
		return
	}
	c := newResponseCache(store, cfg)
	c.runHitWriter(ctx)
	r.cache.Store(c)
}

// PruneResponseCache 删除已过期的缓存行，挂在既有的周期清理上（cmd/polaris/boot_tools.go）。
// 未启用缓存时返回 (0, nil)。
func (r *ProviderRegistry) PruneResponseCache(ctx context.Context) (int64, error) {
	c := r.cache.Load()
	if c == nil {
		return 0, nil
	}
	return c.prune(ctx)
}
