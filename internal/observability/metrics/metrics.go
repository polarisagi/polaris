package metrics

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
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
}

func NewSurpriseIndex() *SurpriseIndex {
	return &SurpriseIndex{
		lastValue:       0.5,
		staleness:       time.Now(),
		historicalTools: make(map[string]int),
	}
}

// ComputeBasic calculates the basic Phase 0.1 surprise index.
func (si *SurpriseIndex) ComputeBasic(ctx context.Context, embedding []float64, toolSeq []string) float64 {
	si.mu.Lock()
	defer si.mu.Unlock()

	si.staleness = time.Now()
	si.callCount++
	if si.historicalTools == nil {
		si.historicalTools = make(map[string]int)
	}

	cosineDist := si.computeCosineDist(embedding)
	jaccardDist := si.computeJaccardDist(toolSeq)

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

	if si.callCount < 3 {
		si.lastValue = 0.5
	} else {
		// α=0.7, β=0.3
		si.lastValue = 0.7*cosineDist + 0.3*jaccardDist
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

func (si *SurpriseIndex) computeJaccardDist(toolSeq []string) float64 {
	seen := make(map[string]bool)
	intersection := 0
	union := len(si.historicalTools)
	for _, t := range toolSeq {
		if !seen[t] {
			seen[t] = true
			if si.historicalTools[t] > 0 {
				intersection++
			} else {
				union++
			}
		}
	}
	jaccardDist := 0.0
	if union > 0 {
		jaccardDist = 1.0 - float64(intersection)/float64(union)
	}

	for t := range seen {
		si.historicalTools[t]++
	}
	return jaccardDist
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

// ============================================================================
// polaris_surrealdb_index_size_mb — Prometheus Gauge
// 覆盖 [Storage-SurrealDB-Core] HNSW + BM25 + 图索引总内存占用。
// 架构文档: docs/arch/M02-Storage-Fabric.md §3
// ============================================================================

var PolarisSurrealDBIndexSizeMB atomic.Int64

// ReportSurrealDBIndexSize 设置当前 [Storage-SurrealDB-Core] 索引的内存占用（MB）。
// 由 SurrealDB-Core FFI 的定期监控 goroutine 调用。
func ReportSurrealDBIndexSize(sizeMB int64) {
	PolarisSurrealDBIndexSizeMB.Store(sizeMB)
}

// MetricsHandler 返回 Prometheus 文本格式的 /metrics HTTP Handler。
// 暴露的指标（HE-Rule-1 一等公民）:
//
//	polaris_token_burn_rate_ema5s_tps       — 5s 滑动窗口 EMA token 速率（token/s）
//	polaris_token_burn_rate_ema30s_tps      — 30s 滑动窗口 EMA token 速率（token/s）
//	polaris_token_burn_rate_total           — 累计消耗 token 数
//	polaris_token_burn_rate_throttle_stage  — 当前熔断阶段 0=Normal 1=THROTTLE 2=HARDSTOP 3=FULLSTOP
//	polaris_surprise_index                  — 当前 SurpriseIndex（0.0~1.0）
//	polaris_surprise_index_stale            — SurpriseIndex 是否过期（1=过期 >120s）
//	polaris_surrealdb_index_size_mb         — SurrealDB-Core 索引内存占用（Gauge）
//
// 所有 gauge 不带 label（MVP 简化版；Tier 1+ 升级为 promhttp.Handler + 标准 OTel 维度）
func MetricsHandler(tbr *TokenBurnRate) http.Handler {
	if fg := probe.GlobalFeatureGate(); fg != nil && fg.IsEnabled(probe.FeatureOTelExporter) {
		return otelMetricsHandler(tbr)
	}
	return legacyMetricsHandler(tbr)
}

var (
	otelOnce       sync.Once
	otelHandlerPtr atomic.Pointer[http.Handler] // 零值 nil，Load() 安全
)

func getHostname() string {
	h, _ := os.Hostname()
	return h
}

func otelMetricsHandler(tbr *TokenBurnRate) http.Handler {
	otelOnce.Do(func() {
		exporter, err := prometheus.New()
		if err != nil {
			slog.Warn("observability: failed to initialize prometheus exporter", "err", err)
			return
		}
		res := resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("polaris"),
			semconv.ServiceVersion(config.BuildVersion),
			semconv.HostName(getHostname()),
		)
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(exporter),
			sdkmetric.WithResource(res),
		)
		meter := provider.Meter("github.com/polarisagi/polaris/internal/observability")

		InitMetrics(meter)

		ema5sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema5s_tps")
		ema30sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema30s_tps")
		totalCounter, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.total")
		throttleGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.throttle_stage")
		surpriseGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index")
		surpriseBasicGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index_basic")
		surpriseStaleGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index.stale")
		surrealSizeGauge, _ := meter.Float64ObservableGauge("polaris.surrealdb.index_size_mb")
		killswitchGauge, _ := meter.Float64ObservableGauge("polaris.killswitch.stage")
		cedarDegradedGauge, _ := meter.Float64ObservableGauge("polaris.cedar.degraded_total")
		outboxDeadLetterGauge, _ := meter.Float64ObservableGauge("polaris.outbox.dead_letter_total")
		factualityJudgeUnavailableGauge, _ := meter.Float64ObservableGauge("polaris.factuality.judge_unavailable_total")

		// V8-S4: BlindZone 路由计数
		blindZoneGauge, _ := meter.Float64ObservableGauge(
			"polaris.blind_zone.routing_total",
			metric.WithDescription("因 BlindZone 检测强制升级为 System2 的累计次数"),
		)

		// V8-S3: 创始锚点漂移评分
		// 注意：若 policy 包导入形成循环，使用函数变量注入（见 §3.3 循环依赖处理）
		anchorDriftGauge, _ := meter.Float64ObservableGauge(
			"polaris.founding_anchor.drift_score",
			metric.WithDescription("与创始行为锚点的综合漂移评分 [0,1]"),
		)

		_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveFloat64(ema5sGauge, tbr.EMA5s())
			o.ObserveFloat64(ema30sGauge, tbr.EMA30s())
			o.ObserveFloat64(totalCounter, float64(tbr.cumulativeTokens.Load()))
			o.ObserveFloat64(throttleGauge, float64(tbr.CheckThrottle()))

			si := GlobalSurpriseIndex()
			o.ObserveFloat64(surpriseGauge, si.Current())
			o.ObserveFloat64(surpriseBasicGauge, si.Current())
			staleVal := 0.0
			if si.IsStale() {
				staleVal = 1.0
			}
			o.ObserveFloat64(surpriseStaleGauge, staleVal)

			ls := PolarisSurrealDBIndexSizeMB.Load()
			o.ObserveFloat64(surrealSizeGauge, float64(ls))

			o.ObserveFloat64(killswitchGauge, float64(GlobalKillswitchStage.Load()))
			o.ObserveFloat64(cedarDegradedGauge, float64(GlobalCedarDegradedTotal.Load()))
			o.ObserveFloat64(outboxDeadLetterGauge, float64(GlobalOutboxDeadLetterTotal.Load()))
			o.ObserveFloat64(factualityJudgeUnavailableGauge, float64(GlobalFactualityJudgeUnavailableTotal.Load()))

			o.ObserveFloat64(blindZoneGauge, float64(GlobalBlindZoneRoutingTotal.Load()))
			o.ObserveFloat64(anchorDriftGauge, GetFoundingAnchorDriftScore())

			return nil
		}, ema5sGauge, ema30sGauge, totalCounter, throttleGauge, surpriseGauge, surpriseBasicGauge, surpriseStaleGauge, surrealSizeGauge, killswitchGauge, cedarDegradedGauge, outboxDeadLetterGauge, factualityJudgeUnavailableGauge, blindZoneGauge, anchorDriftGauge)

		h := promhttp.Handler()
		otelHandlerPtr.Store(&h)
	})
	if h := otelHandlerPtr.Load(); h != nil {
		return *h
	}
	return legacyMetricsHandler(tbr)
}

