package surprise

import (
	"math"

	"github.com/polarisagi/polaris/internal/store/search"
)

// DriftDetector — Embedding 空间漂移检测。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §12.3

type DriftDetector struct {
	anchors        []AnchorSample // 100 条锚定样本
	checkInterval  int64          // 7d
	driftThreshold float64        // 0.05
	embedder       search.Embedder
}

// NewDriftDetector 创建漂移检测器。
func NewDriftDetector(interval int64, threshold float64, embedder search.Embedder) *DriftDetector {
	return &DriftDetector{
		anchors:        make([]AnchorSample, 0),
		checkInterval:  interval,
		driftThreshold: threshold,
		embedder:       embedder,
	}
}

// maxAnchors 锚定样本上限，防止无限增长。
const maxAnchors = 200

// AddAnchor 添加锚定样本。超过 maxAnchors 时淘汰最旧的一批（FIFO）。
func (dd *DriftDetector) AddAnchor(anchor AnchorSample) {
	if len(dd.anchors) >= maxAnchors {
		// 淘汰前 20%（FIFO）：ring-buffer 语义，避免 slice 频繁分配
		drop := maxAnchors / 5
		copy(dd.anchors, dd.anchors[drop:])
		dd.anchors = dd.anchors[:len(dd.anchors)-drop]
	}
	dd.anchors = append(dd.anchors, anchor)
}

// AnchorSample 锚定样本。
type AnchorSample struct {
	TaskType  string
	Query     string
	Embedding []float32
	Expected  []string
}

// anchorCosineDist 计算锚点存储向量与当前重新 Embed 向量之间的余弦距离（1 - similarity）。
// 返回 (dist, ok)；ok=false 表示向量无效或维度不匹配，调用方应跳过该锚点。
func (dd *DriftDetector) anchorCosineDist(a AnchorSample) (float64, bool) {
	if dd.embedder == nil || len(a.Embedding) == 0 {
		return 0, false
	}
	qVec := dd.embedder.Embed(a.Query)
	if len(qVec) == 0 || len(qVec) != len(a.Embedding) {
		return 0, false
	}
	var dot, n1, n2 float64
	for i := range qVec {
		v1 := float64(qVec[i])
		v2 := float64(a.Embedding[i])
		dot += v1 * v2
		n1 += v1 * v1
		n2 += v2 * v2
	}
	if n1 <= 0 || n2 <= 0 {
		return 0, false
	}
	return 1.0 - dot/(math.Sqrt(n1)*math.Sqrt(n2)), true
}

// scoreAnchors 遍历锚点，返回 (cosineDeltaSum, driftedCount, unknownCount, knownCount)。
func (dd *DriftDetector) scoreAnchors() (cosineDeltaSum float64, driftedCount, unknownCount, knownCount int) {
	for _, a := range dd.anchors {
		if len(a.Expected) == 0 {
			unknownCount++
			continue
		}
		knownCount++
		dist, ok := dd.anchorCosineDist(a)
		if !ok {
			continue
		}
		cosineDeltaSum += dist
		if dist > dd.driftThreshold {
			driftedCount++ // 该锚点余弦距离超阈值，视为已漂移
		}
	}
	return
}

// Detect 检测嵌入向量漂移。
// 1. sampleCount < 5 → 跳过（unknownRatio=1.0，标记告警）
// 2. 对每个有 Expected 的锚点：重新 Embed → 计算余弦距离（cosineDelta）
//   - 漂移锚点占比（changeRate）
//
// 3. changeRate > 0.4 且 cosineDelta > driftThreshold → NeedsReindex=true
// 4. unknownRatio > 0.30 → 系统级告警
func (dd *DriftDetector) Detect() (*DriftReport, error) {
	if len(dd.anchors) < 5 {
		return &DriftReport{UnknownRatio: 1.0, UnknownTaskTypeAlarm: true}, nil
	}

	cosineDeltaSum, driftedCount, unknownCount, knownCount := dd.scoreAnchors()

	cosineDelta, changeRate := 0.0, 0.0
	if knownCount > 0 {
		cosineDelta = cosineDeltaSum / float64(knownCount)
		changeRate = float64(driftedCount) / float64(knownCount)
	}

	report := &DriftReport{
		UnknownRatio: float64(unknownCount) / float64(len(dd.anchors)),
		ChangeRate:   changeRate,
		CosineDelta:  cosineDelta,
		NeedsReindex: changeRate > 0.4 && cosineDelta > dd.driftThreshold,
	}
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
