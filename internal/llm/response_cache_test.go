package llm

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeCacheStore 内存实现，语义与 SQLite 实现一致（过期以 nowMs 判定、淘汰保留最新 N 条）。
// 真实 SQL 的过期/淘汰行为由 internal/store/repo 的测试锁定，这里只验证 llm 层如何使用它。
type fakeCacheStore struct {
	mu      sync.Mutex
	rows    map[string]repo.LLMCacheEntry
	getErr  error
	putErr  error
	evicted int64
}

func newFakeCacheStore() *fakeCacheStore {
	return &fakeCacheStore{rows: map[string]repo.LLMCacheEntry{}}
}

func (f *fakeCacheStore) Get(_ context.Context, key string, nowMs int64) (*repo.LLMCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	e, ok := f.rows[key]
	if !ok || e.ExpiresAtMs <= nowMs {
		return nil, nil
	}
	return &e, nil
}

func (f *fakeCacheStore) Put(_ context.Context, e repo.LLMCacheEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.rows[e.Key] = e
	return nil
}

func (f *fakeCacheStore) IncrHit(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.rows[key]; ok {
		e.HitCount++
		f.rows[key] = e
	}
	return nil
}

func (f *fakeCacheStore) EvictOverflow(_ context.Context, maxEntries int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maxEntries <= 0 || len(f.rows) <= maxEntries {
		return 0, nil
	}
	all := make([]repo.LLMCacheEntry, 0, len(f.rows))
	for _, e := range f.rows {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAtMs > all[j].CreatedAtMs })
	var n int64
	for _, e := range all[maxEntries:] {
		delete(f.rows, e.Key)
		n++
	}
	f.evicted += n
	return n, nil
}

func (f *fakeCacheStore) DeleteExpired(_ context.Context, nowMs int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, e := range f.rows {
		if e.ExpiresAtMs <= nowMs {
			delete(f.rows, k)
			n++
		}
	}
	return n, nil
}

func (f *fakeCacheStore) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// countingProvider 统计真实 Provider 调用次数；响应可按需定制。
type countingProvider struct {
	mockProvider
	calls atomic.Int64
	resp  func(n int64) *types.ProviderResponse
}

func (p *countingProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	n := p.calls.Add(1)
	if p.resp != nil {
		return p.resp(n), nil
	}
	return &types.ProviderResponse{Content: "answer", Model: "mock", FinishReason: "stop",
		Usage: types.Usage{InputTokens: 100, OutputTokens: 20}}, nil
}

var whitelist = []string{"graphrag_extract", "graphrag_summary", "rag_query_rewrite", "memory_write_filter"}

type cacheHarness struct {
	router *InferenceRouter
	reg    *ProviderRegistry
	prov   *countingProvider
	store  *fakeCacheStore
	rec    *collectingRecorder
	clock  *atomic.Int64 // 缓存使用的当前时刻（Unix 毫秒）
}

func newCacheHarness(t *testing.T, cfg ResponseCacheConfig) *cacheHarness {
	t.Helper()
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	prov := &countingProvider{}
	reg.Register("flash", "Flash", prov)
	rec := &collectingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.InjectUsageRecorder(ctx, rec)
	store := newFakeCacheStore()
	reg.InjectResponseCache(ctx, store, cfg)
	h := &cacheHarness{router: NewInferenceRouter(reg, nil), reg: reg, prov: prov, store: store, rec: rec,
		clock: &atomic.Int64{}}
	h.clock.Store(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC).UnixMilli())
	if c := reg.cache.Load(); c != nil {
		c.now = func() time.Time { return time.UnixMilli(h.clock.Load()) }
	}
	return h
}

func defaultCfg() ResponseCacheConfig {
	return ResponseCacheConfig{Enabled: true, TTL: time.Hour, MaxEntries: 100, Purposes: whitelist}
}

func bg(purpose string, extra ...types.InferOption) []types.InferOption {
	return append([]types.InferOption{types.WithPurpose(purpose), types.WithThinkingMode(types.ThinkingDisabled)}, extra...)
}

func msg(s string) []types.Message { return []types.Message{{Role: "user", Content: s}} }

