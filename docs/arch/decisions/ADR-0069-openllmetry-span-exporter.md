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
