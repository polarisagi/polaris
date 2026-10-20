
# 模块 3: Observability & Telemetry

> OTel-native | slog | Token_Burn_Rate + Surprise_Index 一等公民 | Hardware Probe | [HE-Rule-1] [HE-Rule-4] | Go
<!-- §跳读: 0-bis:6 职责 / 0-ter:19 不变量速查 / 1:32 四层架构 / 2:62 Metrics / 3:105 TokenBurnRate(CANONICAL) / 4:126 SurpriseIndex / 5:159 HardwareProbe+AutoConfig / 6:224 OSMemoryGuard / 7:240 MonitorMemoryPressure / 8:256 LogLevel / 9:266 TraceContext / 9.1:278 SpanExporter(OpenLLMetry) / 10:288 DecisionLog / 10.1:296 PerformanceDrift / 11:322 Langfuse / 14:344 400 (SOFT)降级 / 15:360 417 依赖 -->
## 0-bis. 职责边界

| M3 **是** | M3 **不是** |
|-----------|-------------|
| 全链路追踪基础设施（OTel + Prometheus + slog） | 安全决策者（M11） |
| Token_Burn_Rate 和 Surprise_Index 基础版的指标暴露 | 质量评判者（M12） |
| HardwareProbe 启动自检 + Tier 分级 | 具体业务指标的定义者（各模块自行暴露） |
| OSMemoryGuard 三级内存压力监控与降级触发 | 降级执行者（M13 ResourceGovernor 联合执行） |
| DecisionLog 审计轨迹记录 | 可视化仪表盘（Tier 1+ Web UI） |
| Trace Context 跨模块传播（gRPC/HTTP/MCP/Go channel） | 隐私数据清洗（M11 PIIGuard） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M3_01 | Span 仅存元数据——payload 在 EventLog，通过 trace_id join 复原 | OTel SampledSpanProcessor 配置审计 |
| inv_M3_02 | TokenBurnRate CANONICAL SOURCE 在 M3——所有消费者（M4/M11/M13）从此单源读取，禁止独立采样 | CI（Continuous Integration，持续集成） `burn_rate_source_lint` |
| inv_M3_03 | SurpriseIndex 基础版（两组件）始终在线——完整版 staleness >60s 自动回退基础版 | M3 staleness 监控 |
| inv_M3_04 | KillSwitch 阶段变迁由 M11 唯一触发——M3 仅推送 TokenBurnRate 和 stage3_triggered Counter | XR-01 跨模块规则 |
| inv_M3_05 | 新增信号在 experimental 阶段仅旁路展示（Gauge + dashboard），不参与熔断/路由决策 | 新增指标审批流程 |
| inv_M3_06 | CardinalityGuard 标签基数硬上限 cap=500——禁止 session_id/task_id/trace_id 作为标签 | CI `TestCardinalityLimits` |

---

## 1. 四层架构 + gen_ai.* Span 属性

L4 可视化(Jaeger/Grafana/Langfuse) ← L3 Metrics(Prometheus+熔断) ← L2 Tracing(OTel) ← L1 slog(JSON)

### 1.1 LLM 调用 (gen_ai.chat)

请求: gen_ai.system, gen_ai.request.{model, max_tokens, temperature}, gen_ai.{provider, input_tokens, task_type, route_tier}
响应: gen_ai.response.{output_tokens, cache_hit_tokens, cost_usd, latency_ms, finish_reason}

### 1.2 工具调用 (tool.call)

属性: tool.name, tool.capability, tool.risk_level, tool.source

### 1.3 记忆操作 (memory.{read|write|consolidate|forget})

属性: memory.layer (episodic/semantic/procedural), memory.operation

### 1.4 Span 层级

Session Span → { gen_ai.chat (Perceive/Plan/Reflect) | tool.call | memory.{write,read} | state.transition }

### 1.5 动态分级采样

- 基础: `TraceIDRatioBased(0.1)`
- AlwaysSample: Surprise_Index ≥ 0.6 | Token_Burn_Rate 异常 | DecisionLog.Route 变更 | 错误响应
- 低流量补充 (LeakyBucket, Tier 0 关键): 每 60s 至少保留 1 条完整 trace；按 session 粒度，每 session 前 10 条请求和全部错误响应 AlwaysSample
- Payload > 4KB → `sha256(payload)[:16]`，完整写入本地 Decision Log (rotating 100MB capped)

