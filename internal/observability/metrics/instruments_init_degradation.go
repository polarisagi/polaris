package metrics

import "go.opentelemetry.io/otel/metric"

// instruments_init_degradation.go — "静默降级"类计数器的注册（从 instruments_init.go
// 拆出，R7 行数上限）。
//
// 归为一组的理由不是凑行数：这三条埋点服务同一个判据——**系统正在降级但没人知道**。
// 检索单路超时、RAG 因污点被过滤、下载跨源续传重下，三者共同点是"功能上没报错、
// 结果却悄悄变差"。HE-1 要求能算就要上报，正是冲这类路径。
func initDegradationInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error

	// [WP-11 / GD-13-003] 混合检索单路超时降级计数：没有它，"向量路长期静默降级"
	// 这件事在生产上不可发现（三条降级分支此前只有 slog.Warn）。
	InstrRetrievalRouteTimeouts, err = meter.Int64Counter(
		"polaris.retrieval.route_timeouts_total",
		metric.WithDescription("混合检索单路超时降级次数 (label: route: vector/graph/extra)"),
	)
	ie.capture("polaris.retrieval.route_timeouts_total", err)

	// [WP-1.2 / GR-7-001] KnowledgeBase.Search 因超出 TaintMax 过滤掉的 chunk 数。
	InstrRAGTaintDrops, err = meter.Int64Counter(
		"polaris.knowledge.taint_drops_total",
		metric.WithDescription("RAG 检索结果中因 TaintLevel 超过 TaintMax 被过滤的 chunk 数"),
	)
	ie.capture("polaris.knowledge.taint_drops_total", err)

	// [WP-10.2 / GR-1-004] 跨源续传内容同一性校验失败导致的重下次数。
	InstrDownloaderResumeRestarts, err = meter.Int64Counter(
		"polaris.downloader.resume_restarts_total",
		metric.WithDescription("断点续传因 ETag/Last-Modified/Content-Length 与 .part 元数据不一致而清零重下的次数"),
	)
	ie.capture("polaris.downloader.resume_restarts_total", err)
}
