package swarm

import (
	"math"
	"sync"
)

// MarkovMatrix SurpriseIndex Layer B：工具序列马尔可夫转移矩阵。
//
// 演进条件（M09 §2.0 Layer B 演进路径）：500+ 成功轨迹后注入，与时间无关。
// 在此之前 matrix 为 nil，SurpriseCalculator 自动降级到 Tier-0 启发式基线。
//
// 算法：
//   - P[from][to] 为历史转移概率（Laplace 平滑，prior=1e-4）
//   - Surprise(seq) = mean(-log P(t_i | t_{i-1})) / log(vocabSize+1)，归一化到 [0,1]
type MarkovMatrix struct {
	mu sync.RWMutex
	// counts[from][to] = 观测到的转移次数（Laplace 计数）
	counts map[string]map[string]float64
	// totals[from] = from 工具的总出发次数
	totals map[string]float64
	// vocab 已见工具集合，用于 Laplace 平滑分母
	vocab map[string]struct{}
}

// NewMarkovMatrix 构造空矩阵（uniform prior）。
func NewMarkovMatrix() *MarkovMatrix {
	return &MarkovMatrix{
		counts: make(map[string]map[string]float64),
		totals: make(map[string]float64),
		vocab:  make(map[string]struct{}),
	}
}

// Update 用一条新工具序列在线更新转移计数（流式增量，无需批量重建）。
func (m *MarkovMatrix) Update(seq []string) {
	if len(seq) < 2 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range seq {
		m.vocab[t] = struct{}{}
	}
	for i := 1; i < len(seq); i++ {
		from, to := seq[i-1], seq[i]
		if m.counts[from] == nil {
			m.counts[from] = make(map[string]float64)
		}
		m.counts[from][to]++
		m.totals[from]++
	}
}

// TransitionProb 返回 P(to | from)，含 Laplace 平滑（α=1）。
// 未见 from 时返回 prior = 1/(|V|+1)。
func (m *MarkovMatrix) TransitionProb(from, to string) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vocabSize := float64(len(m.vocab))
	if vocabSize == 0 {
		return 0.5 // 无数据时返回中性值
	}
	count := m.counts[from][to] // 0 if missing
	total := m.totals[from]     // 0 if missing
	// Laplace smoothing: (count + 1) / (total + |V|)
	return (count + 1) / (total + vocabSize)
}

// Surprise 计算工具序列的马尔可夫惊异值，归一化到 [0, 1]。
// 短序列（<2）返回中性值 0.5。
func (m *MarkovMatrix) Surprise(seq []string) float64 {
	if len(seq) < 2 {
		return 0.5
	}
	m.mu.RLock()
	vocabSize := float64(len(m.vocab))
	m.mu.RUnlock()

	if vocabSize == 0 {
		return 0.5 // 无历史数据：中性惊异值，不干扰 Layer A 权重
	}

	// 归一化因子：最大惊异值 = -log(prior) = log(vocabSize)
	maxSurprise := math.Log(vocabSize + 1)
	if maxSurprise == 0 {
		return 0.5
	}

	var total float64
	for i := 1; i < len(seq); i++ {
		p := m.TransitionProb(seq[i-1], seq[i])
		// -log(p) / maxSurprise ∈ [0, 1]
		total += -math.Log(p) / maxSurprise
	}
	raw := total / float64(len(seq)-1)

	// 截断到 [0, 1]（Laplace 平滑后理论上已在范围内，但防浮点溢出）
	if raw < 0 {
		return 0
	}
	if raw > 1 {
		return 1
	}
	return raw
}

// VocabSize 返回已见工具词汇量（用于监控数据积累进度）。
func (m *MarkovMatrix) VocabSize() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.vocab)
}

// TotalTransitions 返回总记录转移数（用于判断是否达到"足够数据"门槛）。
func (m *MarkovMatrix) TotalTransitions() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total float64
	for _, t := range m.totals {
		total += t
	}
	return total
}