---

## 2. Prometheus Metrics

本节指标名为 OTel instrument 名（点号分段，命名规则见 `00-Constitution.md` R2.6）；Prometheus 导出时自动转为下划线（如 `polaris.llm.input_tokens_total` → `polaris_llm_input_tokens_total`）。标「未实现」者代码无对应 instrument。

| 指标名 | 类型 | 标签 / 说明 |
|--------|------|--------|
| `polaris.agents.active` | Gauge | — |
| `polaris.llm.calls_total` | CounterVec | model, tier, status |
| `polaris.llm.call_latency_ms` | HistogramVec | model (ExponentialBuckets 100ms→51.2s) |
| `polaris.tokens.consumed_total` | CounterVec | type (input/output/cache_hit/cache_miss) |
| `polaris.llm.input_tokens_total` | CounterVec | purpose, provider（全部输入 token，含缓存命中；ADR-0105 决策八） |
| `polaris.llm.cache_hit_tokens_total` | CounterVec | purpose, provider（命中前缀缓存部分）。命中率由查询侧 `rate(cache_hit)/rate(input)` 算，不导出 ratio gauge（不可跨实例/窗口聚合） |
| `polaris.kv_cache_hit_ratio` | GaugeVec | model, provider（未实现） |
| `polaris.api.cost_usd_total` | CounterVec | provider, model, call_type (llm/embedding) |
| `polaris.llm.cache_hit_rate` | GaugeVec | provider, model；EMA 滑动窗口命中率（进程内自适应） |
| `polaris.embedding.tokens_total` | CounterVec | provider, model（未实现；代码现有 `polaris.embedding.call_latency_ms` / `polaris.embedding.errors_total`） |
| `polaris.embedding.batch_size` | HistogramVec | provider（未实现） |
| `polaris.token_burn_rate.total` | Gauge | 熔断信号 CANONICAL SOURCE；同族另有 `polaris.token_burn_rate.ema5s_tps` / `ema30s_tps` / `throttle_stage` |
| `polaris.token_burn.extreme_total` | Counter | Stage 3 FULLSTOP 边沿驱动 |
| `polaris.surprise.index` | Gauge | task_type；路由信号 |
| `polaris.tool.calls_total` | CounterVec | tool_category, status, sandbox_tier |
| `polaris.tool.call_latency_ms` | HistogramVec | tool_category |
| `polaris.memory.ops_total` | CounterVec | layer, operation（未实现；代码现有 `polaris.memory.*_failures_total` 失败计数） |
| `polaris.task.success_rate` | GaugeVec | task_type |
| `polaris.sandbox.executions_total` | CounterVec | tier |
| `polaris.policy.denials_total` | CounterVec | policy（未实现；代码现用 `polaris.cedar.allow_total` / `deny_total` / `degraded_total`） |
| `polaris.runtime.goroutines` / `polaris.runtime.memory_alloc_mb` | Gauge | — |
| `polaris.ffi.memory_estimate_mb` | Gauge | 含 FFI 侧内存估算（未实现；代码现有 `polaris.ffi.call_latency_ms` / `polaris.ffi.errors_total`） |
| `polaris.surprise.index_stale` | Gauge | 距上次成功计算 |
| `polaris.dbwriter.panic_total` | Counter | DatabaseWriter.Run 内 panic 被 recover 的次数 |
| `polaris.goroutine.panic_total` | Counter | `concurrent.SafeGo` recover 的 goroutine panic 总数（boot_substrate 的 panic hook 上报） |
| `polaris.system1.bypass_total` | Counter | System 1 Bypass 触发次数，标签 matched=true/false |
| `polaris.surprise_embedding_dropped` | Counter | LoadShedder 丢弃（未实现） |
| `polaris.surprise_async_failures` | Counter | 异步连续失败（未实现） |

实现 `internal/observability/metrics/`（`instruments.go` 为 OTel 仪表集合，各模块埋点经 `Record*` 函数调用）。

### 2.1 CardinalityGuard — 标签基数硬上限 (cap=500)

`internal/observability/metrics/cardinality_guard.go`：LRU(cap=500)，满时新值 → `<overflow>` 桶。禁止标签: session_id, task_id, trace_id, request_id。受控映射: tool_name→tool_category (builtin/mcp/skill/a2a)，agent_id→agent_role (planner/executor/reviewer)。CI: `go test -run TestCardinalityLimits`。

