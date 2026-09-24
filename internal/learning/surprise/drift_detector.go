package surprise

import (
	"context"
	"math"
	"slices"
	"sync"

	"github.com/polarisagi/polaris/internal/store/search"
)

// DriftDetector — Embedding 空间漂移检测。
// 架构文档: docs/arch/M05-Memory-System.md §12.3

// 并发模型（GR-7.1-001）：AddAnchor/RecordAnchor 由检索热路径在任意请求
// goroutine 调用，Detect/DetectByTaskType 由 DriftOrchestrator 后台周期调用。
// mu 只保护 anchors 切片本身；评分前先在锁内克隆快照，重新 Embed（可能是
// 远程调用）在锁外进行，避免检索热路径被漂移检测阻塞。
type DriftDetector struct {
	mu             sync.RWMutex
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
	dd.mu.Lock()
	defer dd.mu.Unlock()
	if len(dd.anchors) >= maxAnchors {
		// 淘汰前 20%（FIFO）：ring-buffer 语义，避免 slice 频繁分配
		drop := maxAnchors / 5
		copy(dd.anchors, dd.anchors[drop:])
		dd.anchors = dd.anchors[:len(dd.anchors)-drop]
	}
	dd.anchors = append(dd.anchors, anchor)
}

// RecordAnchor 以原始参数追加锚点，语义与 AddAnchor(AnchorSample{...}) 完全一致。
// 供 internal/memory/retrieval（L1）以消费方定义的接口方法名调用（HE-3：接口
// 在调用方定义）——L1 不允许 import L2 的 internal/learning/surprise，
// 消费方本地声明一个只含此方法签名的接口，本方法的方法名与签名需与之精确匹配。
func (dd *DriftDetector) RecordAnchor(taskType, query string, embedding []float32, expected []string) {
	dd.AddAnchor(AnchorSample{TaskType: taskType, Query: query, Embedding: embedding, Expected: expected})
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
	// 漂移检测是后台周期任务，无上游请求截止；Embedder 适配器自带 30s 上限。
	qVec := dd.embedder.Embed(context.Background(), a.Query)
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

// snapshot 在读锁内克隆锚点切片（元素为值类型，其内部切片只读不改，浅拷贝即可）。
func (dd *DriftDetector) snapshot() []AnchorSample {
	dd.mu.RLock()
	defer dd.mu.RUnlock()
	return slices.Clone(dd.anchors)
}

// scoreAnchors 遍历给定锚点快照，返回 (cosineDeltaSum, driftedCount, unknownCount, knownCount)。
// Detect 与 DetectByTaskType（按组传入子集）复用同一套评分逻辑。
func (dd *DriftDetector) scoreAnchors(anchors []AnchorSample) (cosineDeltaSum float64, driftedCount, unknownCount, knownCount int) {
	for _, a := range anchors {
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
	anchors := dd.snapshot()
	if len(anchors) < 5 {
		return &DriftReport{UnknownRatio: 1.0, UnknownTaskTypeAlarm: true}, nil
	}

	cosineDeltaSum, driftedCount, unknownCount, knownCount := dd.scoreAnchors(anchors)

	cosineDelta, changeRate := 0.0, 0.0
	if knownCount > 0 {
		cosineDelta = cosineDeltaSum / float64(knownCount)
		changeRate = float64(driftedCount) / float64(knownCount)
	}

	report := &DriftReport{
		UnknownRatio: float64(unknownCount) / float64(len(anchors)),
		ChangeRate:   changeRate,
		CosineDelta:  cosineDelta,
		NeedsReindex: changeRate > 0.4 && cosineDelta > dd.driftThreshold,
	}
	if report.UnknownRatio > 0.30 {
		report.UnknownTaskTypeAlarm = true
	}
	return report, nil
}

// DetectByTaskType 按 task_type 分组检测漂移。
// M05 §12.3 降级表要求"该 task_type 降级纯 BM25，其余不受影响"——原 Detect()
// 只做全局聚合，AnchorSample.TaskType 字段从未被读取过（2026-07-21 deadcode
// 审查发现的设计缺口）。本方法把分组子集传给 scoreAnchors 评分，
// 判定阈值与 Detect() 保持一致（changeRate>0.4 且 cosineDelta>threshold）。
// 样本数 <5 的组跳过（不产出降级信号，避免小样本噪声误判）。
func (dd *DriftDetector) DetectByTaskType() map[string]*DriftReport {
	byType := make(map[string][]AnchorSample)
	for _, a := range dd.snapshot() {
		if a.TaskType == "" {
			continue
		}
		byType[a.TaskType] = append(byType[a.TaskType], a)
	}

	reports := make(map[string]*DriftReport, len(byType))
	for taskType, anchors := range byType {
		if len(anchors) < 5 {
			continue
		}
		cosineDeltaSum, driftedCount, unknownCount, knownCount := dd.scoreAnchors(anchors)
		cosineDelta, changeRate := 0.0, 0.0
		if knownCount > 0 {
			cosineDelta = cosineDeltaSum / float64(knownCount)
			changeRate = float64(driftedCount) / float64(knownCount)
		}
		report := &DriftReport{
			UnknownRatio: float64(unknownCount) / float64(len(anchors)),
			ChangeRate:   changeRate,
			CosineDelta:  cosineDelta,
			NeedsReindex: changeRate > 0.4 && cosineDelta > dd.driftThreshold,
		}
		if report.UnknownRatio > 0.30 {
			report.UnknownTaskTypeAlarm = true
		}
		reports[taskType] = report
	}
	return reports
}

// DriftReport 漂移检测报告。
type DriftReport struct {
	NeedsReindex         bool
	ChangeRate           float64
	CosineDelta          float64
	UnknownRatio         float64
	UnknownTaskTypeAlarm bool
}
