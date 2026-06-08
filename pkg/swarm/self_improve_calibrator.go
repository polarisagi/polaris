package swarm

import "sort"

// ============================================================================
// DynamicDifficultyCalibrator — 动态难度校准
// 架构文档: docs/arch/09-Self-Improvement-Engine-深度选型.md §2.1

type DynamicDifficultyCalibrator struct {
	history           []DifficultySample
	targetSuccessRate float64 // 0.6
	adjustStep        float64 // 0.05
	currentLow        float64 // SurpriseIndex 下限
	currentHigh       float64 // SurpriseIndex 上限
}

// DifficultySample 难度样本。
type DifficultySample struct {
	TaskType      string
	SurpriseIndex float64
	Success       bool
}

// Calibrate 动态调整难度阈值。
// lastN(50); len<20 → static [0.3, 0.6]
// successRate < 0.5 → low-=0.05, high-=0.05 (floor 0.1)
// successRate > 0.7 → low+=0.05, high+=0.05 (cap 0.85)
func (ddc *DynamicDifficultyCalibrator) Calibrate() {
	if len(ddc.history) < 20 {
		ddc.currentLow = 0.3
		ddc.currentHigh = 0.6
		return
	}

	var successes int
	for _, s := range ddc.history[len(ddc.history)-50:] {
		if s.Success {
			successes++
		}
	}
	rate := float64(successes) / float64(max(50, len(ddc.history)))

	if rate < 0.5 {
		ddc.currentLow = maxF(0.1, ddc.currentLow-ddc.adjustStep)
		ddc.currentHigh = maxF(0.1, ddc.currentHigh-ddc.adjustStep)
	} else if rate > 0.7 {
		ddc.currentLow = minF(0.85, ddc.currentLow+ddc.adjustStep)
		ddc.currentHigh = minF(0.85, ddc.currentHigh+ddc.adjustStep)
	}
}

// CoEvolutionCoordinator — 跨模块协同进化
// 架构文档: docs/arch/09-Self-Improvement-Engine-深度选型.md §2.4

type CoEvolutionCoordinator struct {
	subscribers map[string][]*CoEvSubscriber
}

// Subscribe 注册协同进化事件的监听器。
func (cec *CoEvolutionCoordinator) Subscribe(sub *CoEvSubscriber) {
	if cec.subscribers == nil {
		cec.subscribers = make(map[string][]*CoEvSubscriber)
	}
	cec.subscribers[sub.Module] = append(cec.subscribers[sub.Module], sub)
}

// Publish 广播协同进化事件给相关的监听器。
func (cec *CoEvolutionCoordinator) Publish(event *CoEvolutionEvent) {
	if cec.subscribers == nil {
		return
	}
	for _, subs := range cec.subscribers {
		for _, sub := range subs {
			_ = sub.OnChange(event)
		}
	}
}

// CoEvolutionEvent 协同进化事件。
type CoEvolutionEvent struct {
	SourceModule string
	ChangeType   string
	ChangeLevel  int
}

// CoEvSubscriber 订阅者。
type CoEvSubscriber struct {
	Module   string
	OnChange func(event *CoEvolutionEvent) error
}

// AutoConfigOptimizer 自动配置优化 (L0)。
// 架构文档: docs/arch/09-Self-Improvement-Engine-深度选型.md §4.3
// stats 提供 7-30 天 ProviderStats / SurpriseIndex 历史数据读取。
type AutoConfigOptimizer struct {
	stats   StatsReader
	weights *RouteWeightModel   // 当前路由权重
	thresh  *SurpriseThresholds // 当前惊喜阈值
}

// StatsReader 读取历史 Provider 统计和 SurpriseIndex 分布。
type StatsReader interface {
	ProviderStats7d() ([]ProviderStat, error)
	SurpriseDistribution30d() ([]float64, error)
}

// ProviderStat 7 天内单个 Provider 的统计摘要。
type ProviderStat struct {
	Name         string
	TotalCost    float64
	SuccessCount int
	TotalCalls   int
}

// RouteWeightModel 路由权重模型。
type RouteWeightModel struct {
	Weights map[string]float64
}

// SurpriseThresholds 惊喜指数三级阈值。
type SurpriseThresholds struct {
	Sys1Max  float64 // System 1 上限 (P50)
	Sys15Max float64 // System 1.5 上限 (P90)
}

// NewAutoConfigOptimizer 创建自动配置优化器。
func NewAutoConfigOptimizer(stats StatsReader, weights *RouteWeightModel, thresh *SurpriseThresholds) *AutoConfigOptimizer {
	return &AutoConfigOptimizer{stats: stats, weights: weights, thresh: thresh}
}

// OptimizeRouteWeights 基于 7 天 ProviderStats 自动调整路由权重。
// cps = TotalCost / max(SuccessCount, 1)
// cps < Avg × 0.8 → weight + 0.1
// cps > Avg × 1.5 → weight - 0.1
func (aco *AutoConfigOptimizer) OptimizeRouteWeights() {
	if aco.stats == nil || aco.weights == nil {
		return
	}
	stats, err := aco.stats.ProviderStats7d()
	if err != nil || len(stats) == 0 {
		return
	}
	var totalCost float64
	totalSuccess := 0
	for _, s := range stats {
		totalCost += s.TotalCost
		totalSuccess += s.SuccessCount
	}
	avgCPS := totalCost / float64(max(totalSuccess, 1))
	for _, s := range stats {
		successes := max(s.SuccessCount, 1)
		cps := s.TotalCost / float64(successes)
		currentW := aco.weights.Weights[s.Name]
		switch {
		case cps < avgCPS*0.8:
			currentW += 0.1
		case cps > avgCPS*1.5:
			currentW -= 0.1
		}
		if currentW < 0.05 {
			currentW = 0.05
		}
		if currentW > 0.95 {
			currentW = 0.95
		}
		aco.weights.Weights[s.Name] = currentW
	}
}

// CalibrateSurpriseThresholds 基于 30 天 System 1 成功率校准阈值。
// 取 P50 / P90 替代静态 [0.3, 0.7] — 数据驱动替代硬编码常数。
func (aco *AutoConfigOptimizer) CalibrateSurpriseThresholds() {
	if aco.stats == nil || aco.thresh == nil {
		return
	}
	distribution, err := aco.stats.SurpriseDistribution30d()
	if err != nil || len(distribution) < 3 {
		return
	}
	sort.Float64s(distribution)
	n := len(distribution)
	aco.thresh.Sys1Max = distribution[n*50/100]
	aco.thresh.Sys15Max = distribution[n*90/100]
}
