package metrics

import "sync/atomic"

// GlobalCognitivePressure 记录全系统当前的认知压力 (0-100 Gauge)
var GlobalCognitivePressure atomic.Int64

// SetCognitivePressure 设置当前系统的认知压力指标
func SetCognitivePressure(val int64) {
	GlobalCognitivePressure.Store(val)
}

// GetCognitivePressure 获取当前系统的认知压力指标
func GetCognitivePressure() int64 {
	return GlobalCognitivePressure.Load()
}