---

## 3. Token_Burn_Rate — 熔断信号 (CANONICAL SOURCE)

### 3.1 计算

每个 LLM 流 chunk 到达累加 token 数；每 1s 计算瞬时速率，维护 EMA_5s（α=0.33）与 EMA_30s（α=0.06）。双 EMA 消除 TCP Nagle/缓冲造成的到达间歇性伪加速度，区分短时峰值与持续异常。实现 `internal/observability/metrics/metrics_tokenburn.go`。

### 3.2 熔断阈值

| 条件 | 触发阶段 | 动作 |
|------|---------|------|
| EMA_5s > baseline.P95 × 2.0 | Stage 1 THROTTLE | [KillSwitch] KillThrottle |
| EMA_30s > baseline.P95 × 3.0 | Stage 2 HARD STOP | [KillSwitch] KillFullStop |
| EMA_30s > baseline.P95 × 10.0 | Stage 3 FULLSTOP | [KillSwitch] + Counter `polaris.token_burn.stage3_triggered_total`（未实现；现有信号为 `polaris.killswitch.stage` Gauge 与 `polaris.token_burn.extreme_total`） |

**baseline 冷启动 (HE-Rule-1)**:
- 前 50 个 LLM 调用 → 固定保护值 baseline.P95 = 200 tokens/s
- 50-500 个调用 → EWMA 学习 baseline，双值并存，熔断用 min(学习值, 保护值)
- >500 个调用 → 完全使用动态学习值（绝对上限 5000 tokens/s）

---

## 4. Surprise_Index — M3 可观测侧定义

M3 提供两层计算，M4 按优先级消费。

### 4.0 双层计算架构

**基础计算器（M3 内置，始终在线）**，`internal/observability/metrics/metrics_surprise.go` `ComputeBasic`：`0.7 × cosineDist + 0.3 × seqDist`
- cosineDist：当前 embedding vs EMA 历史质心（α=0.1）的余弦距离
- seqDist：工具调用序列与上一次序列的归一化 Levenshtein 编辑距离（保留顺序信息；无历史序列时为 1.0）；历史工具计数每超 100 次调用按 0.95 衰减
- 冷启动（callCount < 10）→ 固定 0.5，使 M4 走 System 1.5（0.3-0.6），避免数据极少时误触 System 1/2
- 开销 ~100-300ms（仅 embedding API 调用），无 M9 依赖；位于 L0，不依赖 internal/swarm/

**完整计算器（M9 异步推送）**：`0.4 × embeddingCosineDistance + 0.35 × toolSequenceDivergence + 0.25 × MEMFMatchScore`。公式、BoundedWorkQueue、LoadShedder 权威实现见 M9 §2.0；MEMFMatchScore 为独有扩展。

**M4 消费优先级**: `polaris.surprise.index`（完整版）→ staleness > 60s 回退 `polaris.surprise.index_basic` → 均不可用 0.5 (System 1.5)。

### 4.1 Prometheus 指标

- `polaris.surprise.index`（Gauge）：完整版，M9 异步推送
- `polaris.surprise.index_basic`（Gauge）：基础版，M3 本地计算，始终可用
- `polaris.surprise.index_stale`（Gauge）：距上次成功计算
- `polaris.surprise_embedding_dropped` / `polaris.surprise_async_failures`（Counter）：M9 上报（未实现）

### 4.2 Staleness 监控

- 完整版 staleness > 60s → 自动回退基础版 + WARN
- 基础版 staleness > 120s → WARN
- 完整版 staleness > 300s → 路由退化到 task_type 级缓存（最近 24h EMA）→ 无缓存 → 基础版 0.5

M4 读取 `polaris.surprise.index`（fallback `_basic`）做 System 1/1.5/2 路由；M3 负责指标暴露与 staleness 告警。

---

## 5. Hardware Capability Probe + AutoConfig + FeatureGate

> 代码：`internal/observability/probe/`（hardware_probe.go, feature_gate.go, tier_parameters.go, memory_probe_*.go）与 `internal/observability/`（auto_config.go, auto_config_tiers.go, auto_config_pressure.go）。

