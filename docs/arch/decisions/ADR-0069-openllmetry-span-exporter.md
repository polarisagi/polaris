# ADR-0069: OpenLLMetry 轨迹导出器架构

## 状态
Accepted

## 背景 (Context)
随着系统能力的增强，用户（尤其是重度自托管和安全审计要求高的场景）需要将我们内部 `gen_ai.*` 语义的执行轨迹（Trajectory）和 Span 数据导出到外部的大模型可观测平台（如 LangSmith, Braintrust, Phoenix 等）。这符合架构规范 GD-14-002 对可观测性的开放要求。

目前的 `internal/observability/trace` 已经有一套基于 JSON 序列化的 Span 结构。我们需要在 `Tracer.EndSpan` 生命周期挂载导出器。

## 决策 (Decision)
1. **可插拔导出接口**：在 `internal/observability/trace` 包下引入 `SpanExporter` 接口，定义 `ExportSpan` 和 `Shutdown` 方法。
2. **异步最佳努力 (Best-effort)**：导出逻辑必须是**非阻塞的**。在 `EndSpan` 内采用 `concurrent.SafeGo` 启动异步任务发送数据，如果发送超时或失败，仅通过 `slog.Warn` 记录，并增加 `trace_exporter_errors_total` 指标计数，绝不允许阻塞 Agent 热路径。
3. **出站安全合规**：导出器的 HTTP 客户端**必须走 M11 SafeDialer 机制**（XR-06 规范），禁止使用裸 `http.Client` 或者直接拨号，防止 SSRF。
4. **默认行为**：导出器默认关闭（`NoopExporter`），仅在配置显式指定时启用，确保对默认场景的无端侧负担。
5. **协议选择**：首批实现一个通用的基于 HTTP/JSON 的 `OTLPHTTPExporter`。因为目前的 `Span` 结构体已经包含了必要的语义字段，我们通过直接推送 JSON 数据实现。

## 结果 (Consequences)
**正面影响**：
- 为自托管用户提供了无缝对接标准大模型监控平台的能力。
- 保障了运行时的主链路安全，不会因为监控节点宕机导致应用不可用。

**负面影响**：
- 异步导出如果在高并发极端场景下，可能导致 Goroutine 的突发增加。不过由于 M11 层有并发限制以及 SafeDialer 的兜底，整体风险可控。

**2026-07-23 复核订正**：
1. `EndSpan` 最初用裸 `go func` + `//custom-nolint:bare-goroutine` 逃逸豁免实现异步导出，理由是"trace export 不需要 SafeGo 管理"——该理由不成立：SafeGo 的价值是 panic 恢复，一个默认关闭的可选导出器一旦内部 panic（如导出器实现 bug）会直接拖垮整个进程，与"绝不阻塞/绝不影响主链路"的决策原文相悖。已改为 `concurrent.SafeGo`。
2. 决策点 2 要求的 `trace_exporter_errors_total` 指标此前只有一行注释"这里本可以上报指标"，从未真正实现。已补齐为 `metrics.GlobalTraceExporterErrorsTotal`。
3. **本 ADR 标注为 Accepted，但截至 2026-07-23 复核当时，`OTLPHTTPExporter`/`SpanExporter` 除单元测试外未被任何启动路径（boot_*.go）或配置项调用/注册**——即当前自托管用户实际上*无法*启用该导出能力（决策点 4 "仅在配置显式指定时启用"所依赖的配置项与 `NoopExporter` 默认值均不存在）。`trace.NewTracer()` 目前仅在 `internal/knowledge/rag_impl.go`、`internal/knowledge/retriever.go` 两处按调用临时构造，互相独立、均未注册任何 exporter。补齐配置项 + boot 期 SafeDialer 化 `http.Client` 注入属于独立工作量（涉及 `internal/config` 结构体变更即触发 `make gen-threshold-examples` 的强制流程与统一 Tracer 单例的架构决策），本轮不展开，留待后续 ADR/任务跟进，此处仅订正状态描述避免误导。

**2026-07-23 补充接线（同日）**：
1. **配置面**：复核时发现 `internal/config/thresholds.go` 的 `M3ObservabilityThresholds.TraceExport{Enabled, Endpoint}` 及对应的 `configs/threshold-examples/m3_observability.toml`（`[trace_export] enabled=false / endpoint=''`）字段已存在（本轮另一提交已加，此前未被任何代码读取），无需新增字段，直接复用。
2. **NoopExporter 的等价实现**：未单独定义 `NoopExporter` 类型——`Tracer.exporters` 为空 slice 时 `EndSpan` 循环体不执行，语义上与显式 Noop 完全等价，避免冗余抽象（R6/HE-4"最少代码集"）。
3. **单 Tracer 实例 vs 全局默认导出器**：`rag_impl.go`/`retriever.go` 两处各自独立 `NewTracer()`，若要求二者都能导出，唯一低侵入方案是让 `NewTracer()` 自动附加 boot 期注册的默认导出器列表，而非把这两个调用点改造成注入共享单例（后者是超出"接入导出器"范围的架构改动）。新增 `trace.SetDefaultExporters([]SpanExporter)`（`atomic.Pointer[[]SpanExporter]` 零值单例，boot 期单次写入、运行期只读，符合 `Test_inv_NoGlobalVar` 豁免类别 + `golangci-lint gochecknoglobals` 显式 nolint），`NewTracer()` 构造时自动读取并附加。
4. **boot 接线**：`cmd/polaris/boot_substrate.go` 在构造 `safeHTTPClient`（已套 `egressGW` 域名白名单 + `SafeDialer` SSRF 阻断，满足决策点 3）之后，读取 `cfg.Thresholds.M3Observability.TraceExport`：`Enabled=true` 且 `Endpoint` 非空时调用 `trace.SetDefaultExporters([]trace.SpanExporter{trace.NewOTLPHTTPExporter(safeHTTPClient, endpoint)})`；`Enabled=true` 但 `Endpoint` 为空时 `slog.Warn` 提示配置不完整并跳过注册（fail-closed，不静默启用空 endpoint）。若用户配置的导出平台域名不在 `cfg.System.EgressAllowedDomains`/`egress.DefaultAllowedDomains()` 内，请求会被 `egressGW` 拒绝——这是刻意的纵深防御，需要用户显式将该域名加入白名单。
5. **验证**：新增 `internal/observability/trace/tracer_test.go`（`TestSetDefaultExporters_NewTracerInherits`/`TestSetDefaultExporters_NilIsNoop`），`go build ./...`、`go test ./...`、`go test -race ./internal/observability/trace/... ./cmd/polaris/...`、`make lint`、`make deadcode` 均通过；`scripts/deadcode-allowlist.txt` 中 `NewOTLPHTTPExporter`/`ExportSpan`/`Shutdown` 三项因现已被 `boot_substrate.go` 实际调用而移出白名单，仅保留 `Tracer.RegisterExporter`（单 Tracer 实例手动注册入口，boot 走的是 `SetDefaultExporters`，故仍生产不可达）。
6. 至此本 ADR 决策点 1-5 全部落地，状态描述与代码现状一致。
