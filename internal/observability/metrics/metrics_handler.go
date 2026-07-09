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