### 5.1 启动期: HardwareProbe → AutoConfig

`AutoConfig` 依赖注入（消费方接口，防包循环）：`WithSandboxController(sc)`（内存压力时降级沙箱）、`WithSurrealPurger(fn)`（L3 临界时清理 SurrealDB）；均可选，nil 跳过对应响应。

流程：`memoryProbe()` 跨平台探测总/可用内存 → `HardwareProbe.computeTier()`：≥64GB→T3，≥24GB→T2，≥16GB→T1，≥8GB→T0（<8GB 亦归 T0 并标注 degraded）→ FeatureGate 计算各特性 FeatureState（Enabled/Degraded/Disabled）→ `AutoConfig.computeConfig()` 生成：
- computeInferenceConfig(): Provider 选择与自动检测配置
- computeSandboxConfig(): L3 平台自动选择(Firecracker/VZ/WSL2) + Wasm 并发数
- computeTrainingConfig(): QLoRA 模型大小 + PRM 启用判断
- computeStorageConfig(): 引擎选择（SurrealDB-Core 全 Tier 启用）
- computeMemoryBudget(): 按 Tier 分配，可用内存不足时等比缩放
- computeTierParameters(): 按 Tier 选数值参数（§5.3）

### 5.2 运行时: OSMemoryGuard → FeatureGate.Reassess

OSMemoryGuard 每 5s 探测 free memory，三级水位触发 MemoryPressureCallback（阈值与动作详见 §6）；恢复后自动清除 Override，特性重新启用，256MB 迟滞防抖。

### 5.3 TierParameterTable（按 Tier 自动选择）

全量参数见 `internal/observability/auto_config_tiers.go`（含 MaxReplanAttempts、通道缓冲、STT/TTS 线程等）。主要参数：

| 参数 | Tier0(8GB) | Tier1(16GB) | Tier2(24GB+) | Tier3(64GB+) |
|------|-----------|------------|-------------|-------------|
| MaxConcurrentDAGNodes | 4 | 8 | 12 | 16 |
| MaxAgents | 3 | 5 | 8 | 12 |
| MemL0CacheMB | 80 | 160 | 256 | 512 |
| WasmPoolMax | 4 | 8 | 12 | 16 |
| MaxLogicCollapseConcurrent | 0(禁用) | 2 | 4 | 4 |
| SkillPreloadGold/Silver/Bronze | 5/20/25 | 10/40/100 | 15/60/150 | 20/80/200 |
| PipelineConcurrency | 2 | 4 | 6 | 8 |
| GraphRAGConcurrentWorkers | 1 | 2 | 4 | 8 |
| RegressionBudgetMin | 10 | 20 | 30 | 30 |
| PoolIntentHandler/Ingest/Background/Eval/Cron | 5/5/10/2/2 | 5/5/10/2/2 | 10/8/15/4/4 | 15/12/20/6/6 |

### 5.4 FeatureGate 特性清单（17 项）

权威来源 `internal/observability/probe/feature_gate.go`（`getFeatureRules`）。

| 特性 | 最低 Tier | 最低空闲内存 | 跨特性依赖 / 约束 |
|------|----------|------------|-----------|
| FeatureLocalInference | Tier1 | 2 GB | — |
| FeatureLocalEmbedding | Tier0 | 256 MB | — |
| FeatureQLoRA | Tier1 | 4 GB | — |
| FeaturePRMTraining | Tier2 | 8 GB | — |
| FeatureL3Sandbox | Tier0 | 512 MB | 平台检测 |
| FeatureL2Sandbox | Tier0 | 128 MB | — |
| FeatureGraphRAGFull | Tier0 | 1 GB | — |
| FeatureSurrealDBCore | Tier0 | 256 MB | — |
| FeatureLargeLocalLLM | Tier2 | 6 GB | LocalInference |
| FeatureLogicCollapse | Tier0 | 1 GB | L2Sandbox |
| FeatureComputerUseGUI | Tier0 | 512 MB | requires_display |
| FeatureVisionDisplayServer | Tier1 | 1 GB | 仅 linux（Xvfb） |
| FeaturePresidioPII | Tier1 | 512 MB | — |
| FeatureWebUI | Tier1 | 128 MB | — |
| FeatureActivationSteer | Tier1 | 1.5 GB | LocalInference |
| FeatureDeepRAG | Tier0 | 1 GB | — |
| FeatureOTelExporter | Tier1 | 64 MB | — |

