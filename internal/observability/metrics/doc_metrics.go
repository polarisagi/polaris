package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func initDocInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error
	InstrExtensionDocRequestsTotal, err = meter.Int64Counter(
		"polaris.extension.doc.requests_total",
		metric.WithDescription("说明文档请求总数 (label: level, status, locale)"),
	)
	ie.capture("polaris.extension.doc.requests_total", err)

	InstrExtensionDocTranslationsTotal, err = meter.Int64Counter(
		"polaris.extension.doc.translations_total",
		metric.WithDescription("扩展翻译任务状态计数 (label: status, error_code)"),
	)
	ie.capture("polaris.extension.doc.translations_total", err)

	InstrExtensionDocTranslationDurationSeconds, err = meter.Float64Histogram(
		"polaris.extension.doc.translation_duration_seconds",
		metric.WithDescription("扩展翻译耗时直方图（秒）"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 30, 60, 120),
	)
	ie.capture("polaris.extension.doc.translation_duration_seconds", err)
}

// RecordExtensionDocRequest 记录文档读取请求。有界基数：level (origin/index_upstream/index_machine/cache/missing), status (ready/missing/stale/queued), locale (zh-CN/en)
func RecordExtensionDocRequest(ctx context.Context, level, status, locale string) {
	if InstrExtensionDocRequestsTotal != nil {
		InstrExtensionDocRequestsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("level", level),
			attribute.String("status", status),
			attribute.String("locale", locale),
		))
	}
}

// RecordExtensionDocTranslation 记录翻译任务状态及错误码（有界枚举）。
func RecordExtensionDocTranslation(ctx context.Context, status, errorCode string) {
	if InstrExtensionDocTranslationsTotal != nil {
		InstrExtensionDocTranslationsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", status),
			attribute.String("error_code", errorCode),
		))
	}
}

// RecordExtensionDocTranslationDuration 记录翻译耗时。
func RecordExtensionDocTranslationDuration(ctx context.Context, sec float64) {
	if InstrExtensionDocTranslationDurationSeconds != nil {
		InstrExtensionDocTranslationDurationSeconds.Record(ctx, sec)
	}
}