func TestResponseCache_MissThenHitSkipsProviderAndRecordsCacheHit(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	ctx := context.Background()

	r1, err := h.router.Infer(ctx, msg("extract this"), bg("graphrag_extract")...)
	if err != nil || r1.Content != "answer" {
		t.Fatalf("first call: %v %+v", err, r1)
	}
	r2, err := h.router.Infer(ctx, msg("extract this"), bg("graphrag_extract")...)
	if err != nil {
		t.Fatal(err)
	}
	if h.prov.calls.Load() != 1 {
		t.Fatalf("命中时不得再调用 Provider，实际调用 %d 次", h.prov.calls.Load())
	}
	if r2.Content != "answer" || r2.Usage != (types.Usage{}) {
		t.Fatalf("命中响应应复用内容且 token 为 0: %+v", r2)
	}

	rows := h.rec.waitRows(t, 2)
	var ok, hit *protocol.LLMUsageRecord
	for i := range rows {
		switch rows[i].Status {
		case protocol.LLMUsageStatusOK:
			ok = &rows[i]
		case protocol.LLMUsageStatusCacheHit:
			hit = &rows[i]
		}
	}
	if ok == nil || hit == nil {
		t.Fatalf("应各有一行 ok 与 cache_hit: %+v", rows)
	}
	if ok.InputTokens != 100 || ok.OutputTokens != 20 {
		t.Fatalf("未命中行应记真实 token: %+v", ok)
	}
	if hit.InputTokens != 0 || hit.OutputTokens != 0 || hit.CacheHitTokens != 0 || hit.CostUSD != 0 ||
		hit.Purpose != "graphrag_extract" || hit.Provider != "flash" || hit.ThinkingMode != "disabled" {
		t.Fatalf("cache_hit 行 token/费用必须为 0 并保留归因: %+v", hit)
	}
}

func TestResponseCache_NonWhitelistedPurposeNotCached(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	for range 2 {
		if _, err := h.router.Infer(context.Background(), msg("x"), bg("consolidate_summary")...); err != nil {
			t.Fatal(err)
		}
	}
	if h.prov.calls.Load() != 2 || h.store.size() != 0 {
		t.Fatalf("非白名单 purpose 不得缓存: calls=%d rows=%d", h.prov.calls.Load(), h.store.size())
	}
}

// 内核阶段即使被误配进白名单也必须被硬性排除（ADR-0105 决策六 / 反例守护）。
func TestResponseCache_KernelPurposesHardExcludedEvenIfWhitelisted(t *testing.T) {
	kernel := []string{"perceive", "plan", "execute", "reflect", "respond", "validate", "validate_watchdog",
		"kernel", "plan_prm_candidate", "unspecified", ""}
	cfg := defaultCfg()
	cfg.Purposes = append(append([]string{}, kernel...), "graphrag_extract")
	h := newCacheHarness(t, cfg)

	c := h.reg.cache.Load()
	for _, p := range kernel {
		if _, ok := c.purposes[p]; ok {
			t.Fatalf("内核 purpose %q 不得进入生效白名单", p)
		}
	}
	if _, ok := c.purposes["graphrag_extract"]; !ok {
		t.Fatal("合法白名单项应保留")
	}

	for _, p := range kernel {
		before := h.prov.calls.Load()
		for range 2 {
			opts := bg(p)
			if p == "" {
				opts = []types.InferOption{types.WithThinkingMode(types.ThinkingDisabled)}
			}
			if _, err := h.router.Infer(context.Background(), msg("same input"), opts...); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.prov.calls.Load() - before; got != 2 {
			t.Fatalf("内核 purpose %q 两次相同调用必须都打到 Provider，实际 %d 次", p, got)
		}
	}
	if h.store.size() != 0 {
		t.Fatalf("内核 purpose 不得写入缓存，实际 %d 行", h.store.size())
	}
}

func TestResponseCache_ConditionsRequired(t *testing.T) {
	cases := map[string][]types.InferOption{
		"temperature 非零": bg("graphrag_extract", types.WithTemperature(0.7)),
		"未声明 thinking":   {types.WithPurpose("graphrag_extract")},
		"thinking low":   {types.WithPurpose("graphrag_extract"), types.WithThinkingMode(types.ThinkingLow)},
		"thinking high":  {types.WithPurpose("graphrag_extract"), types.WithThinkingMode(types.ThinkingHigh)},
		"带 tools":        bg("graphrag_extract", types.WithTools([]types.ToolSchema{{Name: "t"}})),
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			h := newCacheHarness(t, defaultCfg())
			for range 2 {
				if _, err := h.router.Infer(context.Background(), msg("x"), opts...); err != nil {
					t.Fatal(err)
				}
			}
			if h.prov.calls.Load() != 2 || h.store.size() != 0 {
				t.Fatalf("%s：不得缓存 calls=%d rows=%d", name, h.prov.calls.Load(), h.store.size())
			}
		})
	}

	t.Run("多模态 Parts", func(t *testing.T) {
		h := newCacheHarness(t, defaultCfg())
		h.prov.caps = types.ProviderCapabilities{SupportsVision: true} // 路由要求含图请求只选具备 Vision 的 Provider
		m := []types.Message{{Role: "user", Content: "x", Parts: []any{types.ImagePart{}}}}
		for range 2 {
			if _, err := h.router.Infer(context.Background(), m, bg("graphrag_extract")...); err != nil {
				t.Fatal(err)
			}
		}
		if h.prov.calls.Load() != 2 {
			t.Fatalf("含 Parts 的请求不得缓存，实际调用 %d 次", h.prov.calls.Load())
		}
	})
}

