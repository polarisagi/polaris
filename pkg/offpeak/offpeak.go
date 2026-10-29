// Package offpeak 实现"错峰窗口"的纯计算与等待原语（ADR-0105 决策七）。
//
// 为什么放 pkg/：窗口判定是与业务无关的 UTC 时间算术，被 L3 的 learning/knowledge、
// L4 的 automation 与 cmd 装配层同时消费；放在 pkg/ 才不会制造跨层反向依赖
// （docs/arch/Module-Dependency-Axioms.md §2.1，pkg 只含 POD 与纯内存方法）。
//
// 窗口一律 UTC、半开区间 [start, end)，格式 "HH:MM-HH:MM"：
//   - start > end 表示跨零点（"22:00-06:00" = 22:00~次日 06:00）；
//   - 结束时刻允许写 "24:00"（"16:00-24:00"）；
//   - start == end 无意义（零长度还是全天有歧义），一律拒绝；全天请写 "00:00-24:00"。
//
// 不硬编码任何厂商时刻表：窗口完全来自配置，空配置 = 不错峰（Allow 恒真）。
package offpeak

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const minutesPerDay = 24 * 60

// span 为一天内的分钟区间 [start, end)；end 可为 1440（"24:00"）。start>end 表示跨零点。
type span struct{ start, end int }

// Windows 是已解析的窗口集合。零值（无窗口）表示不错峰。
type Windows struct{ spans []span }

// Parse 解析窗口列表。任一条格式非法即整体失败（配置错误应在启动/校验期暴露，而不是静默忽略）。
func Parse(specs []string) (Windows, error) {
	var w Windows
	for _, s := range specs {
		sp, err := parseSpan(s)
		if err != nil {
			return Windows{}, err
		}
		w.spans = append(w.spans, sp)
	}
	return w, nil
}

// Validate 只校验格式，供 config.Validate 使用。
func Validate(specs []string) error {
	_, err := Parse(specs)
	return err
}

func parseSpan(s string) (span, error) {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return span{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("offpeak: 窗口 %q 格式应为 HH:MM-HH:MM", s))
	}
	start, err := parseClock(lo, false)
	if err != nil {
		return span{}, apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("offpeak: 窗口 %q 起点非法", s), err)
	}
	end, err := parseClock(hi, true)
	if err != nil {
		return span{}, apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("offpeak: 窗口 %q 终点非法", s), err)
	}
	if start == end {
		return span{}, apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("offpeak: 窗口 %q 起止相同，语义有歧义；全天请写 00:00-24:00", s))
	}
	return span{start: start, end: end}, nil
}

// parseClock 把 "HH:MM" 解析为当天分钟数；allow24 时接受 "24:00"（仅作终点）。
func parseClock(s string, allow24 bool) (int, error) {
	s = strings.TrimSpace(s)
	var h, m int
	if len(s) != 5 || s[2] != ':' {
		return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("%q 不是 HH:MM", s))
	}
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("%q 不是 HH:MM", s), err)
	}
	total := h*60 + m
	if h < 0 || m < 0 || m > 59 || total > minutesPerDay || (total == minutesPerDay && !allow24) {
		return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("%q 超出 00:00~23:59（终点可为 24:00）", s))
	}
	return total, nil
}

// Enabled 是否配置了窗口。false = 不错峰。
func (w Windows) Enabled() bool { return len(w.spans) > 0 }

// contains 判断当天分钟数 m 是否落在该区间内。
func (sp span) contains(m int) bool {
	if sp.start < sp.end {
		return m >= sp.start && m < sp.end
	}
	return m >= sp.start || m < sp.end // 跨零点
}

// InWindow 报告 t（转 UTC）是否处于任一窗口内。无窗口配置时恒为 true（不错峰）。
func (w Windows) InWindow(t time.Time) bool {
	if !w.Enabled() {
		return true
	}
	u := t.UTC()
	m := u.Hour()*60 + u.Minute()
	for _, sp := range w.spans {
		if sp.contains(m) {
			return true
		}
	}
	return false
}

// NextWindowStart 返回严格晚于 t 的下一个窗口起点（UTC）。无窗口配置返回零值。
// 注意：即使 t 已在窗口内，返回的也是"下一个"窗口起点；要"最早可执行时刻"用 NextOpen。
func (w Windows) NextWindowStart(t time.Time) time.Time {
	if !w.Enabled() {
		return time.Time{}
	}
	u := t.UTC()
	dayStart := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	var best time.Time
	// 起点每天重复：今天与明天的起点已覆盖"严格晚于 t 的最近一个"。
	for d := 0; d <= 1; d++ {
		for _, sp := range w.spans {
			cand := dayStart.AddDate(0, 0, d).Add(time.Duration(sp.start) * time.Minute)
			if cand.After(u) && (best.IsZero() || cand.Before(best)) {
				best = cand
			}
		}
	}
	return best
}

// NextOpen 返回最早可执行时刻：t 在窗口内则为 t 本身，否则为下一个窗口起点。
// 无窗口配置返回 t。
func (w Windows) NextOpen(t time.Time) time.Time {
	if w.InWindow(t) {
		return t
	}
	return w.NextWindowStart(t)
}

// Gate 把 Windows 与可注入时钟绑在一起，供调度触发点使用。
// 所有方法对 nil *Gate 安全并按"不错峰"处理，调用方无需判空。
type Gate struct {
	w     Windows
	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
}

// NewGate 由窗口配置构造 Gate。窗口为空返回 nil（即不错峰，零开销）。
func NewGate(w Windows) *Gate {
	return NewGateWithClock(w, time.Now, time.After)
}

// NewGateWithClock 注入时钟与定时器（测试用）。
func NewGateWithClock(w Windows, now func() time.Time, after func(time.Duration) <-chan time.Time) *Gate {
	if !w.Enabled() {
		return nil
	}
	return &Gate{w: w, now: now, after: after}
}

// Allow 当前是否允许执行可延迟任务（窗口内，或未配置窗口）。
func (g *Gate) Allow() bool {
	if g == nil {
		return true
	}
	return g.w.InWindow(g.now())
}

// Until 返回可执行的最早时刻。已可执行时返回 (当前时刻, false)；需要等待时返回 (窗口起点, true)。
func (g *Gate) Until() (time.Time, bool) {
	if g == nil {
		return time.Time{}, false
	}
	now := g.now()
	if g.w.InWindow(now) {
		return now, false
	}
	return g.w.NextWindowStart(now), true
}

// Wait 阻塞到窗口开启；ctx 取消返回其错误。已在窗口内立即返回 nil。
// 唤醒后再复核一次（定时器精度或时钟回拨都可能让唤醒时刻仍在窗口外）。
func (g *Gate) Wait(ctx context.Context) error {
	for {
		until, wait := g.Until()
		if !wait {
			return nil
		}
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeCancelled, "offpeak: 等待窗口被取消", ctx.Err())
		case <-g.after(until.Sub(g.now())):
		}
	}
}
