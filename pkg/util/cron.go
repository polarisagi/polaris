package util

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxCronLookahead 防止无限循环的上限（一年）。
const maxCronLookahead = 366 * 24 * 60

// CronSchedule 标准 5 字段 cron 表达式的解析求值器。
//
// 格式: minute hour day-of-month month day-of-week
//
//	minute:       0-59
//	hour:         0-23
//	day-of-month: 1-31
//	month:        1-12
//	day-of-week:  0-6（0=周日）
//
// 支持: 数字、范围(1-5)、步进(*/5, 1-10/2)、列表(1,3,5)、通配符(*)。
type CronSchedule struct {
	raw    string
	fields [5]cronField
}

type cronField struct {
	allowed map[int]bool // 空=nil 表示全通配
}

// ParseCron 解析 5 字段 cron 表达式，返回调度器。失败返回 error。
func ParseCron(expr string) (*CronSchedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("cron: need 5 fields, got %d: %q", len(parts), expr))
	}
	var cs CronSchedule
	cs.raw = expr
	bounds := [5][2]int{
		{0, 59}, // minute
		{0, 23}, // hour
		{1, 31}, // day-of-month
		{1, 12}, // month
		{0, 6},  // day-of-week
	}
	for i, part := range parts {
		f, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("cron field %d: %v", i, err), err)
		}
		cs.fields[i] = f
	}
	return &cs, nil
}

func parseCronField(s string, min, max int) (cronField, error) { //nolint:gocyclo
	if s == "*" {
		return cronField{}, nil // nil allowed → all
	}
	allowed := make(map[int]bool)
	for seg := range strings.SplitSeq(s, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		var lo, hi, step int
		step = 1
		if strings.Contains(seg, "/") {
			sp := strings.SplitN(seg, "/", 2)
			stepStr := strings.TrimSpace(sp[1])
			st, err := strconv.Atoi(stepStr)
			if err != nil || st <= 0 {
				return cronField{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("bad step %q", seg))
			}
			step = st
			seg = strings.TrimSpace(sp[0])
		}
		if seg == "*" {
			lo, hi = min, max
		} else if strings.Contains(seg, "-") {
			sp := strings.SplitN(seg, "-", 2)
			a, err1 := strconv.Atoi(strings.TrimSpace(sp[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(sp[1]))
			if err1 != nil || err2 != nil {
				return cronField{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("bad range %q", seg))
			}
			lo, hi = a, b
		} else {
			v, err := strconv.Atoi(seg)
			if err != nil {
				return cronField{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("bad value %q", seg))
			}
			lo, hi = v, v
		}
		for v := lo; v <= hi; v += step {
			if v >= min && v <= max {
				allowed[v] = true
			}
		}
	}
	if len(allowed) == 0 && s != "*" {
		return cronField{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("value %q out of range [%d,%d]", s, min, max))
	}
	return cronField{allowed: allowed}, nil
}

// Matches 检查给定时间是否匹配 cron 表达式。
func (cs *CronSchedule) Matches(t time.Time) bool {
	vals := [5]int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, v := range vals {
		if cs.fields[i].allowed != nil && !cs.fields[i].allowed[v] {
			return false
		}
	}
	return true
}

// NextAfter 返回 from 之后的下一个匹配时间（不含 from 本身）。
// 在一年范围内搜索，找不到返回零值。
func (cs *CronSchedule) NextAfter(from time.Time) time.Time {
	candidate := from.Truncate(time.Minute).Add(time.Minute)
	for range maxCronLookahead {
		if cs.Matches(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}
}

// MinInterval 计算 cron 表达式在 from 之后的连续 samples 次触发时间的相邻差最小值。
// 若 samples <= 1，默认取 50 次采样。
func MinInterval(expr string, from time.Time, samples int) (time.Duration, error) {
	if samples <= 1 {
		samples = 50
	}
	switch strings.TrimSpace(expr) {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	}

	parts := strings.Fields(expr)
	if len(parts) == 6 {
		parts = parts[1:]
	}
	if len(parts) != 5 {
		return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("cron: need 5 fields, got %d: %q", len(parts), expr))
	}

	cs, err := ParseCron(strings.Join(parts, " "))
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "cron: invalid expr for MinInterval", err)
	}

	cur := from
	var minDiff time.Duration = -1
	for i := 0; i < samples; i++ {
		next := cs.NextAfter(cur)
		if next.IsZero() {
			break
		}
		if i > 0 {
			diff := next.Sub(cur)
			if minDiff < 0 || diff < minDiff {
				minDiff = diff
			}
		}
		cur = next
	}

	if minDiff <= 0 {
		return 0, apperr.New(apperr.CodeInvalidInput, "cron: cannot determine minimum interval")
	}
	return minDiff, nil
}