func TestResponseCache_KeySensitivity(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	c := h.reg.cache.Load()
	base := c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64)))
	if base == nil {
		t.Fatal("基准调用应可缓存")
	}
	rf := &types.ResponseFormat{Type: "json_object"}
	variants := map[string]*cacheCall{
		"消息":         c.prepare("flash", "m", msg("b"), bg("graphrag_extract", types.WithMaxTokens(64))),
		"purpose":    c.prepare("flash", "m", msg("a"), bg("graphrag_summary", types.WithMaxTokens(64))),
		"provider":   c.prepare("other", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64))),
		"默认模型":       c.prepare("flash", "m2", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64))),
		"显式模型":       c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64), types.WithModel("m3"))),
		"max_tokens": c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(128))),
		"top_p":      c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64), types.WithTopP(0.5))),
		"格式":         c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64), types.WithResponseFormat(rf))),
		"角色":         c.prepare("flash", "m", []types.Message{{Role: "system", Content: "a"}}, bg("graphrag_extract", types.WithMaxTokens(64))),
	}
	for name, v := range variants {
		if v == nil {
			t.Fatalf("%s 变体应可缓存", name)
		}
		if v.key == base.key {
			t.Fatalf("%s 不同必须得到不同的键", name)
		}
	}
	same := c.prepare("flash", "m", msg("a"), bg("graphrag_extract", types.WithMaxTokens(64)))
	if same.key != base.key {
		t.Fatal("相同输入必须得到相同的键")
	}
	// NUL 分隔：("ab","c") 与 ("a","bc") 不得拼接碰撞。
	k1, _ := buildCacheKey("ab", "c", &types.InferOptions{Purpose: "p"}, msg("x"))
	k2, _ := buildCacheKey("a", "bc", &types.InferOptions{Purpose: "p"}, msg("x"))
	if k1 == k2 {
		t.Fatal("相邻字段拼接不得碰撞")
	}
}

func TestResponseCache_Expiry(t *testing.T) {
	h := newCacheHarness(t, defaultCfg()) // TTL 1h
	ctx := context.Background()
	call := func() {
		if _, err := h.router.Infer(ctx, msg("x"), bg("rag_query_rewrite")...); err != nil {
			t.Fatal(err)
		}
	}
	call()
	h.clock.Add(int64(59 * time.Minute / time.Millisecond))
	call()
	if h.prov.calls.Load() != 1 {
		t.Fatalf("TTL 内应命中，Provider 调用 %d 次", h.prov.calls.Load())
	}
	h.clock.Add(int64(2 * time.Minute / time.Millisecond)) // 累计 61 分钟
	call()
	if h.prov.calls.Load() != 2 {
		t.Fatalf("过期后应重新调用 Provider，实际 %d 次", h.prov.calls.Load())
	}
}

func TestResponseCache_PruneDeletesExpired(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	_, _ = h.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
	if n, err := h.reg.PruneResponseCache(context.Background()); err != nil || n != 0 {
		t.Fatalf("未过期不应删除: n=%d err=%v", n, err)
	}
	h.clock.Add(int64(2 * time.Hour / time.Millisecond))
	if n, err := h.reg.PruneResponseCache(context.Background()); err != nil || n != 1 || h.store.size() != 0 {
		t.Fatalf("过期行应被清理: n=%d err=%v rows=%d", n, err, h.store.size())
	}
}

func TestResponseCache_BoundedEntriesEvictOldest(t *testing.T) {
	cfg := defaultCfg()
	cfg.MaxEntries = 3
	h := newCacheHarness(t, cfg)
	for i := range 6 {
		h.clock.Add(1000) // 每条写入时刻递增，最旧者可确定
		if _, err := h.router.Infer(context.Background(), msg(string(rune('a'+i))), bg("graphrag_extract")...); err != nil {
			t.Fatal(err)
		}
		if h.store.size() > 3 {
			t.Fatalf("第 %d 次写入后条数 %d 超过上限 3", i+1, h.store.size())
		}
	}
	if h.store.size() != 3 || h.store.evicted != 3 {
		t.Fatalf("应保留 3 条并淘汰 3 条: size=%d evicted=%d", h.store.size(), h.store.evicted)
	}
	// 最旧的 "a" 已被淘汰：再次调用须回源；最新的 "f" 仍命中。
	before := h.prov.calls.Load()
	_, _ = h.router.Infer(context.Background(), msg("f"), bg("graphrag_extract")...)
	if h.prov.calls.Load() != before {
		t.Fatal("最新条目应仍命中")
	}
	_, _ = h.router.Infer(context.Background(), msg("a"), bg("graphrag_extract")...)
	if h.prov.calls.Load() != before+1 {
		t.Fatal("最旧条目应已被淘汰")
	}
}

