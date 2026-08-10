package graph

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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
//
// 返回 (float64, error)（ADR-0094 决策四）：DB 写入失败时不得返回"看起来已生效"
// 的伪值——突触可塑性是世界模型的核心状态，调用方必须能感知持久化失败并自行
// 决定是否重试/降级，而不是把一个只存在于内存里的权重当真。写成功时返回新权重
// 与 nil error；写失败时返回未变更的原始 currentWeight 与非 nil error。
func (ewm *EdgeWeightManager) ReinforcePath(ctx context.Context, edgeID string, currentWeight float64) (float64, error) {
	// 真实更新存储中的 strength
	query := `
		INSERT INTO world_model_edges (edge_id, storage_strength, retrieval_strength, last_accessed_at)
		VALUES (?, 1.0 + ?, 1.0, CURRENT_TIMESTAMP)
		ON CONFLICT(edge_id) DO UPDATE SET
		    storage_strength = storage_strength + ?,
		    retrieval_strength = 1.0,
		    last_accessed_at = CURRENT_TIMESTAMP
	`
	if _, err := ewm.db.ExecContext(ctx, query, edgeID, ewm.reinforceRate, ewm.reinforceRate); err != nil {
		slog.WarnContext(ctx, "edge_weight: reinforce persist failed, returning unchanged weight",
			"edge_id", edgeID, "err", err)
		return currentWeight, apperr.Wrap(apperr.CodeStorageUnavailable, "edge_weight: reinforce persist failed", err)
	}

	newWeight := currentWeight + ewm.reinforceRate
	if newWeight > 1.0 {
		newWeight = 1.0
	}
	return newWeight, nil
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
	// 逐条累积错误而非首错即返：校准是对一整条成功轨迹的加权，
	// 中途 return 会让轨迹前半段加权、后半段没加，产生偏斜的世界模型。
	var errs []error
	for _, edgeID := range successPath {
		// 读出现有 storage_strength, 增加 0.03
		query := `UPDATE world_model_edges SET storage_strength = storage_strength + 0.03 WHERE edge_id = ?`
		if _, err := ewm.db.ExecContext(ctx, query, edgeID); err != nil {
			errs = append(errs, apperr.Wrap(apperr.CodeInternal, "edge "+edgeID, err))
		}
	}
	if len(errs) > 0 {
		// 此前本函数无论发生什么都 return nil——签名承诺了错误上报却从不上报，
		// 调用方据此认为校准已完成。
		return apperr.Wrap(apperr.CodeInternal,
			"EdgeWeightManager.FeedbackCalibrate: partial calibration failure", errors.Join(errs...))
	}
	return nil
}

// PeriodicPrune 每日凌晨触发的清理任务，删除 storage_strength * retrieval_strength < pruneThreshold 的边。
func (ewm *EdgeWeightManager) PeriodicPrune(ctx context.Context) error {
	query := `DELETE FROM world_model_edges WHERE (storage_strength * retrieval_strength) < ?`
	_, err := ewm.db.ExecContext(ctx, query, ewm.pruneThreshold)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EdgeWeightManager: failed to prune edges", err)
	}
	return nil
}
