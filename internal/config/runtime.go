// Package config 运行时配置（atomic 更新，非 L4 不可变）。
package config

import "sync/atomic"

// EmbeddingDim 当前活跃的 embedding 维度（M1 Embedder.Dimension() 运行时返回）。
// M2 OnlineReindexer、M5 HybridRetriever、M10 KnowledgeBase 通过此变量获取维度。
// 维度变更时 M1 原子更新此值，各消费模块在下次操作时检测变更。
var EmbeddingDim atomic.Int32

// globalConfig holds the currently active configuration in a wait-free manner.
// Any module can read the current configuration snapshot without locking.
var globalConfig atomic.Pointer[Config]

// Get returns the current active configuration snapshot.
// It never returns nil after the initial Init/Load.
func Get() *Config {
	return globalConfig.Load()
}

func Update(newCfg *Config) {
	globalConfig.Store(newCfg)
}
