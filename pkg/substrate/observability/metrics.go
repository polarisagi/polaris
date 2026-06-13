package observability

import (
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
	"github.com/polarisagi/polaris/internal/protocol"
)

var (
	GlobalSurpriseIndex = NewSurpriseIndex()

	// GlobalKillswitchStage 全局 KillSwitch 阶段原子量（0=Normal…3=FullStop）。
	// killswitch.go 的 StateChangeCallback 写入；M13 handleStatus 读取。
	GlobalKillswitchStage atomic.Int32
)

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

func (tbr *TokenBurnRate) CheckThrottle() ThrottleStage {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()

	// 学习型基线 (MVP: 暂时使用静态保守上限)
	limit := math.Max(tbr.baselineP95, 200.0)

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
	if fg := GlobalFeatureGate(); fg != nil && fg.IsEnabled(FeatureOTelExporter) {
		return otelMetricsHandler(tbr)
	}
	return legacyMetricsHandler(tbr)
}

var (
	otelOnce    sync.Once
	otelHandler http.Handler
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
		meter := provider.Meter("github.com/polarisagi/polaris/pkg/substrate/observability")

		InitMetrics(meter)

		ema5sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema5s_tps")
		ema30sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema30s_tps")
		totalCounter, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.total")
		throttleGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.throttle_stage")
		surpriseGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index")
		surpriseStaleGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index.stale")
		surrealSizeGauge, _ := meter.Float64ObservableGauge("polaris.surrealdb.index_size_mb")

		_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveFloat64(ema5sGauge, tbr.EMA5s())
			o.ObserveFloat64(ema30sGauge, tbr.EMA30s())
			o.ObserveFloat64(totalCounter, float64(tbr.cumulativeTokens.Load()))
			o.ObserveFloat64(throttleGauge, float64(tbr.CheckThrottle()))

			si := GlobalSurpriseIndex
			o.ObserveFloat64(surpriseGauge, si.Current())
			staleVal := 0.0
			if si.IsStale() {
				staleVal = 1.0
			}
			o.ObserveFloat64(surpriseStaleGauge, staleVal)

			ls := PolarisSurrealDBIndexSizeMB.Load()
			o.ObserveFloat64(surrealSizeGauge, float64(ls))
			return nil
		}, ema5sGauge, ema30sGauge, totalCounter, throttleGauge, surpriseGauge, surpriseStaleGauge, surrealSizeGauge)

		otelHandler = promhttp.Handler()
	})
	if otelHandler != nil {
		return otelHandler
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
		si := GlobalSurpriseIndex
		siVal := si.Current()
		siStale := 0
		if si.IsStale() {
			siStale = 1
		}

		fmt.Fprintf(w, "# HELP polaris_surprise_index Current surprise index (0.0~1.0)\n")
		fmt.Fprintf(w, "# TYPE polaris_surprise_index gauge\n")
		fmt.Fprintf(w, "polaris_surprise_index %g\n", siVal)

		fmt.Fprintf(w, "# HELP polaris_surprise_index_stale Whether surprise index is stale (1=stale >120s)\n")
		fmt.Fprintf(w, "# TYPE polaris_surprise_index_stale gauge\n")
		fmt.Fprintf(w, "polaris_surprise_index_stale %d\n", siStale)

		// ── SurrealDB 索引大小 ──────────────────────────────────────────────────
		ls := PolarisSurrealDBIndexSizeMB.Load()
		fmt.Fprintf(w, "# HELP polaris_surrealdb_index_size_mb SurrealDB-Core index memory usage\n")
		fmt.Fprintf(w, "# TYPE polaris_surrealdb_index_size_mb gauge\n")
		fmt.Fprintf(w, "polaris_surrealdb_index_size_mb %d\n", ls)
	})
}

// SelectThinkingMode 根据当前系统的运行时状态，决定应该使用哪个档位的 ThinkingMode。
// 规则：
// 1. 若重规划次数 > 0，或任务最大污点等级 >= 3（TaintHigh），或 SurpriseIndex > 0.6，则使用 ThinkingMax (Fail-safe/High-risk)
// 2. 若 SurpriseIndex >= 0.3，则使用 ThinkingHigh (Moderate risk)
// 3. 否则默认 ThinkingDisabled
func SelectThinkingMode(replanCount int, maxTaint protocol.TaintLevel, surpriseIndex float64) protocol.ThinkingMode {
	if replanCount > 0 || maxTaint >= protocol.TaintHigh || surpriseIndex > 0.6 {
		return protocol.ThinkingMax
	}
	if surpriseIndex >= 0.3 {
		return protocol.ThinkingHigh
	}
	return protocol.ThinkingDisabled
}