调用方检查: `GlobalFeatureGate().IsEnabled(probe.FeatureQLoRA)`。

---

## 6. OSMemoryGuard — 绝对空闲内存兜底

实现 `internal/observability/probe/hardware_probe.go`（OSMemoryGuard）。与 M13 ResourceGovernor 共享三级资源降级体系。**阈值加载路径**：`internal/config/thresholds.go` `M3ObservabilityThresholds`（`MemCautionMB`=1536 / `MemWarningMB`=1024 / `MemCriticalMB`=512，`config.LoadThresholds(dataDir)` 读 `~/.polaris/config/m3_observability.toml` 覆盖）。`spec/state.yaml §thresholds.memory_pressure` 的百分比策略（`memory_governor_soft_pct` 等）与 MB 绝对阈值是两套互补系统，非同一来源。

斜率窗口：4 槽环形缓冲，采样间隔 5s，斜率阈值 -100MB/s。

| 水位 | 空闲内存 | 动作 |
|------|---------|------|
| L1 预警 | <1.5GB | QLoRA→Degraded，禁止新 Wasm 沙箱，提高上下文压缩阈值，暂停后台自进化 |
| L2 紧急 | <1.0GB | QLoRA/大模型→Disabled，限制并发 Agent < 2，禁止 LogicCollapse，挂起 Consolidation |
| L3 临界 | <512MB | Tier-3 本地模型卸载，暂停后台自进化，关闭 SurrealDB-Core cache，runtime.GC() + FreeOSMemory()，告警，全部非关键特性禁用 |

CheckAndProtect：取可用内存 (ReadMemStats + sysinfo) → 更新 slopeWindow → **斜率快速通道** dV/dt < -100MB/s 时提前按 L1 降级（即使当前空闲 > 1.5GB，5s 内降 500MB 即预判 OOM）→ 按上表逐级判定。

---

## 7. MonitorMemoryPressure + FFIMemoryController

### 7.1 RunMemoryWatcher

后台 goroutine 每 5s 调 `ProbeAvailableMemoryMB()`，经 `OSMemoryGuard.CheckAndProtect()` 得压力等级，驱动 `MemoryPressureCallback()`（`internal/observability/auto_config_pressure.go`）向 FeatureGate 推送 Override（临界禁用大模型/QLoRA；恢复清除）。256MB 迟滞防抖。

### 7.2 FFI 内存管控

Go GC 对 purego/Rust FFI 堆外内存无可见性，管控逻辑内联于 MemoryPressureCallback，非独立组件：
- GOMEMLIMIT：由配置 `go_memlimit_mb` 决定（0=不设，运行时自动管理，`internal/config/config.go`）；设计目标 `TotalRAM - 2GB(OS) - 1GB(缓冲)`
- 设计目标（当前 git grep 无调用，未实现）：GOGC 默认 50、FFI 密集阶段降到 25；L3 临界由 Callback 手动 `runtime.GC() + debug.FreeOSMemory()`

**OS 级兜底**（推荐，非强制）: 部署时用 cgroups v2 `memory.max` / `memory.high` 设硬上限，Tier 0 建议 `memory.max = 7.5GB`；应用层斜率检测未及时响应时由内核 OOM killer 按 cgroup 边界终止，不影响宿主其他进程。

---

## 8. Dynamic Log Level

设计：`POST /_admin/log-level?module=cognition&level=debug`（Unix Domain Socket / 127.0.0.1）；`atomic.Int32` 无锁存 slog.Level + `slog.Handler` 装饰器按 Level 过滤；debug 设 30min 定时器自动降回 info（防磁盘写满），已有定时器先取消。**当前未实现**（`git grep` 无 `log-level` 路由或 levelHandler）。

### 8-bis. LLM 原始流转储（`POLARIS_LLM_RAW_DUMP`，调试专用）

