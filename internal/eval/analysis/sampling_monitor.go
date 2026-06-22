// Package eval 实现 M12 Eval Harness 连续采样监控与回归检测。
// 架构文档: docs/arch/M12-Eval-Harness.md §9, §11
package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	samplingRate         = 0.01 // 1% 采样率
	windowSize           = 100  // 滑动窗口大小
	degradationThreshold = 0.9  // avgScore < baseline × 0.9 → alert
	monitorInterval      = 10 * time.Minute
	baselineSnapshotTTL  = 7 * 24 * time.Hour // 7 天前快照用于归因
)

// DegradationAlert 退化告警，含归因结果。
type DegradationAlert struct {
	CurrentAvg  float64
	BaselineAvg float64
	DropRate    float64 // (baseline - current) / baseline
	Attribution CausalAttribution
	DetectedAt  time.Time
}

// CausalAttribution 退化归因类型（M12 §9 归因分析）。
type CausalAttribution int

const (
	CausalUnknown            CausalAttribution = iota
	CausalInternal                             // 内部回归（触发 M9 autoRollback）
	CausalExternal                             // 外部因素（Provider 降级/网络抖动，仅 Alert）
	CausalMixed                                // 混合因素（保守抑制回滚 + HITL）
	CausalAttributionTimeout                   // 归因超时（保守策略）
)

// ContinuousSamplingMonitor 连续采样退化监控。
// 线程安全；Start() 启动后台 goroutine，每 10min 检测一次。
type ContinuousSamplingMonitor struct {
	mu           sync.Mutex
	window       []float64 // 滑动窗口（环形，最多 windowSize 条）
	windowHead   int
	windowFilled bool
	// 7 天前基线快照
	baselineAvg     float64
	sevenDayAvg     float64
	baselineTakenAt time.Time
	lastSnapshotAt  time.Time
	// 注入回调：degraded=true 时由 M9 注入冻结 Auto-Curriculum + 回滚链逻辑
	onDegradation func(alert *DegradationAlert)
}

// NewContinuousSamplingMonitor 创建监控器。onDegradation 可为 nil（仅记录日志）。
func NewContinuousSamplingMonitor(onDegradation func(*DegradationAlert)) *ContinuousSamplingMonitor {
	return &ContinuousSamplingMonitor{
		window:        make([]float64, windowSize),
		onDegradation: onDegradation,
	}
}

// RecordSample 记录一个评测分数样本（由调用方按 1% 采样率决定是否调用）。
// score 范围 [0.0, 1.0]。
func (m *ContinuousSamplingMonitor) RecordSample(score float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.window[m.windowHead] = score
	m.windowHead = (m.windowHead + 1) % windowSize
	if m.windowHead == 0 {
		m.windowFilled = true
	}
	// 首次填满窗口时记录基线快照
	if !m.windowFilled && m.windowHead == 0 {
		m.snapshotBaselineLocked()
	}
}

// CheckDegradation 检测当前窗口均值是否低于基线 × 0.9。
// 返回 (degraded, alert)；alert 仅在 degraded=true 时非 nil。
func (m *ContinuousSamplingMonitor) CheckDegradation() (bool, *DegradationAlert) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.windowFilled && m.windowHead < windowSize/2 {
		return false, nil // 样本不足
	}
	if m.baselineAvg == 0 {
		// 首次：用当前窗口作基线
		m.snapshotBaselineLocked()
		return false, nil
	}

	current := m.avgWindowLocked()
	threshold := m.baselineAvg * degradationThreshold
	if current >= threshold {
		return false, nil
	}

	// 超过 7 天则刷新基线
	if time.Since(m.baselineTakenAt) > baselineSnapshotTTL {
		m.snapshotBaselineLocked()
		return false, nil
	}

	drop := (m.baselineAvg - current) / m.baselineAvg
	attr := m.attributeLocked(current)

	alert := &DegradationAlert{
		CurrentAvg:  current,
		BaselineAvg: m.baselineAvg,
		DropRate:    drop,
		Attribution: attr,
		DetectedAt:  time.Now(),
	}
	return true, alert
}

// LoadSevenDaySnapshot 从 eval 存储加载 7 天前的通过率快照。
// 应在 Monitor 启动时调用一次，之后每 24h 刷新。
func (m *ContinuousSamplingMonitor) LoadSevenDaySnapshot(ctx context.Context, store *harness.SQLiteEvalStore) error {
	avg, err := store.GetPassRateAvgSince(ctx, time.Now().AddDate(0, 0, -7))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ContinuousSamplingMonitor.LoadSevenDaySnapshot", err)
	}
	m.mu.Lock()
	m.sevenDayAvg = avg
	m.lastSnapshotAt = time.Now()
	m.mu.Unlock()
	return nil
}

// Start 启动后台监控 goroutine，每 monitorInterval 检测一次，ctx 取消时停止。
func (m *ContinuousSamplingMonitor) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(monitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				degraded, alert := m.CheckDegradation()
				if !degraded {
					continue
				}
				slog.Warn("SilentDegradationAlert",
					"current", alert.CurrentAvg,
					"baseline", alert.BaselineAvg,
					"drop_pct", alert.DropRate*100,
					"attribution", alert.Attribution)
				if m.onDegradation != nil {
					m.onDegradation(alert)
				}
			}
		}
	}()
}

// snapshotBaselineLocked 更新基线快照（需持锁调用）。
func (m *ContinuousSamplingMonitor) snapshotBaselineLocked() {
	m.baselineAvg = m.avgWindowLocked()
	m.baselineTakenAt = time.Now()
}

// avgWindowLocked 计算当前窗口均值（需持锁调用）。
func (m *ContinuousSamplingMonitor) avgWindowLocked() float64 {
	count := windowSize
	if !m.windowFilled {
		count = m.windowHead
	}
	if count == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < count; i++ {
		sum += m.window[i]
	}
	return sum / float64(count)
}

// attributeLocked 归因分析（≤60s 超时，需持锁调用）。
// 简化实现：若基线与当前同比退化差 <5%，判定为外部因素；否则内部回归。
// 生产实现应查询 7 天前基线快照与 Provider 错误率指标。
func (m *ContinuousSamplingMonitor) attributeLocked(current float64) CausalAttribution {
	baseline := m.sevenDayAvg
	if baseline <= 0 {
		baseline = m.baselineAvg
	}
	if baseline <= 0 {
		return CausalUnknown
	}
	drop := (baseline - current) / baseline
	switch {
	case drop < 0.05:
		return CausalExternal // <5%：外部抖动，仅 Alert
	case drop < 0.15:
		return CausalMixed // 5%~15%：混合因素，保守抑制回滚 + HITL
	default:
		return CausalInternal // ≥15%：内部回归，触发 M9 autoRollback
	}
}
