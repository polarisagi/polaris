package pricing

import (
	"math"
	"testing"
	"time"

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
	if got := Resolve("claude-sonnet-x", caps, peakAt); got != (Price{3, 0.3, 15}) {
		t.Fatalf("未收录模型应回退适配器费率: %v", got)
	}
	// 收录的模型以目录为准，不被适配器（如 openai_compat 的错价）覆盖。
	if got := Resolve("deepseek-flash", types.ProviderCapabilities{CostPerMTokInput: 0.15}, peakAt); got.Input != 0.30 {
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

// peakAt 2026-10-20（周二）02:00 UTC，高峰时段内。
var peakAt = time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)

// 表驱动：高峰/非高峰边界。2026-10-19 周一、10-23 周五、10-24 周六、10-25 周日。
func TestIsPeak(t *testing.T) {
	d := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"周一 00:59 非高峰", d(19, 0, 59), false},
		{"周一 01:00 高峰起点", d(19, 1, 0), true},
		{"周一 03:59 高峰", d(19, 3, 59), true},
		{"周一 04:00 窗口结束", d(19, 4, 0), false},
		{"周一 05:59 非高峰", d(19, 5, 59), false},
		{"周一 06:00 第二窗口起点", d(19, 6, 0), true},
		{"周一 09:59 高峰", d(19, 9, 59), true},
		{"周一 10:00 窗口结束", d(19, 10, 0), false},
		{"周五 09:30 高峰", d(23, 9, 30), true},
		{"周五 23:59 非高峰", d(23, 23, 59), false},
		{"周六 02:00 周末非高峰", d(24, 2, 0), false},
		{"周六 07:00 周末非高峰", d(24, 7, 0), false},
		{"周日 08:00 周末非高峰", d(25, 8, 0), false},
		{"周日 23:59 UTC 仍是周日", d(25, 23, 59), false},
		{"周一 00:00 UTC", d(26, 0, 0), false},
		{"周一 01:00 UTC（下周）", d(26, 1, 0), true},
	} {
		if got := IsPeak(tc.at); got != tc.want {
			t.Errorf("%s: IsPeak(%v)=%v want %v", tc.name, tc.at, got, tc.want)
		}
	}
}

// 非 UTC 时区的输入按 UTC 判定：北京时间 周一 09:00 = UTC 周一 01:00，高峰；北京周六 09:00 = UTC 周六 01:00，周末非高峰。
func TestIsPeak_ConvertsToUTC(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	if !IsPeak(time.Date(2026, 10, 19, 9, 0, 0, 0, cst)) {
		t.Error("北京周一 09:00 即 UTC 01:00，应为高峰")
	}
	if IsPeak(time.Date(2026, 10, 24, 9, 0, 0, 0, cst)) {
		t.Error("北京周六 09:00 即 UTC 周六 01:00，周末应为非高峰")
	}
	// 北京周一 05:00 = UTC 周日 21:00：虽是北京工作日，UTC 属周末。
	if IsPeak(time.Date(2026, 10, 19, 5, 0, 0, 0, cst)) {
		t.Error("北京周一 05:00 即 UTC 周日 21:00，应为非高峰")
	}
}

// 非高峰价为高峰价的一半（三档单价同折）；不分时段的模型全天同价。
func TestLookupAt_OffPeakHalf(t *testing.T) {
	peak := time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC)
	off := time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id        string
		peakPrice Price
	}{
		{"deepseek-flash", Price{0.30, 0.006, 1.20}},
		{"deepseek-v4-flash", Price{0.30, 0.006, 1.20}},
		{"deepseek-v4-pro", Price{1.32, 0.044, 3.96}},
	} {
		gp, ok := LookupAt(tc.id, peak)
		if !ok || gp != tc.peakPrice {
			t.Errorf("%s 高峰价 %v,%v want %v", tc.id, gp, ok, tc.peakPrice)
		}
		go_, _ := LookupAt(tc.id, off)
		want := Price{tc.peakPrice.Input / 2, tc.peakPrice.CacheHit / 2, tc.peakPrice.Output / 2}
		if go_ != want {
			t.Errorf("%s 非高峰价 %v want %v", tc.id, go_, want)
		}
	}
	// gpt-4o-mini 不分时段。
	a, _ := LookupAt("gpt-4o-mini", peak)
	b, _ := LookupAt("gpt-4o-mini", off)
	if a != b || a != (Price{0.15, 0.075, 0.60}) {
		t.Errorf("不分时段模型应全天同价: %v %v", a, b)
	}
	if _, ok := LookupAt("unknown", off); ok {
		t.Error("未收录模型不应命中")
	}
}

// CostFor 按调用开始时刻计价：同样的 token，周末费用恰为工作日高峰的一半。
func TestCostFor_UsesCallStart(t *testing.T) {
	weekdayPeak := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	weekend := time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC)
	caps := types.ProviderCapabilities{}
	hi := CostFor("deepseek-flash", caps, weekdayPeak, 24612, 14976, 2444)
	lo := CostFor("deepseek-flash", caps, weekend, 24612, 14976, 2444)
	if !near(hi, (9636*0.30+14976*0.006+2444*1.20)/1e6) || !near(lo, hi/2) {
		t.Fatalf("hi=%v lo=%v", hi, lo)
	}
	// 目录外模型回退适配器费率，不受时段影响。
	c := types.ProviderCapabilities{CostPerMTokInput: 3, CostPerMTokOutput: 15}
	if CostFor("x", c, weekend, 1000, 0, 0) != CostFor("x", c, weekdayPeak, 1000, 0, 0) {
		t.Fatal("目录外模型不应分时段")
	}
}
