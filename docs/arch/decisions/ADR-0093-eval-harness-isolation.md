# ADR-0093: Eval 评测隔离、基准适配器与轨迹导出

> 状态：Accepted ｜ 首次：2026-07-28 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0068/0069 ｜ 模块：M12 `internal/eval/`、`internal/observability/trace/`

## 决策一：评测隔离与 Benchmark Runner 扩展（原 ADR-0093 决策一，GD-14-007）

- 隔离原则：M9 自进化模块禁止直接修改评测策略；评测数据集完整性不可被被评对象触及，防止模型在验证集作弊。
- 外部基准在 `internal/eval/harness` 之上扩展：`protocol.EvalRunner.RunBenchmarkDataset` 复用 `RunSuite` 的沙盒执行与评分（统一映射为 `EvalCase`），不干涉 training/validation 存储链路。
- 公开数据集经 `internal/eval/benchmark` 拉取 JSONL，HTTP 客户端必须是 SafeDialer 派生（XR-06），nil 时 fail-closed 返回 `CodeInvalidInput`。
- 入口：`POST /v1/eval/benchmark`（`EvalAdmin`），查询参数 `dataset` 选预设，异步触发并立即返回 `StatusAccepted`，调用方轮询状态。
- 禁止绕过 Sandbox 直接执行 Benchmark 代码（RCE 面）；基准数据视为只读，不得作为 `events` 回流主库。

## 决策二：开放基准适配器（原 ADR-0068）

- `protocol.BenchmarkAdapter`（`Name()` / `Load(ctx, datasetPath)`）把外部基准映射为 `harness.EvalCase`，复用 `RunnerImpl`，不另造执行器；`benchmark.GetAdapter(name)` 按名取适配器。
- 数据集路径由用户运行时指定，不打包进仓库（隐私/许可），仓库仅留 2~3 条 fixture。
- 现状（代码核实）：已实现 tau-bench / locomo / longmemeval / swe-bench / swe-bench-jsonl / gaia；`terminal`（Terminal-Bench）仍返回 `CodeUnimplemented`，外部格式未定前不臆测实现。
- CLI `polaris eval bench --suite --data [--execute]`：无 `--execute` 仅验证加载/转换（报告 `executed:false`）；带 `--execute` 由 `main.go` 拦截走完整 boot 序列取合法 `RunnerImpl`，在隔离环境落盘执行，不绕过安全/评测门控。

## 决策三：OpenLLMetry 轨迹导出器（原 ADR-0069）

- `internal/observability/trace` 提供可插拔 `SpanExporter`；`EndSpan` 内经 `concurrent.SafeGo` 异步最佳努力导出，失败仅 `slog.Warn` + `trace_exporter_errors_total`，绝不阻塞 Agent 热路径（裸 `go func` 已订正为 SafeGo：导出器 panic 会拖垮进程）。
- 导出器 HTTP 客户端走 SafeDialer（XR-06）；目标域名须在 `EgressAllowedDomains` 内，否则被 egressGW 拒绝（刻意纵深防御）。
- 默认关闭；`exporters` 为空 slice 即 Noop，不定义 NoopExporter 类型（R6）。首批 `OTLPHTTPExporter`（HTTP/JSON）。
- `trace.SetDefaultExporters`（`atomic.Pointer`，boot 期单次写入、运行期只读）由 `cmd/polaris/boot_substrate.go` 按 `TraceExport{Enabled,Endpoint}` 注册，`NewTracer()` 自动附加。

## 反例守护

- 直接绕过 Sandbox 跑 Benchmark 代码 — RCE — 2026-07-28
- 修改基准数据集元数据或回流 `events` — 污染验证集 — 2026-07-28
- 为接口完整性臆测 Terminal-Bench 格式 — 外部格式未定 — 2026-07-23
- 导出路径引入同步阻塞兜底 — 阻塞 Agent 热路径 — 2026-07-23

## 重新评估触发条件

- 出现真实需要用基准结果反哺训练的场景：须先经安全评估确认不引入验证集作弊，才重议"数据集只读不回流"；图方便的放宽一律拒绝。
- Terminal-Bench 推进实现前须有明确外部数据格式定义。
- 新增导出器（Jaeger/Zipkin 原生协议）前须确认 `OTLPHTTPExporter` 无法覆盖；trace 丢失率过高才重议兜底，且不得阻塞热路径。

## 沿革

| 日期 | 变更 | 原编号/理由 |
|---|---|---|
| 2026-07-23 | 决策二、决策三 | 原 ADR-0068、ADR-0069 |
| 2026-07-28 | 决策一 | 原编号 0051 |
| 2026-08-09 | 由 0051 改号为 0093 | 与已删除的 ADR-0051 撞号（编号不复用） |
| 2026-10-12 | 并入 0068/0069；适配器清单按代码订正 | 文档合并 |

## 引用代码

`internal/eval/benchmark/benchmark.go`、`internal/eval/harness/runner.go`、`internal/eval/harness/benchmark/benchmark.go`、`internal/eval/harness/benchmark/terminalbench.go`、`internal/gateway/server/sysadmin/evaladmin/admin.go`、`cmd/polaris/cli_eval_bench.go`、`internal/observability/trace/tracer.go`、`internal/observability/trace/otlp_exporter.go`、`cmd/polaris/boot_substrate.go`
