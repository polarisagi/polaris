package pricing

import (
	"math"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLookup(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want Price
		ok   bool
	}{
		{"deepseek-flash", Price{0.30, 0.006, 1.20}, true},
		{"DeepSeek-Flash", Price{0.30, 0.006, 1.20}, true},
		{"deepseek/deepseek-v4-pro", Price{1.32, 0.044, 3.96}, true},
		{"deepseek-v4-flash", Price{0.30, 0.006, 1.20}, true},
		{"gpt-4o-mini-2024-07-18", Price{0.15, 0.075, 0.60}, true},
		{"unknown-model", Price{}, false},
		{"", Price{}, false},
	} {
		got, ok := Lookup(tc.id)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Lookup(%q)=%v,%v want %v,%v", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

// 表驱动：含缓存命中价、无命中价回退输入价、命中超过输入时夹紧。
func TestCostUSD(t *testing.T) {
	flash, _ := Lookup("deepseek-flash")
	for _, tc := range []struct {
		name         string
		p            Price
		in, hit, out int
		want         float64
	}{
		{"flash 无命中", flash, 11762, 0, 322, (11762*0.30 + 322*1.20) / 1e6},
		{"flash 含命中", flash, 24612, 14976, 2444, (9636*0.30 + 14976*0.006 + 2444*1.20) / 1e6},
		{"无命中价回退输入价", Price{Input: 2, Output: 4}, 1000, 400, 100, (1000*2 + 100*4) / 1e6},
		{"命中夹紧到输入", flash, 100, 500, 0, 100 * 0.006 / 1e6},
		{"全零", flash, 0, 0, 0, 0},
	} {
		if got := tc.p.CostUSD(tc.in, tc.hit, tc.out); !near(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestResolveFallsBackToAdapterCaps(t *testing.T) {
	caps := types.ProviderCapabilities{CostPerMTokInput: 3, CostPerMTokOutput: 15, CostPerMTokCacheHit: 0.3}
	if got := Resolve("claude-sonnet-x", caps); got != (Price{3, 0.3, 15}) {
		t.Fatalf("未收录模型应回退适配器费率: %v", got)
	}
	// 收录的模型以目录为准，不被适配器（如 openai_compat 的错价）覆盖。
	if got := Resolve("deepseek-flash", types.ProviderCapabilities{CostPerMTokInput: 0.15}); got.Input != 0.30 {
		t.Fatalf("目录应优先: %v", got)
	}
}

// 两条真实事故记录：修正后必须是美分以下量级，而非旧的 1.9575 / 2.91。
func TestRealIncidentRecords(t *testing.T) {
	flash, _ := Lookup("deepseek-flash")
	a := flash.CostUSD(11762, 0, 322)
	b := flash.CostUSD(24612, 14976, 2444)
	if a > 0.01 || a < 0.001 || b > 0.01 || b < 0.001 {
		t.Fatalf("量级错误: a=%v b=%v", a, b)
	}
}
