# ADR-0069: OpenLLMetry 轨迹导出器架构

- **状态**: Accepted（已执行，含 boot 期接线）| **模块**: `internal/observability/trace`

## 决策

`internal/observability/trace` 引入可插拔 `SpanExporter` 接口。异步最佳努力（`EndSpan` 内 `concurrent.SafeGo` 启动，失败仅 `slog.Warn` + `trace_exporter_errors_total` 计数，绝不阻塞 Agent 热路径——原用裸 `go func` 逃逸豁免，已订正为 SafeGo 因导出器 panic 会拖垮进程）。导出器 HTTP 客户端必须走 M11 SafeDialer（XR-06）。默认关闭，`exporters` 为空 slice 时等价 Noop（不单独定义 NoopExporter 类型，R6 最少代码集）。

首批实现通用 `OTLPHTTPExporter`（HTTP/JSON）。`trace.SetDefaultExporters`（`atomic.Pointer` 零值单例，boot 期单次写入运行期只读）供 `boot_substrate.go` 按 `M3ObservabilityThresholds.TraceExport{Enabled,Endpoint}` 配置注册；`NewTracer()` 构造时自动附加。导出目标域名须在 `EgressAllowedDomains` 白名单内，否则被 egressGW 拒绝（刻意纵深防御）。

## 引用代码

`internal/observability/trace/tracer.go`、`cmd/polaris/boot_substrate.go`
