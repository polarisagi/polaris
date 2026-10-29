package offpeak

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func at(h, m int) time.Time { return time.Date(2026, 9, 30, h, m, 0, 0, time.UTC) }

func mustParse(t *testing.T, specs ...string) Windows {
	t.Helper()
	w, err := Parse(specs)
	if err != nil {
		t.Fatalf("Parse(%v): %v", specs, err)
	}
	return w
}

// 空配置 = 不错峰：任何时刻都允许，且没有"下一窗口"。
func TestEmptyWindowsNeverDefers(t *testing.T) {
	w := mustParse(t)
	if w.Enabled() || !w.InWindow(at(3, 0)) || !w.NextWindowStart(at(3, 0)).IsZero() {
		t.Fatal("空窗口必须恒放行且无下一窗口")
	}
	if g := NewGate(w); g != nil || !g.Allow() {
		t.Fatal("空窗口的 Gate 应为 nil 且 Allow 恒真")
	}
}

func TestInWindowBoundariesHalfOpen(t *testing.T) {
	w := mustParse(t, "16:00-24:00")
	cases := []struct {
		h, m int
		in   bool
	}{{15, 59, false}, {16, 0, true}, {23, 59, true}, {0, 0, false}, {8, 30, false}}
	for _, c := range cases {
		if got := w.InWindow(at(c.h, c.m)); got != c.in {
			t.Errorf("%02d:%02d InWindow=%v want %v", c.h, c.m, got, c.in)
		}
	}
}

func TestCrossMidnight(t *testing.T) {
	w := mustParse(t, "22:00-06:00")
	for _, c := range []struct {
		h, m int
		in   bool
	}{{21, 59, false}, {22, 0, true}, {23, 30, true}, {0, 0, true}, {5, 59, true}, {6, 0, false}, {12, 0, false}} {
		if got := w.InWindow(at(c.h, c.m)); got != c.in {
			t.Errorf("%02d:%02d InWindow=%v want %v", c.h, c.m, got, c.in)
		}
	}
	// 窗口外 12:00 -> 当天 22:00；窗口内 01:00 -> 当天 22:00（严格晚于 now 的下一个起点）。
	if got := w.NextWindowStart(at(12, 0)); !got.Equal(at(22, 0)) {
		t.Errorf("12:00 next=%v", got)
	}
	if got := w.NextWindowStart(at(1, 0)); !got.Equal(at(22, 0)) {
		t.Errorf("01:00 next=%v", got)
	}
}

func TestNextWindowStartWrapsToTomorrowAndPicksEarliest(t *testing.T) {
	w := mustParse(t, "16:00-24:00", "02:00-04:00")
	if got := w.NextWindowStart(at(17, 0)); !got.Equal(at(0, 0).AddDate(0, 0, 1).Add(2 * time.Hour)) {
		t.Errorf("17:00 next=%v，应为次日 02:00", got)
	}
	if got := w.NextWindowStart(at(5, 0)); !got.Equal(at(16, 0)) {
		t.Errorf("05:00 next=%v，应为当日 16:00", got)
	}
	// 恰在起点：严格晚于 => 下一个起点，而非自身。
	if got := w.NextWindowStart(at(16, 0)); !got.Equal(at(0, 0).AddDate(0, 0, 1).Add(2 * time.Hour)) {
		t.Errorf("16:00 next=%v", got)
	}
	if got := w.NextOpen(at(5, 0)); !got.Equal(at(16, 0)) {
		t.Errorf("NextOpen(05:00)=%v", got)
	}
	if got := w.NextOpen(at(3, 0)); !got.Equal(at(3, 0)) {
		t.Errorf("窗口内 NextOpen 应为 now，got %v", got)
	}
}

func TestNonUTCInputNormalized(t *testing.T) {
	w := mustParse(t, "16:00-24:00")
	loc := time.FixedZone("UTC+8", 8*3600)
	// 北京时间 2026-10-01 01:00 == UTC 2026-09-30 17:00，在窗口内。
	if !w.InWindow(time.Date(2026, 10, 1, 1, 0, 0, 0, loc)) {
		t.Fatal("应按 UTC 判定")
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"", "16:00", "16-24", "16:00-16:00", "25:00-26:00", "24:00-02:00", "10:60-12:00", "aa:bb-cc:dd", "1:00-2:00", "10:00-24:01"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		} else if !apperr.IsCode(err, apperr.CodeInvalidInput) {
			t.Errorf("%q 错误码应为 INVALID_INPUT: %v", bad, err)
		}
	}
	if err := Validate([]string{"00:00-24:00", "22:00-06:00"}); err != nil {
		t.Fatalf("合法窗口被拒: %v", err)
	}
	if !mustParse(t, "00:00-24:00").InWindow(at(13, 7)) {
		t.Fatal("00:00-24:00 应为全天")
	}
}

// fakeClock 让 Wait 测试完全确定：after 只记录请求的等待时长并把时钟推进到目标时刻。
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.waits = append(f.waits, d)
	f.now = f.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- f.now
	return ch
}

func TestGateWaitAdvancesToWindowStart(t *testing.T) {
	clk := &fakeClock{now: at(9, 0)}
	g := NewGateWithClock(mustParse(t, "16:00-24:00"), clk.Now, clk.After)
	if g.Allow() {
		t.Fatal("09:00 应在窗口外")
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.Allow() || len(clk.waits) != 1 || clk.waits[0] != 7*time.Hour {
		t.Fatalf("应恰好等待 7h 后放行: waits=%v now=%v", clk.waits, clk.now)
	}
	// 已在窗口内：Wait 立即返回，不再登记等待。
	if err := g.Wait(context.Background()); err != nil || len(clk.waits) != 1 {
		t.Fatal("窗口内 Wait 不应等待")
	}
}

func TestGateWaitCancellable(t *testing.T) {
	clk := &fakeClock{now: at(9, 0)}
	never := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	g := NewGateWithClock(mustParse(t, "16:00-24:00"), clk.Now, never)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Wait(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("取消应返回 context.Canceled 链: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait 未响应取消")
	}
}

func TestNilGateIsPermissive(t *testing.T) {
	var g *Gate
	if !g.Allow() {
		t.Fatal("nil Gate 必须放行")
	}
	if _, wait := g.Until(); wait {
		t.Fatal("nil Gate 不应要求等待")
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
