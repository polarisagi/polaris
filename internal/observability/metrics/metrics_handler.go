package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/types"
)

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

		// [阶段02-错误吞没整改] 全部 sync instrument 注册失败说明 meter provider
		// 根本没起来，此时不再继续注册 gauge/callback，直接降级为 legacy handler
		// （otelHandlerPtr 保持 nil，MetricsHandler 下次调用回退）。
		if err := InitMetrics(meter); err != nil {
			slog.Error("observability: OTel metrics initialization failed entirely, falling back to legacy handler", "err", err)
			return
		}

		ema5sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema5s_tps")
		ema30sGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.ema30s_tps")
		totalCounter, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.total")
		throttleGauge, _ := meter.Float64ObservableGauge("polaris.token_burn_rate.throttle_stage")
		surpriseGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index")
		surpriseBasicGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index_basic")
		surpriseStaleGauge, _ := meter.Float64ObservableGauge("polaris.surprise_index.stale")
		surrealSizeGauge, _ := meter.Float64ObservableGauge("polaris.surrealdb.index_size_mb")
		killswitchGauge, _ := meter.Float64ObservableGauge("polaris.killswitch.stage")

		// V8-S3: 创始锚点漂移评分
		// 注意：若 policy 包导入形成循环，使用函数变量注入（见 §3.3 循环依赖处理）
		anchorDriftGauge, _ := meter.Float64ObservableGauge(
			"polaris.founding_anchor.drift_score",
			metric.WithDescription("与创始行为锚点的综合漂移评分 [0,1]"),
		)

		// PerformanceDrift（M03 §10.1，2026-07-21 deadcode 审查补齐 gauge 暴露，见 legacyMetricsHandler 同名注释）
		perfDriftPassRateGauge, _ := meter.Float64ObservableGauge(
			"polaris.performance_drift.pass_rate",
			metric.WithDescription("Current windowed task success pass rate"),
		)
		perfDriftBaselineGauge, _ := meter.Float64ObservableGauge(
			"polaris.performance_drift.baseline",
			metric.WithDescription("Performance drift detector baseline pass rate"),
		)

		// 无 label Global*Total 累计计数器：统一由 simpleCounters() 表驱动注册（GR-1.2-004）。
		counters := simpleCounters()
		counterGauges := make([]metric.Float64ObservableGauge, len(counters))
		for i, c := range counters {
			counterGauges[i], _ = meter.Float64ObservableGauge(c.name, metric.WithDescription(c.help))
		}

		// [2026-08-02 HE-1 补齐] 失败不再静默丢弃，改为记录日志+回退 legacy handler
		// （与上方 InitMetrics 全部失败时的既有降级路径语义一致），否则本组 gauge
		// 会在 /metrics 上无声消失且无日志线索。
		observables := []metric.Observable{ema5sGauge, ema30sGauge, totalCounter, throttleGauge, surpriseGauge, surpriseBasicGauge,
			surpriseStaleGauge, surrealSizeGauge, killswitchGauge, anchorDriftGauge, perfDriftPassRateGauge, perfDriftBaselineGauge}
		for _, g := range counterGauges {
			observables = append(observables, g)
		}
		_, cbErr := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
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
			o.ObserveFloat64(anchorDriftGauge, GetFoundingAnchorDriftScore())

			pd := GlobalPerformanceDrift()
			o.ObserveFloat64(perfDriftPassRateGauge, pd.CurrentPassRate())
			o.ObserveFloat64(perfDriftBaselineGauge, pd.Baseline())

			for i, c := range counters {
				o.ObserveFloat64(counterGauges[i], float64(c.v.Load()))
			}

			return nil
		}, observables...)
		if cbErr != nil {
			slog.Error("observability: failed to register OTel observable gauge callback, falling back to legacy handler", "err", cbErr)
			return
		}

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

		// ── KillSwitch Stage ──────────────────────────────────────────────────
		stage := GlobalKillswitchStage.Load()
		fmt.Fprintf(w, "# HELP polaris_killswitch_stage Current M13 KillSwitch stage\n")
		fmt.Fprintf(w, "# TYPE polaris_killswitch_stage gauge\n")
		fmt.Fprintf(w, "polaris_killswitch_stage %d\n", stage)

		// ── PerformanceDrift（M03 §10.1，2026-07-21 deadcode 审查补齐 gauge 暴露）──
		// 检测器本体（Record/RegisterListener→KillSwitch）此前已生产接入
		// （agent_lifecycle.go Record、boot_substrate.go RegisterListener），
		// 但 CurrentPassRate/Baseline 两个只读访问器从未暴露到 /metrics，
		// 与 SurpriseIndex/TokenBurnRate 等一等公民信号的可观测标准不一致。
		pd := GlobalPerformanceDrift()
		fmt.Fprintf(w, "# HELP polaris_performance_drift_pass_rate Current windowed task success pass rate\n")
		fmt.Fprintf(w, "# TYPE polaris_performance_drift_pass_rate gauge\n")
		fmt.Fprintf(w, "polaris_performance_drift_pass_rate %g\n", pd.CurrentPassRate())

		fmt.Fprintf(w, "# HELP polaris_performance_drift_baseline Performance drift detector baseline pass rate\n")
		fmt.Fprintf(w, "# TYPE polaris_performance_drift_baseline gauge\n")
		fmt.Fprintf(w, "polaris_performance_drift_baseline %g\n", pd.Baseline())

		// ── 无 label Global*Total 累计计数器（表驱动，见 metrics_counters.go）──────
		for _, c := range simpleCounters() {
			fmt.Fprintf(w, "# HELP %s %s\n", c.promName(), c.help)
			fmt.Fprintf(w, "# TYPE %s counter\n", c.promName())
			fmt.Fprintf(w, "%s %d\n", c.promName(), c.v.Load())
		}
	})
}

