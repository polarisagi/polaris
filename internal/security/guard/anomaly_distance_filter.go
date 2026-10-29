package guard

import (
	"math"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AnomalyDistanceFilter 马氏距离异常检测（OWASP LLM08 - M11 §2.2）。
// 架构文档: docs/arch/M11-Policy-Safety.md §2.2
//
// 实现策略：
//   - 对每个 task_type 维护特征向量的在线均值 μ 和方差 σ²（Welford 算法，O(1) 内存）。
//   - 对角近似：使用各特征维度独立 z-score 之平方和代替完整马氏距离（避免协方差矩阵求逆）。
//     等价于假设特征维度相互独立；对缺乏历史数据的新 task_type 自动 bypass（样本数 < minSamples）。
//   - 阈值: 3.0σ（可配置），超限 → ErrAnomalyDetected + TaintHigh。
const (
	adfDefaultSigmaThreshold = 3.0
	adfMinSamples            = 30 // 样本数不足时 bypass，避免冷启动误拦
)

var (
	ErrAnomalyDetected = apperr.New(apperr.CodeForbidden, "anomaly_distance_filter: input vector anomalous (>3σ), blocked")
)

// featureStats 单 task_type 单维度的在线统计（Welford 算法）。
type featureStats struct {
	n    float64 // 样本数
	mean float64
	m2   float64 // 方差分子（sum of squared deviations）
}

func (fs *featureStats) update(x float64) {
	fs.n++
	delta := x - fs.mean
	fs.mean += delta / fs.n
	fs.m2 += delta * (x - fs.mean)
}

func (fs *featureStats) variance() float64 {
	if fs.n < 2 {
		return 1.0 // 避免除零
	}
	return fs.m2 / (fs.n - 1)
}

// AnomalyDistanceFilter 对 LLM 接收外部请求前进行异常检测。
// 调用方在将外部非结构化输入传入 Infer 前调用 Check；同时通过 Record 持续更新历史分布。
type AnomalyDistanceFilter struct {
	mu             sync.RWMutex
	stats          map[string][]featureStats // task_type → 各维度统计
	sigmaThreshold float64
	minSamples     int
}

// NewAnomalyDistanceFilter 创建 AnomalyDistanceFilter；sigmaThreshold=0 使用默认 3.0。
func NewAnomalyDistanceFilter(sigmaThreshold float64) *AnomalyDistanceFilter {
	if sigmaThreshold <= 0 {
		sigmaThreshold = adfDefaultSigmaThreshold
	}
	return &AnomalyDistanceFilter{
		stats:          make(map[string][]featureStats),
		sigmaThreshold: sigmaThreshold,
		minSamples:     adfMinSamples,
	}
}

// Record 将一个新特征向量更新到 task_type 的历史分布（正常样本训练阶段调用）。
func (f *AnomalyDistanceFilter) Record(taskType string, vec []float64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	s := f.stats[taskType]
	if len(s) != len(vec) {
		s = make([]featureStats, len(vec))
	}
	for i, v := range vec {
		s[i].update(v)
	}
	f.stats[taskType] = s
}

// Check 检测输入向量是否超出 task_type 历史分布的 3σ 边界。
// 返回 (taintLevel, error)：
//   - 正常：(TaintNone, nil)
//   - 历史样本不足（冷启动 bypass）：(TaintMedium, nil)（默认打上 medium 标记，不拦截）
//   - 异常：(TaintHigh, ErrAnomalyDetected)
func (f *AnomalyDistanceFilter) Check(taskType string, vec []float64) (types.TaintLevel, error) {
	f.mu.RLock()
	s := f.stats[taskType]
	f.mu.RUnlock()

	if len(s) == 0 || len(s) != len(vec) {
		// 未知 task_type 或维度不匹配：bypass，保守打 TaintMedium
		return types.TaintMedium, nil
	}

	// 冷启动保护：样本数不足 minSamples bypass
	if s[0].n < float64(f.minSamples) {
		return types.TaintMedium, nil
	}

	// 对角马氏距离（z-score 平方和，各维度独立假设）
	var sumSq float64
	for i, v := range vec {
		sigma := math.Sqrt(s[i].variance())
		if sigma < 1e-9 {
			continue // 方差近零维度跳过
		}
		z := (v - s[i].mean) / sigma
		sumSq += z * z
	}
	// 综合 σ = sqrt(sumSq / dims) — 对角 Mahalanobis 近似
	dims := float64(len(vec))
	if dims == 0 {
		return types.TaintNone, nil
	}
	distSigma := math.Sqrt(sumSq / dims)

	if distSigma > f.sigmaThreshold {
		return types.TaintHigh, ErrAnomalyDetected
	}
	return types.TaintNone, nil
}
