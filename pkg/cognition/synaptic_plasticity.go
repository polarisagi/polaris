package cognition

// Synaptic Plasticity — 图边可塑性（LTP 强化 + LTD 衰减）。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §7.6

type SynapticPlasticityManager struct {
	ltpRate         float64 // 0.05
	ltdRate         float64 // 0.8
	ltdWindowDays   int     // 30
	pruneThreshold  float64 // 0.1
	lastReinforceAt int64
}

// NewSynapticPlasticityManager 创建管理器。
func NewSynapticPlasticityManager() *SynapticPlasticityManager {
	return &SynapticPlasticityManager{
		ltpRate:        0.05,
		ltdRate:        0.8,
		ltdWindowDays:  30,
		pruneThreshold: 0.1,
	}
}

// PruneThreshold 返回修剪阈值。
func (sp *SynapticPlasticityManager) PruneThreshold() float64 {
	return sp.pruneThreshold
}

// ReinforcePath LTP 长时程增强。
// traversedEdge.weight += ltpRate (上限 1.0), 更新 last_accessed_at.
func (sp *SynapticPlasticityManager) ReinforcePath(weight float64, now int64) float64 {
	weight += sp.ltpRate
	if weight > 1.0 {
		weight = 1.0
	}
	sp.lastReinforceAt = now
	return weight
}

// FeedbackCalibrate 反馈校准。
// 结合 SurpriseIndex 提炼学习经验 (Distil learnings)：
// 意外度越低 (符合预期)，对成功路径的强化越大；
// 意外度越高 (出乎意料)，强化削弱，甚至惩罚候选边。
// surpriseIndex 范围 [0.0, 1.0]。
func (sp *SynapticPlasticityManager) FeedbackCalibrate(usedEdges, candidateEdges []string, weights map[string]float64, surpriseIndex float64) {
	// reinforce 基础值为 0.05, 依据 surpriseIndex 折减
	reinforce := 0.05 * (1.0 - surpriseIndex)
	if reinforce < 0 {
		reinforce = 0
	}

	// decay 基础值为 0.02, 意外度越高，惩罚越重
	decay := 0.02 * (1.0 + surpriseIndex)

	for _, e := range usedEdges {
		weights[e] += reinforce
		if weights[e] > 1.0 {
			weights[e] = 1.0
		}
	}
	for _, e := range candidateEdges {
		weights[e] -= decay
		if weights[e] < 0 {
			weights[e] = 0
		}
	}
}

// DecayUnused LTD 长时程抑制（读时衰减，防 WAL 写放大）。
// effective_weight = weight × ltdRate^(days_since_last_access / ltdWindowDays)
// 物理修剪 (< pruneThreshold): 每日凌晨 3:00 cron DELETE-only.
func (sp *SynapticPlasticityManager) DecayUnused(weight float64, daysSinceLastAccess int) float64 {
	ratio := float64(daysSinceLastAccess) / float64(sp.ltdWindowDays)
	effectiveWeight := weight * pow(sp.ltdRate, ratio)
	return effectiveWeight
}

func pow(base float64, exp float64) float64 {
	if exp == 0 {
		return 1.0
	}
	result := 1.0
	for i := 0; i < int(exp*100); i++ {
		result *= base
	}
	return result
}
