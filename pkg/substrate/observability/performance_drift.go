package observability

import (
	"sync"
	"time"
)

// PerformanceDriftDetector 运行时质量漂移检测。
// 架构文档: docs/arch/M03-Observability.md §10.1
//
// 与 M12 CI RegressionDetector 互补:
//
//	M03（本模块）: 在线运行时检测，实时响应，轻量滑动窗口
//	M12:          离线 CI 检测，全量评测套件，阻断发布
//
// 触发条件: rolling window pass_rate < baseline × (1 - DriftThreshold)
// 漂移后行为: OnDrift 回调通知 + SurpriseIndex baselineShift += 0.1
type PerformanceDriftDetector struct {
	mu             sync.Mutex
	window         []float64 // 最近 windowSize 个任务的评分（0.0~1.0）
	windowSize     int
	baseline       float64 // 历史基准通过率（EWMA α=0.01 更新）
	driftThreshold float64 // 相对下降阈值（默认 0.15 = 15%）
	lastDriftAt    time.Time
	OnDrift        func(DriftAlert) // 漂移告警回调（可为 nil）
}

// DriftAlert 漂移告警数据。
type DriftAlert struct {
	DetectedAt   time.Time
	CurrentRate  float64 // 当前 rolling window 通过率
	BaselineRate float64 // 历史基准通过率
	RelativeDrop float64 // 相对下降幅度 = (baseline - current) / baseline
	WindowSize   int
}

// NewPerformanceDriftDetector 创建漂移检测器。
// windowSize: rolling window 大小（建议 50~200）。
// baseline: 初始基准通过率（0.0~1.0，无历史数据时使用）。
func NewPerformanceDriftDetector(windowSize int, baseline float64) *PerformanceDriftDetector {
	if windowSize <= 0 {
		windowSize = 100
	}
	if baseline <= 0 {
		baseline = 0.9 // 默认 90% 基准
	}
	return &PerformanceDriftDetector{
		window:         make([]float64, 0, windowSize),
		windowSize:     windowSize,
		baseline:       baseline,
		driftThreshold: 0.15,
	}
}

// Record 记录一次任务评分（1.0=成功，0.0=失败）。
// 超过 windowSize 时滑动驱逐最旧记录。
// 每次记录后检测漂移，漂移则触发 OnDrift。
func (d *PerformanceDriftDetector) Record(score float64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.window = append(d.window, score)
	if len(d.window) > d.windowSize {
		d.window = d.window[1:]
	}

	// EWMA 更新基准（α=0.01，缓慢追踪长期趋势）
	d.baseline = d.baseline*0.99 + score*0.01

	if len(d.window) < d.windowSize/2 {
		return // 窗口未满一半，不检测
	}

	current := d.windowPassRate()
	if d.baseline > 0 { //nolint:nestif
		relativeDrop := (d.baseline - current) / d.baseline
		if relativeDrop > d.driftThreshold {
			// 防止短时间内重复告警（冷却期 5 分钟）
			if time.Since(d.lastDriftAt) < 5*time.Minute {
				return
			}
			d.lastDriftAt = time.Now()
			if d.OnDrift != nil {
				d.OnDrift(DriftAlert{
					DetectedAt:   d.lastDriftAt,
					CurrentRate:  current,
					BaselineRate: d.baseline,
					RelativeDrop: relativeDrop,
					WindowSize:   len(d.window),
				})
			}
		}
	}
}

// CurrentPassRate 返回当前 rolling window 通过率（调用方持有的锁之外调用）。
func (d *PerformanceDriftDetector) CurrentPassRate() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.windowPassRate()
}

// Baseline 返回当前 EWMA 基准通过率。
func (d *PerformanceDriftDetector) Baseline() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseline
}

func (d *PerformanceDriftDetector) windowPassRate() float64 {
	if len(d.window) == 0 {
		return 1.0
	}
	var sum float64
	for _, s := range d.window {
		sum += s
	}
	return sum / float64(len(d.window))
}

// GlobalPerformanceDrift 全局默认实例（可被各模块直接引用）。
var GlobalPerformanceDrift = NewPerformanceDriftDetector(100, 0.9)
