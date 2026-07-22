package graph

import (
	"context"
	"math"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// EdgeWeightManager & EvidenceSubgraphExtractor
// 架构文档: docs/arch/M05-Memory-System.md §7.5, §7.6
// ============================================================================

// EdgeWeightManager 负责维护记忆图谱中边的权重衰减和强化。
type EdgeWeightManager struct {
	graphDB        protocol.Store // 真实实现中这里应该是一个 GraphStore 接口，使用 Store 做演示
	reinforceRate  float64
	decayRate      float64
	pruneThreshold float64
	decayWindow    time.Duration
}

func NewEdgeWeightManager(store protocol.Store) *EdgeWeightManager {
	return &EdgeWeightManager{
		graphDB:        store,
		reinforceRate:  0.05,
		decayRate:      0.8,
		pruneThreshold: 0.1,
		decayWindow:    30 * 24 * time.Hour,
	}
}

// ReinforcePath 在图遍历经过某条边时进行强化。
func (ewm *EdgeWeightManager) ReinforcePath(ctx context.Context, edgeID string, currentWeight float64) float64 {
	newWeight := currentWeight + ewm.reinforceRate
	if newWeight > 1.0 {
		newWeight = 1.0
	}
	// 在真实场景中，会异步更新存储中的权重和 last_accessed_at
	return newWeight
}

// DecayUnused (读时衰减, 防 WAL 写放大):
// effective_weight = weight × decayRate^(days_since_last_access / decayWindowDays)
func (ewm *EdgeWeightManager) DecayUnused(currentWeight float64, lastAccessedAt time.Time) float64 {
	days := time.Since(lastAccessedAt).Hours() / 24.0
	windowDays := ewm.decayWindow.Hours() / 24.0
	if days <= 0 {
		return currentWeight
	}

	decayFactor := math.Pow(ewm.decayRate, days/windowDays)
	return currentWeight * decayFactor
}

// FeedbackCalibrate 基于成功任务轨迹进行反馈校准。
func (ewm *EdgeWeightManager) FeedbackCalibrate(ctx context.Context, successPath []string) error {
	for _, edgeID := range successPath {
		// 实际上需要从数据库读取原 weight, +0.03 applicability 等
		_ = edgeID
	}
	return nil
}

// PeriodicPrune 每日凌晨触发的清理任务，删除权重 < pruneThreshold 的边。
func (ewm *EdgeWeightManager) PeriodicPrune(ctx context.Context) error {
	// 执行 DELETE-only 的边删除
	// 这里仅做桩实现
	return nil
}
