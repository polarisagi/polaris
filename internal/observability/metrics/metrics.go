package metrics

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// GlobalSurpriseIndex 全局 SurpriseIndex 单例，sync.OnceValue 惰性初始化。
	// 调用方用 GlobalSurpriseIndex() 而非 GlobalSurpriseIndex 直接访问实例。
	GlobalSurpriseIndex = sync.OnceValue(func() *SurpriseIndex { return NewSurpriseIndex() })

	// GlobalKillswitchStage 全局 KillSwitch 阶段原子量（0=Normal…3=FullStop）。
	// killswitch.go 的 StateChangeCallback 写入；M13 handleStatus 读取。
	GlobalKillswitchStage atomic.Int32

	// GlobalCedarDegradedTotal tracks the number of times Cedar FFI evaluation failed.
	GlobalCedarDegradedTotal atomic.Int64

	// GlobalOutboxDeadLetterTotal tracks the number of outbox records marked as dead.
	GlobalOutboxDeadLetterTotal atomic.Int64

	// GlobalFactualityJudgeUnavailableTotal 记录 L3 SemanticJudge 因超时/故障静默降级的累计次数。
	// factuality_guard.go semanticJudge() 在 llm_judge_unavailable 时写入。
	// OTel gauge 在 RegisterMetrics() 的 RegisterCallback 中注册。
	GlobalFactualityJudgeUnavailableTotal atomic.Int64

	// GlobalBlindZoneRoutingTotal 因 BlindZone 检测强制升级为 System2 的累计次数（V8-S4）。
	GlobalBlindZoneRoutingTotal atomic.Int64
)

// foundingAnchorDriftScorePtr 以 atomic.Pointer 持有漂移评分注入函数，避免包级可变 var。
// atomic.Pointer[T] 零值为 nil，Load() 安全返回 nil，无需 init()。
var foundingAnchorDriftScorePtr atomic.Pointer[func() float64]

// SetFoundingAnchorDriftScorer 供 main.go 在启动时注入真实漂移评分实现（避免包循环依赖）。
func SetFoundingAnchorDriftScorer(fn func() float64) {
	foundingAnchorDriftScorePtr.Store(&fn)
}

// GetFoundingAnchorDriftScore 返回创始锚点漂移评分（委托给注入函数）。
// 若实现未注入（测试/冷启动场景），安全返回 0.0。
func GetFoundingAnchorDriftScore() float64 {
	if p := foundingAnchorDriftScorePtr.Load(); p != nil {
		return (*p)()
	}
	return 0.0
}

// TokenBurnRate tracks token consumption rate for circuit breaking.
// 架构文档: docs/arch/M03-Observability-深度选型.md §3
type TokenBurnRate struct {
	cumulativeTokens atomic.Int64
	lastTick         time.Time
	lastTokens       int64

	ema5s  float64
	ema30s float64

	baselineP95 float64
	callCount   atomic.Int64

	mu sync.RWMutex
}

func NewTokenBurnRate() *TokenBurnRate {
	return &TokenBurnRate{
		lastTick:    time.Now(),
		baselineP95: 200.0, // 冷启动保护值
	}
}

func (tbr *TokenBurnRate) Add(tokens int64) {
	tbr.cumulativeTokens.Add(tokens)
	tbr.callCount.Add(1)
}

// Tick updates the EMA rates. Must be called periodically (e.g., every 1s).
func (tbr *TokenBurnRate) Tick() {
	tbr.mu.Lock()
	defer tbr.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tbr.lastTick).Seconds()
	if elapsed <= 0 {
		return
	}

	currentTokens := tbr.cumulativeTokens.Load()
	deltaTokens := currentTokens - tbr.lastTokens
	instantRate := float64(deltaTokens) / elapsed

	// α=0.33 for ~5s window
	tbr.ema5s = (0.33 * instantRate) + (1-0.33)*tbr.ema5s
	// α=0.06 for ~30s window
	tbr.ema30s = (0.06 * instantRate) + (1-0.06)*tbr.ema30s

	tbr.lastTokens = currentTokens
	tbr.lastTick = now

	// 动态基线学习：callCount >= 100 后启用 EWMA 更新 baselineP95。
	// α=0.05 对应约 20 次 Tick 平滑（1s 周期下约 20s 稳定），冷启动保留 200.0 兜底。
	if tbr.callCount.Load() >= 100 {
		if tbr.ema30s > tbr.baselineP95 {
			// 上行：快速跟随（防止误限速）
			tbr.baselineP95 = 0.2*tbr.ema30s + 0.8*tbr.baselineP95
		} else {
			// 下行：慢速收缩（防止因短时低谷使基线过低）
			tbr.baselineP95 = 0.02*tbr.ema30s + 0.98*tbr.baselineP95
		}
		// 下界保护：不低于 50 token/s（避免基线学成 0 导致永久限速）
		if tbr.baselineP95 < 50.0 {
			tbr.baselineP95 = 50.0
		}
	}
}

type ThrottleStage int

const (
	ThrottleNormal ThrottleStage = 0
	ThrottleStage1 ThrottleStage = 1 // THROTTLE
	ThrottleStage2 ThrottleStage = 2 // HARD STOP
	ThrottleStage3 ThrottleStage = 3 // FULLSTOP
)

// EMA5s 返回 5s 窗口 EMA 速率（token/s），供 /metrics 暴露。
func (tbr *TokenBurnRate) EMA5s() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.ema5s
}

// EMA30s 返回 30s 窗口 EMA 速率（token/s），供 /metrics 暴露。
func (tbr *TokenBurnRate) EMA30s() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.ema30s
}

// BaselineP95 返回动态学习的 P95 基线速率（token/s）。
// 供 ResourceBudget.BackgroundPermit 门控后台任务使用（C1.2）。
func (tbr *TokenBurnRate) BaselineP95() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.baselineP95
}

func (tbr *TokenBurnRate) CheckThrottle() ThrottleStage {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()

	// 动态基线（callCount>=100 后由 Tick() EWMA 更新；冷启动兜底 200.0）
	limit := math.Max(tbr.baselineP95, 50.0)

	switch {
	case tbr.ema30s > limit*10.0:
		return ThrottleStage3
	case tbr.ema30s > limit*3.0:
		return ThrottleStage2
	case tbr.ema5s > limit*2.0:
		return ThrottleStage1
	default:
		return ThrottleNormal
	}
}

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

// DecisionLog records a single routing decision for offline analysis.
type DecisionLog struct {
	Timestamp     time.Time `json:"timestamp"`
	Route         string    `json:"route"`
	SurpriseIndex float64   `json:"surprise_index"`
	Provider      string    `json:"provider"`
	Reason        string    `json:"reason"`
}

type DecisionLogStore interface {
	Append(ctx context.Context, log DecisionLog) error
}

type DecisionLogger struct {
	mu    sync.Mutex
	store DecisionLogStore
}

// NewDecisionLogger 创建新的决策日志记录器。
func NewDecisionLogger(store DecisionLogStore) *DecisionLogger {
	return &DecisionLogger{
		store: store,
	}
}

// Log 记录一条决策日志。
func (dl *DecisionLogger) Log(ctx context.Context, log DecisionLog) error {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.store == nil {
		return nil
	}
	return dl.store.Append(ctx, log)
}
