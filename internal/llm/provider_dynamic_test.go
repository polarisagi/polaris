package llm

import (
	"context"
	"fmt"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockFactory struct {
	created map[string]int
}

func (f *mockFactory) CreateProvider(_ context.Context, providerID, modelID string) (protocol.Provider, string, error) {
	if f.created == nil {
		f.created = make(map[string]int)
	}
	key := providerID + "/" + modelID
	f.created[key]++
	if providerID == "invalid" {
		return nil, "", apperr.New(apperr.CodeNotFound, "not found")
	}
	return &mockProvider{caps: types.ProviderCapabilities{SupportsTools: true}}, fmt.Sprintf("[%s] %s", providerID, modelID), nil
}

func TestProviderRegistry_PickEntryByRef_RecordIDSuffix(t *testing.T) {
	cfg := config.CurrentThresholds().M1Router
	reg := NewProviderRegistry(cfg)

	p := &mockProvider{}
	// 名字为 typ/suffix，如 openai/12345678
	reg.Register("openai/12345678", "OpenAI Model", p)

	ctx := context.Background()
	// 用无 / 的前缀查找
	entry := reg.PickEntryByRef(ctx, "12345678", true)
	if entry == nil {
		t.Fatal("expected entry to be found by record ID suffix")
	}
	if entry.name != "openai/12345678" {
		t.Fatalf("expected name 'openai/12345678', got %q", entry.name)
	}

	// 超过8位的前缀自动截取前8位匹配
	entryLong := reg.PickEntryByRef(ctx, "12345678-abcd-ef01", true)
	if entryLong == nil {
		t.Fatal("expected entry to be found by long record ID suffix")
	}
}

func TestProviderRegistry_PickEntryByRef_DynamicInstantiateAndLRU(t *testing.T) {
	cfg := config.CurrentThresholds().M1Router
	cfg.ModelDynamicCacheSize = 2 // 设置为 2 验证 LRU
	reg := NewProviderRegistry(cfg)

	fac := &mockFactory{}
	reg.InjectProviderFactory(fac)

	ctx := context.Background()

	// 1. 动态实例化 prov1/m1
	e1 := reg.PickEntryByRef(ctx, "prov1/m1", true)
	if e1 == nil {
		t.Fatal("expected e1 to be instantiated")
	}
	if fac.created["prov1/m1"] != 1 {
		t.Fatalf("expected 1 creation call, got %d", fac.created["prov1/m1"])
	}

	// 2. 第二次访问命中缓存，不应再次调用 factory
	e1Cached := reg.PickEntryByRef(ctx, "prov1/m1", true)
	if e1Cached != e1 {
		t.Fatal("expected same entry pointer from cache")
	}
	if fac.created["prov1/m1"] != 1 {
		t.Fatalf("expected still 1 creation call, got %d", fac.created["prov1/m1"])
	}

	// 3. 动态实例化 prov2/m2
	e2 := reg.PickEntryByRef(ctx, "prov2/m2", true)
	if e2 == nil {
		t.Fatal("expected e2 to be instantiated")
	}

	// 此时队列为 [prov1/m1, prov2/m2]，容量为 2
	// 4. 再次访问 prov1/m1 使其变热，队列变为 [prov2/m2, prov1/m1]
	_ = reg.PickEntryByRef(ctx, "prov1/m1", true)

	// 5. 动态实例化 prov3/m3，将淘汰最冷的 prov2/m2
	e3 := reg.PickEntryByRef(ctx, "prov3/m3", true)
	if e3 == nil {
		t.Fatal("expected e3 to be instantiated")
	}

	// 此时 prov2/m2 应已被逐出缓存
	reg.mu.RLock()
	_, m2Present := reg.dynamicEntries["prov2/m2"]
	_, m1Present := reg.dynamicEntries["prov1/m1"]
	_, m3Present := reg.dynamicEntries["prov3/m3"]
	reg.mu.RUnlock()

	if m2Present {
		t.Errorf("prov2/m2 should have been evicted")
	}
	if !m1Present || !m3Present {
		t.Errorf("prov1/m1 and prov3/m3 should be present in dynamicEntries")
	}

	// 6. 再次访问 prov2/m2 应该触发重新创建
	_ = reg.PickEntryByRef(ctx, "prov2/m2", true)
	if fac.created["prov2/m2"] != 2 {
		t.Fatalf("expected prov2/m2 creation count 2, got %d", fac.created["prov2/m2"])
	}
}

func TestInferenceRouter_ProviderRef_Fallback(t *testing.T) {
	cfg := config.CurrentThresholds().M1Router
	reg := NewProviderRegistry(cfg)

	// 注册一个默认池的 Provider 作为回退兜底
	pPool := &mockProvider{caps: types.ProviderCapabilities{SupportsTools: true}}
	reg.RegisterWithRole("default/fallback", "Fallback Default", "default", pPool)

	ir := NewInferenceRouter(reg, nil)

	ctx := context.Background()
	msgs := []types.Message{{Role: "user", Content: "hello"}}

	// 传入不存在的 ProviderRef，且未注入 Factory
	resp, err := ir.Infer(ctx, msgs, types.WithProviderRef("nonexistent/model"), types.WithModelPool(string(types.ModelPoolDefault)))
	if err != nil {
		t.Fatalf("Infer expected success via fallback, got %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("expected resp 'ok', got %q", resp.Content)
	}

	// 流式调用回落
	stream, err := ir.StreamInfer(ctx, msgs, types.WithProviderRef("nonexistent/model"), types.WithModelPool(string(types.ModelPoolDefault)))
	if err != nil {
		t.Fatalf("StreamInfer expected success via fallback, got %v", err)
	}
	var events []types.StreamEvent
	for ev := range stream {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatalf("expected stream events")
	}
}
