// Package config 运行时配置（atomic 更新，非 L4 不可变）。
package config

import "sync/atomic"

// globalConfig holds the currently active configuration in a wait-free manner.
// Any module can read the current configuration snapshot without locking.
var globalConfig atomic.Pointer[Config]

// Get returns the current active configuration snapshot.
// It never returns nil after the initial Init/Load.
func Get() *Config {
	return globalConfig.Load()
}

// CurrentThresholds 返回当前生效的阈值表；全局配置尚未装载时回落到 DefaultThresholds()。
//
// 立此函数的原因：Get() 在 Update() 之前返回 nil，`config.Get().Thresholds.X` 这种
// 写法在任何「配置未装载」的路径上都是空指针解引用。既有代码（llm/safecall、
// observability/metrics_handler）都老实写了 `cfg != nil &&` 的三行样板，但 2026-08-13
// 轮新增的三个读取点全部漏掉了它，其中两个落在热路径上（SSE 轮的 runFSMTurn、
// MCP stdio 的 readLoop）——靠每个调用方自觉重复样板，实测不成立。
// 只读阈值的场景一律用本函数，不要再写 config.Get().Thresholds。
func CurrentThresholds() Thresholds {
	if cfg := globalConfig.Load(); cfg != nil {
		return cfg.Thresholds
	}
	return DefaultThresholds()
}

func Update(newCfg *Config) {
	globalConfig.Store(newCfg)
}
