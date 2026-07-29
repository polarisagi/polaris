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
	db             protocol.SQLQuerier
	reinforceRate  float64
	decayRate      float64
	pruneThreshold float64
	decayWindow    time.Duration
}

func NewEdgeWeightManager(db protocol.SQLQuerier) *EdgeWeightManager {
	return &EdgeWeightManager{
		db:             db,
		reinforceRate:  0.05,
		decayRate:      0.8,
		pruneThreshold: 0.1,
		decayWindow:    30 * 24 * time.Hour,
	}
}

// ReinforcePath 在图遍历经过某条边时进行强化。
func (ewm *EdgeWeightManager) ReinforcePath(ctx context.Context, edgeID string, currentWeight float64) float64 {
	// 真实更新存储中的 strength
	query := `
		INSERT INTO world_model_edges (edge_id, storage_strength, retrieval_strength, last_accessed_at)
		VALUES (?, 1.0 + ?, 1.0, CURRENT_TIMESTAMP)
		ON CONFLICT(edge_id) DO UPDATE SET
		    storage_strength = storage_strength + ?,
		    retrieval_strength = 1.0,
		    last_accessed_at = CURRENT_TIMESTAMP
	`
	_, _ = ewm.db.ExecContext(ctx, query, edgeID, ewm.reinforceRate, ewm.reinforceRate)

	newWeight := currentWeight + ewm.reinforceRate
	if newWeight > 1.0 {
		newWeight = 1.0
	}
	return newWeight
}

// DecayUnused (读时衰减, 防 WAL 写放大):
// effective_weight = retrieval_strength × decayRate^(days_since_last_access / decayWindowDays)
func (ewm *EdgeWeightManager) DecayUnused(retrievalStrength float64, lastAccessedAt time.Time) float64 {
	days := time.Since(lastAccessedAt).Hours() / 24.0
	windowDays := ewm.decayWindow.Hours() / 24.0
	if days <= 0 {
		return retrievalStrength
	}

	decayFactor := math.Pow(ewm.decayRate, days/windowDays)
	return retrievalStrength * decayFactor
}

// FeedbackCalibrate 基于成功任务轨迹进行反馈校准。
func (ewm *EdgeWeightManager) FeedbackCalibrate(ctx context.Context, successPath []string) error {
	for _, edgeID := range successPath {
		// 读出现有 storage_strength, 增加 0.03
		query := `UPDATE world_model_edges SET storage_strength = storage_strength + 0.03 WHERE edge_id = ?`
		_, _ = ewm.db.ExecContext(ctx, query, edgeID)
	}
	return nil
}

// PeriodicPrune 每日凌晨触发的清理任务，删除 storage_strength * retrieval_strength < pruneThreshold 的边。
func (ewm *EdgeWeightManager) PeriodicPrune(ctx context.Context) error {
	query := `DELETE FROM world_model_edges WHERE (storage_strength * retrieval_strength) < ?`
	_, err := ewm.db.ExecContext(ctx, query, ewm.pruneThreshold)
	return err
}