func TestResponseCache_UncacheableResponsesNotStored(t *testing.T) {
	bad := []*types.ProviderResponse{
		{Content: "", FinishReason: "stop"},
		{Content: "   ", FinishReason: "stop"},
		{Content: "cut off", FinishReason: "length"},
		{Content: "filtered", FinishReason: "content_filter"},
		{Content: "x", FinishReason: "stop", ToolCalls: []types.InferToolCall{{ID: "1", Name: "t"}}},
	}
	for i, resp := range bad {
		h := newCacheHarness(t, defaultCfg())
		h.prov.resp = func(int64) *types.ProviderResponse { return resp }
		for range 2 {
			if _, err := h.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...); err != nil {
				t.Fatal(err)
			}
		}
		if h.prov.calls.Load() != 2 || h.store.size() != 0 {
			t.Fatalf("case %d：不完整/空/工具调用响应不得缓存: calls=%d rows=%d", i, h.prov.calls.Load(), h.store.size())
		}
	}
}

func TestResponseCache_StoreFailuresDegradeToMiss(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	h.store.getErr = errors.New("db down")
	h.store.putErr = errors.New("db down")
	for range 2 {
		resp, err := h.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
		if err != nil || resp.Content != "answer" {
			t.Fatalf("缓存存储故障不得影响调用结果: %v %+v", err, resp)
		}
	}
	if h.prov.calls.Load() != 2 {
		t.Fatalf("存储故障时应按未命中处理，实际调用 %d 次", h.prov.calls.Load())
	}
}

func TestResponseCache_DisabledOrUnsetDoesNothing(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	h := newCacheHarness(t, cfg)
	for range 2 {
		_, _ = h.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
	}
	if h.prov.calls.Load() != 2 || h.store.size() != 0 {
		t.Fatalf("关闭时不得缓存: calls=%d rows=%d", h.prov.calls.Load(), h.store.size())
	}

	// 启用后再撤销：立即停止命中。
	h2 := newCacheHarness(t, defaultCfg())
	_, _ = h2.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
	h2.reg.InjectResponseCache(context.Background(), h2.store, ResponseCacheConfig{Enabled: false})
	_, _ = h2.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
	if h2.prov.calls.Load() != 2 {
		t.Fatal("撤销缓存后应回源")
	}
	if n, err := h2.reg.PruneResponseCache(context.Background()); n != 0 || err != nil {
		t.Fatalf("未启用时 Prune 应为空操作: %d %v", n, err)
	}
}

func TestResponseCache_ConfigFallbacksAreBounded(t *testing.T) {
	c := newResponseCache(newFakeCacheStore(), ResponseCacheConfig{Enabled: true, Purposes: whitelist})
	if c.ttl != defaultResponseCacheTTL || c.maxEntries != defaultResponseCacheMaxEntries {
		t.Fatalf("零值配置必须回落到有界默认值，而非永不过期/无上限: ttl=%v max=%d", c.ttl, c.maxEntries)
	}
	thr := config.DefaultThresholds().M1Router
	cfg := ResponseCacheConfigFromThresholds(thr)
	if !cfg.Enabled || cfg.TTL != 168*time.Hour || cfg.MaxEntries != 20000 || len(cfg.Purposes) != 7 {
		t.Fatalf("默认阈值: %+v", cfg)
	}
	cc := newResponseCache(newFakeCacheStore(), cfg)
	for _, p := range cfg.Purposes {
		if _, ok := cc.purposes[p]; !ok {
			t.Fatalf("默认白名单项 %q 不应被内核排除误伤", p)
		}
	}
}

// 未命中的首次调用写入且仅写入一行。
func TestResponseCache_WritesOnMiss(t *testing.T) {
	h := newCacheHarness(t, defaultCfg())
	_, _ = h.router.Infer(context.Background(), msg("x"), bg("graphrag_extract")...)
	if h.store.size() != 1 {
		t.Fatalf("Infer 应写入 1 行，实际 %d", h.store.size())
	}
}
