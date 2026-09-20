package probe

import "testing"

// GR-1.2-001：三态必须全部可达——≥Degrade 全量、[Min,Degrade) 降级、<Min 禁用。
func TestComputeState_MemoryTriState(t *testing.T) {
	fg := &FeatureGate{probe: &HardwareProbe{Tier: Tier2}, states: map[Feature]FeatureState{}, overrides: map[Feature]FeatureState{}}
	rule := featureRule{MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536}
	cases := []struct {
		mb   uint64
		want FeatureState
	}{{2048, FeatureEnabled}, {1536, FeatureEnabled}, {1200, FeatureDegraded}, {1024, FeatureDegraded}, {512, FeatureDisabled}}
	for _, c := range cases {
		if got := fg.computeState(FeatureGraphRAGFull, rule, c.mb); got != c.want {
			t.Errorf("availableMB=%d: got %v want %v", c.mb, got, c.want)
		}
	}
}

// 规则表不变量：降级阈值必须严格高于最低阈值，否则降级区间为空。
func TestFeatureRules_DegradeAboveMin(t *testing.T) {
	for f, r := range getFeatureRules() {
		if r.DegradeMemoryMB <= r.MinMemoryMB {
			t.Errorf("feature %v: DegradeMemoryMB(%d) must be > MinMemoryMB(%d)", f, r.DegradeMemoryMB, r.MinMemoryMB)
		}
	}
}
