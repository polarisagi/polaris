package sysmgr

import (
	"sync"
	"time"
)

// PressureLevel 定义系统当前的认知压力级别。
type PressureLevel int

const (
	PressureNormal   PressureLevel = iota // 正常：全功能可用
	PressureHigh                          // 高压：触发降级策略（如缩减上下文、禁用非关键后台任务）
	PressureCritical                      // 极高：严重过载，仅保留核心心跳与故障恢复机制
)

// PressureManager 全局单例，管理系统的认知压力状态。
// 架构参考: docs/arch/M04-Orchestrator.md §6.2 CC-2 GlobalCognitivePressure
type PressureManager struct {
	mu           sync.RWMutex
	currentLevel PressureLevel
	lastUpdated  time.Time
}

var (
	globalPressureMgr *PressureManager
	once              sync.Once
)

// GetPressureManager 返回 PressureManager 单例。
func GetPressureManager() *PressureManager {
	once.Do(func() {
		globalPressureMgr = &PressureManager{
			currentLevel: PressureNormal,
			lastUpdated:  time.Now(),
		}
	})
	return globalPressureMgr
}

// Current 返回当前的认知压力级别。
func (p *PressureManager) Current() PressureLevel {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.currentLevel
}

// UpdateLevel 外部通过此方法更新系统压力（如被监控组件触发）。
func (p *PressureManager) UpdateLevel(level PressureLevel) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentLevel = level
	p.lastUpdated = time.Now()
}
