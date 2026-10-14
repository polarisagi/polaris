package automation

import (
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/util"
)

// CronSchedule 标准 5 字段 cron 表达式的解析求值器（类型别名）。
type CronSchedule = util.CronSchedule

// ParseCron 解析 5 字段 cron 表达式，返回调度器。
func ParseCron(expr string) (*CronSchedule, error) {
	s, err := util.ParseCron(expr)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "parse cron expression", err)
	}
	return s, nil
}

// MinInterval 计算 cron 表达式的最小间隔。
func MinInterval(expr string, from time.Time, samples int) (time.Duration, error) {
	d, err := util.MinInterval(expr, from, samples)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "calculate min interval", err)
	}
	return d, nil
}