func legacyMetricsHandler(tbr *TokenBurnRate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// ── TokenBurnRate（HE-Rule-1 一等公民，必须在 /metrics 可见）──────────────
		ema5 := tbr.EMA5s()
		ema30 := tbr.EMA30s()
		cumTokens := tbr.cumulativeTokens.Load()
		throttleStage := int(tbr.CheckThrottle())

		fmt.Fprintf(w, "# HELP polaris_token_burn_rate_ema5s_tps Token burn rate 5s EMA (tokens/s)\n")
		fmt.Fprintf(w, "# TYPE polaris_token_burn_rate_ema5s_tps gauge\n")
		fmt.Fprintf(w, "polaris_token_burn_rate_ema5s_tps %g\n", ema5)

		fmt.Fprintf(w, "# HELP polaris_token_burn_rate_ema30s_tps Token burn rate 30s EMA (tokens/s)\n")
		fmt.Fprintf(w, "# TYPE polaris_token_burn_rate_ema30s_tps gauge\n")
		fmt.Fprintf(w, "polaris_token_burn_rate_ema30s_tps %g\n", ema30)

		fmt.Fprintf(w, "# HELP polaris_token_burn_rate_total Cumulative tokens consumed\n")
		fmt.Fprintf(w, "# TYPE polaris_token_burn_rate_total counter\n")
		fmt.Fprintf(w, "polaris_token_burn_rate_total %d\n", cumTokens)

		fmt.Fprintf(w, "# HELP polaris_token_burn_rate_throttle_stage Current throttle stage (0=Normal 1=THROTTLE 2=HARDSTOP 3=FULLSTOP)\n")
		fmt.Fprintf(w, "# TYPE polaris_token_burn_rate_throttle_stage gauge\n")
		fmt.Fprintf(w, "polaris_token_burn_rate_throttle_stage %d\n", throttleStage)

		// ── SurpriseIndex（HE-Rule-1 一等公民）──────────────────────────────────
		si := GlobalSurpriseIndex()
		siVal := si.Current()
		siStale := 0
		if si.IsStale() {
			siStale = 1
		}

		fmt.Fprintf(w, "# HELP polaris_surprise_index Current surprise index (0.0~1.0)\n")
		fmt.Fprintf(w, "# TYPE polaris_surprise_index gauge\n")
		fmt.Fprintf(w, "polaris_surprise_index %g\n", siVal)

		fmt.Fprintf(w, "# HELP polaris_surprise_index_basic Current surprise index basic (no labels) (0.0~1.0)\n")
		fmt.Fprintf(w, "# TYPE polaris_surprise_index_basic gauge\n")
		fmt.Fprintf(w, "polaris_surprise_index_basic %g\n", siVal)

		fmt.Fprintf(w, "# HELP polaris_surprise_index_stale Whether surprise index is stale (1=stale >120s)\n")
		fmt.Fprintf(w, "# TYPE polaris_surprise_index_stale gauge\n")
		fmt.Fprintf(w, "polaris_surprise_index_stale %d\n", siStale)

		// ── SurrealDB 索引大小 ──────────────────────────────────────────────────
		ls := PolarisSurrealDBIndexSizeMB.Load()
		fmt.Fprintf(w, "# HELP polaris_surrealdb_index_size_mb SurrealDB-Core index memory usage\n")
		fmt.Fprintf(w, "# TYPE polaris_surrealdb_index_size_mb gauge\n")
		fmt.Fprintf(w, "polaris_surrealdb_index_size_mb %d\n", ls)

		// ── Cedar Degraded Total ──────────────────────────────────────────────────
		cd := GlobalCedarDegradedTotal.Load()
		fmt.Fprintf(w, "# HELP polaris_cedar_degraded_total Total number of Cedar FFI evaluation failures\n")
		fmt.Fprintf(w, "# TYPE polaris_cedar_degraded_total counter\n")
		fmt.Fprintf(w, "polaris_cedar_degraded_total %d\n", cd)

		// ── KillSwitch Stage ──────────────────────────────────────────────────
		stage := GlobalKillswitchStage.Load()
		fmt.Fprintf(w, "# HELP polaris_killswitch_stage Current M13 KillSwitch stage\n")
		fmt.Fprintf(w, "# TYPE polaris_killswitch_stage gauge\n")
		fmt.Fprintf(w, "polaris_killswitch_stage %d\n", stage)

		// ── Outbox Dead Letters ────────────────────────────────────────────────
		dl := GlobalOutboxDeadLetterTotal.Load()
		fmt.Fprintf(w, "# HELP polaris_outbox_dead_letter_total Total number of outbox messages dead\n")
		fmt.Fprintf(w, "# TYPE polaris_outbox_dead_letter_total counter\n")
		fmt.Fprintf(w, "polaris_outbox_dead_letter_total %d\n", dl)
	})
}

// SelectThinkingMode 根据当前系统的运行时状态，决定应该使用哪个档位的 ThinkingMode。
// 规则：
// 1. 若重规划次数 > 0，或任务最大污点等级 >= 3（TaintHigh），或 SurpriseIndex > 0.6，则使用 ThinkingMax (Fail-safe/High-risk)
// 2. 若 SurpriseIndex >= 0.3，则使用 ThinkingHigh (Moderate risk)
// 3. 否则默认 ThinkingDisabled
func SelectThinkingMode(replanCount int, maxTaint types.TaintLevel, surpriseIndex float64) types.ThinkingMode {
	if replanCount > 0 || maxTaint >= types.TaintHigh || surpriseIndex > 0.6 {
		return types.ThinkingMax
	}
	if surpriseIndex >= 0.3 {
		return types.ThinkingHigh
	}
	return types.ThinkingDisabled
}
