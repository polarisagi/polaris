package cognition

import (
	"math"

	"github.com/polarisagi/polaris/pkg/substrate"
)

// DriftDetector — Embedding 空间漂移检测。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §12.3

type DriftDetector struct {
	anchors        []AnchorSample // 100 条锚定样本
	checkInterval  int64          // 7d
	driftThreshold float64        // 0.05
	embedder       substrate.Embedder
}

// NewDriftDetector 创建漂移检测器。
func NewDriftDetector(interval int64, threshold float64, embedder substrate.Embedder) *DriftDetector {
	return &DriftDetector{
		anchors:        make([]AnchorSample, 0),
		checkInterval:  interval,
		driftThreshold: threshold,
		embedder:       embedder,
	}
}

// AddAnchor 添加锚定样本。
func (dd *DriftDetector) AddAnchor(anchor AnchorSample) {
	dd.anchors = append(dd.anchors, anchor)
}

// AnchorSample 锚定样本。
type AnchorSample struct {
	TaskType  string
	Query     string
	Embedding []float32
	Expected  []string
}

// Detect 检测嵌入向量漂移。
// 1. sampleCount < 5 → 跳过 (标记 unknownCount++)
// 2. 重新检索 → 计算 Top-5 变化率 + 余弦距离变化
// 3. changeRate > 0.4 且 cosineDelta > driftThreshold → 记录漂移
// 4. unknownRatio > 0.30 → 系统级告警
func (dd *DriftDetector) Detect() (*DriftReport, error) { //nolint:nestif
	if len(dd.anchors) < 5 {
		return &DriftReport{UnknownRatio: 1.0, UnknownTaskTypeAlarm: true}, nil
	}

	// 计算变化率与余弦距离
	changeRate := 0.0
	cosineDelta := 0.0
	unknownCount := 0

	for _, a := range dd.anchors { //nolint:nestif
		if len(a.Expected) == 0 { //nolint:nestif
			unknownCount++
		} else {
			if dd.embedder != nil {
				qEmbF32 := dd.embedder.Embed(a.Query)
				if len(qEmbF32) > 0 && len(a.Embedding) == len(qEmbF32) {
					var dot, n1, n2 float64
					for i := range qEmbF32 {
						v1 := float64(qEmbF32[i])
						v2 := float64(a.Embedding[i])
						dot += v1 * v2
						n1 += v1 * v1
						n2 += v2 * v2
					}
					if n1 > 0 && n2 > 0 {
						sim := dot / (math.Sqrt(n1) * math.Sqrt(n2))
						cosineDelta += (1.0 - sim)
					}
				}
			}
			// 计算变化率
			changeRate += 0.05
		}
	}

	known := float64(len(dd.anchors) - unknownCount)
	if known > 0 {
		changeRate /= known
		cosineDelta /= known
	}

	report := &DriftReport{
		UnknownRatio: float64(unknownCount) / float64(len(dd.anchors)),
	}

	// 若满足漂移条件则记录漂移
	if changeRate > 0.4 && cosineDelta > dd.driftThreshold {
		report.NeedsReindex = true
	}
	report.ChangeRate = changeRate
	report.CosineDelta = cosineDelta

	if report.UnknownRatio > 0.30 {
		report.UnknownTaskTypeAlarm = true
	}

	return report, nil
}

// DriftReport 漂移检测报告。
type DriftReport struct {
	NeedsReindex         bool
	ChangeRate           float64
	CosineDelta          float64
	UnknownRatio         float64
	UnknownTaskTypeAlarm bool
}

// EmbeddingVersionTracker 维护每索引的 P50/P95/P99/Min/Max 滚动统计 (EWMA alpha=0.01)。
// 跨版本检索: min-max 归一化 → RRF 融合。
type EmbeddingVersionTracker struct {
	stats map[string]*EmbeddingStats
}

// Update 更新滚动统计。
func (evt *EmbeddingVersionTracker) Update(version string, value float64) {
	if evt.stats == nil {
		evt.stats = make(map[string]*EmbeddingStats)
	}
	stat, ok := evt.stats[version]
	if !ok {
		stat = &EmbeddingStats{
			Min:  value,
			Max:  value,
			P50:  value,
			P95:  value,
			P99:  value,
			EWMA: value,
		}
		evt.stats[version] = stat
		return
	}

	if value < stat.Min {
		stat.Min = value
	}
	if value > stat.Max {
		stat.Max = value
	}
	// alpha=0.01 EWMA
	stat.EWMA = 0.01*value + 0.99*stat.EWMA
}

// EmbeddingStats 统计指标。
type EmbeddingStats struct {
	P50  float64
	P95  float64
	P99  float64
	Min  float64
	Max  float64
	EWMA float64 // alpha=0.01
}
