package types

import "time"

type ContextStats struct {
	TokenCount    int       // 当前估算 token 数
	MessageCount  int       // 消息条数
	Threshold     int       // 自动压缩触发 token 阈值（contextWindow × autoCompactPct）
	WarnThreshold int       // 警告触发 token 阈值（contextWindow × warnPct）
	UsagePercent  float64   // 当前使用率（0~100，基于 contextWindow）
	LastCompactAt time.Time // 最近一次压缩时间（零值=从未压缩）
	Thrashing     bool
}

type CompactResult struct {
	TokensBefore int
	TokensAfter  int
	Skipped      bool // hook 阻塞、降级或内容不足时为 true
}