默认关闭。设置环境变量 `POLARIS_LLM_RAW_DUMP=<目录>` 后，OpenAI 兼容适配器每次流式请求写一个 `<目录>/<unixnano>-<随机>.sse`（目录 0700、文件 0600）：首行 `# request {...}` 为请求摘要（时间、模型、消息条数、工具名、tool_choice、thinking、reasoning_effort），其后逐行原样记录 SSE 数据行。**绝不写 API Key / Authorization**，但**会包含完整对话内容（用户输入、模型输出、工具参数与结果）**——只用于排查“空输出/usage 丢失/厂商协议差异”，用完立即关闭并删除目录。每次流式请求读取一次环境变量（无包级状态），改值后对新请求立即生效。实现：`internal/llm/adapter/stream_diag.go`。另有常驻的 Debug 日志 `llm stream: summary`（text/reasoning 字节、tool_calls 数、finish_reason、usage）。

---

## 9. Trace Context Propagation

| 通信 | 机制 |
|------|------|
| gRPC | OTel W3C Trace Context |
| HTTP (REST/SSE) | `traceparent` header |
| MCP stdio | JSON-RPC `_meta.traceparent` |
| Go channel (Blackboard) | `BlackboardEvent.TraceContext` [Blackboard] |
| Wasm | 外层 span 包裹 |

---

## 9.1 Trace Span Exporter（OpenLLMetry，ADR-0093，原 0069）

`internal/observability/trace` 提供可插拔 `SpanExporter`（`ExportSpan`/`Shutdown`），挂载于 `Tracer.EndSpan`；首批实现 `OTLPHTTPExporter`（HTTP/JSON）。约束（均已落地）：
- **默认关闭**：`config.Thresholds.M3Observability.TraceExport{Enabled, Endpoint}`（`configs/threshold-examples/m3_observability.toml`）；空 exporter 列表等价零 exporter
- **异步尽力而为**：经 `concurrent.SafeGo`，不阻塞热路径；失败仅 `slog.Warn` + `metrics.GlobalTraceExporterErrorsTotal`
- **出站安全**：复用 boot 期 SafeDialer + EgressGateway 客户端（M11 §6 + M13 §1.2.2），导出端点域名须显式加入 EgressGateway 白名单
- **接线**：`cmd/polaris/boot_substrate.go` 一次性 `trace.SetDefaultExporters`，各 `trace.NewTracer()` 实例自动继承

---

## 10. Decision Log

SQLite decision_log 表 append-only [Storage-SQLite] [HE-Rule-6] [MutationBus]；DDL `internal/protocol/schema/006_decision_log.sql`。

实现 `internal/store/audit/decisionlog.go`（`SQLiteDecisionLog`，实现 `protocol.DecisionLogger`，由 `internal/execute/orchestrator/pipeline.go` 注入）；`AppendDecision()` 经 [MutationBus] 串行写入。聚合分析 `Analyze()` 从未实现。

---

## 10.1 `[PerformanceDrift]` — 运行时任务质量漂移检测

区别于 M12 `RegressionDetector`（CI 离线）：运行时滑窗检测，与 [TokenBurnRate] / [SurpriseIndex] 并列的一等公民漂移信号。动因：M9 自演化 + Provider 模型版本更新 + 用户画像变化会让成功率悄然漂移，CI 离线发现已晚。

**度量**:
- 滑窗 [Window-Quality-10min]（与连续采样监控共享）；维度 `task_type × tier`
- `polaris.task.success_rate` Gauge（10min 内 success/total per task_type）
- `polaris.task_drift_sigma` Gauge（与 RollingBaseline 偏差的 σ 倍数；设计指标，当前代码未导出）

**RollingBaseline**: 24h EMA 基线（α=0.05）；冷启动 (<100 任务) 固定为该 task_type 的 Eval Suite 期望值（M12 §5）；每日 04:00 归档前一日 baseline 至 `decision_log` 供 M12 ShadowExecutor 对比。

**告警阈值**（当前实现：单阈值相对下降）:

| 偏差 | 等级 | 响应 |
|------|------|------|
| 相对下降 > `driftThreshold`（默认 0.15） | WARN | `OnDrift` 回调 |
| 相对下降 >= 0.8 | CRITICAL | `boot_substrate.go` 路由到 `KillSwitch.ManualFullStop`（`internal/security/killswitch.go`，ADR-0009）+ ERROR 日志，人工介入 |

