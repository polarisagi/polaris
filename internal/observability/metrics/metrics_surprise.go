package metrics

// 2026-08-02 从 metrics.go 拆分（Test_inv_FileLineLimit R7 400 行上限存量债务，
// 见 local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现），纯搬运无行为变更。

import (
	"context"
	"math"
	"sync"
	"time"
)

// SurpriseIndex measures trajectory deviation from historical successes.
// 基础版实现 (两组件: embedding + tool sequence).
// 架构文档: docs/arch/M03-Observability-深度选型.md §4.0
type SurpriseIndex struct {
	mu              sync.RWMutex
	lastValue       float64
	staleness       time.Time
	historicalEmbed []float64
	historicalTools map[string]int
	callCount       int
	// [Task 13] 上一次工具调用序列，用于 Levenshtein 序列距离计算。
	// Jaccard 是集合相似度（丢失顺序信息），Levenshtein 是编辑距离（保留顺序）。
	lastToolSeq []string
}

func NewSurpriseIndex() *SurpriseIndex {
	return &SurpriseIndex{
		lastValue:       0.5,
		staleness:       time.Now(),
		historicalTools: make(map[string]int),
	}
}

// ComputeBasic calculates the basic Phase 1 surprise index.
// [Task 13] 升级：工具调用序列相似度改用 Levenshtein 编辑距离（替代 Jaccard 集合距离），
// 能捕捉双用序列 [A,B,C] vs [C,B,A] 这类顺序差异。冷启动鈰倠从 callCount<3 放宽到 callCount<10。
func (si *SurpriseIndex) ComputeBasic(ctx context.Context, embedding []float64, toolSeq []string) float64 {
	si.mu.Lock()
	defer si.mu.Unlock()

	si.staleness = time.Now()
	si.callCount++
	if si.historicalTools == nil {
		si.historicalTools = make(map[string]int)
	}

	cosineDist := si.computeCosineDist(embedding)
	// [Task 13] 使用 Levenshtein 序列距离替代 Jaccard 集合距离。
	seqDist := si.computeLevenshteinDist(toolSeq)

	if si.callCount > 100 {
		for k, v := range si.historicalTools {
			newV := int(float64(v) * 0.95)
			if newV == 0 {
				delete(si.historicalTools, k)
			} else {
				si.historicalTools[k] = newV
			}
		}
	}

	// [Task 13] 冷启动鈰倠从 3 放宽到 10：早期样本不足时，序列距离算法根据单一样本得到的距离可能具不具参考价値。
	if si.callCount < 10 {
		si.lastValue = 0.5
	} else {
		// 权重: 嵌入下语义距离 70%，序列编辑距离 30%。
		si.lastValue = 0.7*cosineDist + 0.3*seqDist
	}

	// 更新历史序列
	si.lastToolSeq = make([]string, len(toolSeq))
	copy(si.lastToolSeq, toolSeq)
	for t := range toolSeq {
		si.historicalTools[toolSeq[t]]++
	}

	return si.lastValue
}

func (si *SurpriseIndex) computeCosineDist(embedding []float64) float64 {
	cosineDist := 0.0
	if len(embedding) == 0 {
		return cosineDist
	}
	if len(si.historicalEmbed) != len(embedding) {
		si.historicalEmbed = make([]float64, len(embedding))
		copy(si.historicalEmbed, embedding)
	} else {
		var dot, n1, n2 float64
		for i, v := range embedding {
			// EMA alpha=0.1
			si.historicalEmbed[i] = 0.9*si.historicalEmbed[i] + 0.1*v
			dot += v * si.historicalEmbed[i]
			n1 += v * v
			n2 += si.historicalEmbed[i] * si.historicalEmbed[i]
		}
		if n1 > 0 && n2 > 0 {
			cosineSim := dot / (math.Sqrt(n1) * math.Sqrt(n2))
			cosineDist = 1.0 - cosineSim
		}
	}
	return cosineDist
}

// computeLevenshteinDist 计算工具调用序列与历史序列之间的归一化编辑距离。
// 返回 [0,1]：0 表示序列完全相同，1 表示完全不同。
// [Task 13] Levenshtein 序列距离：能区分 [A,B,C] vs [C,B,A]（Jaccard 两者相同）。
func (si *SurpriseIndex) computeLevenshteinDist(toolSeq []string) float64 {
	prev := si.lastToolSeq
	if len(prev) == 0 {
		// 没有历史序列时，返回最大距离（连接第一次调用必然差异化）
		return 1.0
	}
	m, n := len(prev), len(toolSeq)
	// dp[i][j] = prev[:i] 到 toolSeq[:j] 的编辑距离
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
		dp[i][0] = i
	}
	for j := 1; j <= n; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if prev[i-1] == toolSeq[j-1] {
				dp[i][j] = dp[i-1][j-1]
			} else {
				dp[i][j] = 1 + min3(dp[i-1][j], dp[i][j-1], dp[i-1][j-1])
			}
		}
	}
	// 归一化：除以最大可能距离（max(m,n)）
	maxLen := m
	if n > maxLen {
		maxLen = n
	}
	if maxLen == 0 {
		return 0.0
	}
	return float64(dp[m][n]) / float64(maxLen)
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// Current 返回最近一次计算的 SurpriseIndex 值，供 /metrics 暴露。
func (si *SurpriseIndex) Current() float64 {
	si.mu.RLock()
	defer si.mu.RUnlock()
	return si.lastValue
}

// SetLastValue 由外部（SurpriseCalculator）写入计算结果，供 SelectThinkingMode 读取。
// 线程安全：与 ComputeBasic 使用同一 mu 锁。
func (si *SurpriseIndex) SetLastValue(v float64) {
	si.mu.Lock()
	si.lastValue = v
	si.mu.Unlock()
}

func (si *SurpriseIndex) IsStale() bool {
	si.mu.RLock()
	defer si.mu.RUnlock()
	// Staleness > 120s -> true
	return time.Since(si.staleness).Seconds() > 120
}

// InjectFaultSignal raises the SurpriseIndex forcibly when an OS-level fault is detected.
func (si *SurpriseIndex) InjectFaultSignal(severity float64) {
	si.mu.Lock()
	defer si.mu.Unlock()
	si.lastValue += severity
	if si.lastValue > 1.0 {
		si.lastValue = 1.0
	}
}