// SelectThinkingMode 决定本次规划的思考档位（ADR-0101 决策二修订 ADR-0020 决策二）。
// 规则：
// 1. 重规划（replanCount > 0）或 SurpriseIndex ≥ high → ThinkingMax（便宜路径已失败/高度陌生，升级）
// 2. 任务复杂度 ≥ plan.reasoning_complexity 或 SurpriseIndex ≥ low → ThinkingHigh
// 3. 否则 ThinkingDisabled
//
// 不再以污点等级为输入：用户输入恒为 TaintHigh，旧规则令每个首轮规划都走 ThinkingMax，
// 使"思考深度"退化为常量。污点是数据来源的安全标签，由 Taint/Cedar/PolicyGate 防线
// 消费；思考深度是成本/质量旋钮，二者正交。
func SelectThinkingMode(replanCount int, complexity, surpriseIndex float64) types.ThinkingMode {
	low, high := 0.30, 0.60
	complexGate := DefaultPlanReasoningComplexity
	if cfg := config.Get(); cfg != nil {
		t := cfg.Thresholds.M9SelfImprove
		if t.SurpriseRouteLowThreshold > 0 {
			low = t.SurpriseRouteLowThreshold
		}
		if t.SurpriseRouteHighThreshold > 0 {
			high = t.SurpriseRouteHighThreshold
		}
		if g := cfg.Thresholds.M4Kernel.PlanReasoningComplexity; g > 0 {
			complexGate = g
		}
	}
	if replanCount > 0 || surpriseIndex >= high {
		return types.ThinkingMax
	}
	if complexity >= complexGate || surpriseIndex >= low {
		return types.ThinkingHigh
	}
	return types.ThinkingDisabled
}

// DefaultPlanReasoningComplexity plan.reasoning_complexity 未配置时的兜底（SSoT：spec/state.yaml）。
const DefaultPlanReasoningComplexity = 0.7

// SelectPlanModelPool 规划阶段的模型池级联（ADR-0101 决策二）：便宜池先行，
// 失败（重规划）/高复杂度/高 Surprise 才升级到 reasoning 池。
// 与 SelectThinkingMode 同源判定，避免"便宜模型 + 满档思考"或"贵模型 + 不思考"的错配。
func SelectPlanModelPool(replanCount int, complexity, surpriseIndex float64) types.ModelPool {
	mode := SelectThinkingMode(replanCount, complexity, surpriseIndex)
	complexGate := DefaultPlanReasoningComplexity
	if cfg := config.Get(); cfg != nil {
		if g := cfg.Thresholds.M4Kernel.PlanReasoningComplexity; g > 0 {
			complexGate = g
		}
	}
	if mode == types.ThinkingMax || complexity >= complexGate {
		return types.ModelPoolReasoning
	}
	return types.ModelPoolGeneral
}
