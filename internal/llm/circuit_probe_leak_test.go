package llm

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// 选路/只读查询不得占用 HalfOpen 探测权：被比较但未被选中的 Provider 必须仍可被选。
func TestSelectBest_DoesNotLeakHalfOpenProbe(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{CircuitBreakerFailureCount: 1, CircuitBreakerCooldownSeconds: 1})
	router := NewInferenceRouter(reg, nil)
	// cheap 成本低、健康分高；pricey 冷却期满进入可探测状态但分数更低，不会被选中。
	reg.RegisterWithRole("cheap", "cheap", "general", &mockRouterProvider{id: "cheap"})
	reg.RegisterWithRole("pricey", "pricey", "general", &mockRouterProvider{id: "pricey",
		caps: types.ProviderCapabilities{CostPer1KInput: 9}})

	pricey := reg.entries["pricey"]
	pricey.cb.RecordFailure() // maxFailures=1 → Open
	pricey.cb.openUntil.Store(time.Now().Add(-time.Second).UnixNano())

	for i := 0; i < 5; i++ {
		_ = router.ModelID()
		_ = reg.PickProviderName("general")
		if e := reg.best(nil); e == nil || e.name != "cheap" {
			t.Fatalf("expected cheap chosen, got %+v", e)
		}
		reg.entries["cheap"].cb.RecordSuccess()
	}
	if !pricey.cb.Available() {
		t.Fatal("pricey lost its probe slot without ever being dispatched (leaked HalfOpen probe)")
	}
}

// GR-2.2-002：失败转移/ModelPool 选路同样受滑动窗口熔断约束。
func TestMultiSkip_RespectsWindowBreaker(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	router := NewInferenceRouter(reg, nil)
	reg.RegisterWithRole("a", "a", "general", &mockRouterProvider{id: "a"})
	reg.entries["a"].winBreaker.openUntil.Store(time.Now().Add(time.Minute).UnixNano())
	reg.mu.RLock()
	got := router.findBestProviderLockedMultiSkip(nil, nil)
	reg.mu.RUnlock()
	if got != nil {
		t.Fatalf("window-open provider must be skipped, got %s", got.name)
	}
}