**实现**: `internal/observability/metrics/performance_drift.go`（PerformanceDriftDetector）。`Record(score)` 做 EWMA 基准更新（α=0.01）+ 滑动窗口 + 相对下降判断 + 5 分钟冷却防重复告警 + `OnDrift` 回调；`internal/agent/agent_lifecycle.go` 在任务终态调用 `Record(score)`（S_COMPLETE/S_FAILED 信号）。与 ContinuousSamplingMonitor（M12 §9）共享 10min 窗口。

**与 M11 [FactualityGuard] 联动**: D6 抽样率随 drift 上调（漂移期 ×2，最高 20%）；漂移期临时关闭 [BestOfN] N>1。

**HT0**: 滑窗内存约 200KB，默认开启。

---

## 11. Langfuse — 隐私门控 + PII 红化

**当前未实现**（`internal/` 无 Langfuse 集成代码）；以下为接入时的强制前置条件:
1. Privacy mode `local_only` → 完全禁用（nil，非 error）
2. PII 红化：非 local_only → 所有 Prompt/Response 先经 M11 PIIGuard.Detect → RedactReplace [PIIGuard]，实体换会话 token（如 [NAME_1]），原始映射不出本地
3. 本地模式 `LANGFUSE_HOST=http://localhost:3000` 仅本地回路
4. 截断：Input ≤ 4096 chars，Output ≤ 2048 chars，完整内容仅本地 audit log

导出顺序: local_only→nil → PIIGuard.Redact → 截断 → Trace(SessionID) → Generation → gen.End()

---

> [HE-Rule-1] 物理实现: 从第 0 行代码起全链路可追溯——每次 token、tool call、memory 读写必须可追溯。

---

## 已知实现缺口与修复记录

历史缺口（FeatureOTelExporter 遗漏于 `reassessAll()`、`newID()` 碰撞、`StartSpan()` 不传播父 TraceID、`getAvailableMemoryMB()` 单位混用、Linux probe 未用 `MemAvailable`、指标埋点不全、PerformanceDrift 未接线）均已修复。`instruments.go` 已覆盖 LLM/缓存命中、沙箱降级（ADR-0087）、委派唤醒/恢复（ADR-0104，原 0086）、PII 映射淘汰、M10 图谱读写失败、Extension 结构化生成熔断、Outbox 游标/处理失败等埋点；memory 类复用 `InstrToolCallsTotal` 打 `category="memory"` 标签。

---

## 14. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| OTel collector 阻塞 | 丢弃非关键 span + WARN，保持 metrics + 关键日志 | collector 恢复后自动重连 |
| Prometheus metrics 文件满 | 循环覆盖旧 series（保留最近 72h） | 用户清理磁盘 |
| slog 写入磁盘失败 | 降级到 stderr (ring buffer 128KB) | 磁盘恢复后切回文件 handler |
| HardwareProbe 启动失败 | 固定 Tier0 最低配置启动 | 重启重新探测 |
| BurnRate EMA 计算线程崩溃 | 熔断器降级为原始速率（无 EMA，保守超标触发） | suture 自动重启 |

与 OSMemoryGuard 协同: RunMemoryWatcher 每 5s 推送压力等级 → Tier 自动降级/恢复，阈值与 M13 ResourceGovernor 共享。

## 默认参数

阈值与重评触发条件: `spec/state.yaml §thresholds.m3_observability`。

## 15. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | TokenBurnRate 流式 token 数据来源、gen_ai.* Span 属性注入 | M1 §5 |
| M2 Storage | DecisionLog 写入（MutationBus 串行写）、EventLog 审计轨迹 | M2 §2.1 |
| M4 Agent Kernel | SurpriseIndex consumer（System 1/1.5/2 路由） | M4 §5 |
| M9 Self-Improve | SurpriseIndex 完整版 producer（MEMF 依赖）、Gauge 异步推送 | M9 §2.0 |
| M11 Policy Safety | KillSwitch 熔断信号（M3 → M11 推送 TokenBurnRate） | M11 §4.3 |
| M13 Interface | ResourceGovernor 三级降级共享阈值 | M13 §2.0 |
| 接口定义 | TokenBurnRate/SurpriseIndex 指标 | `internal/observability/` |
| 全局字典 | HE-Rule-1、TokenBurnRate/SurpriseIndex 定义、Window-* 常量 | 00-Global-Dictionary §2, §3, §10 |
| 时序图 | KillSwitch 触发链（M3 BurnRateDetector 的角色） | DIAGRAMS.md#killswitch |
