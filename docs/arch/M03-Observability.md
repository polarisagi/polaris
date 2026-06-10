# 模块 3: Observability & Telemetry

> OTel-native | slog | Token_Burn_Rate + Surprise_Index 一等公民 | Hardware Probe | [HE-Rule-1] [HE-Rule-4] | Go
> **§跳读**: 0-bis:5 职责 / 0-ter:18 不变量速查 / 1:31 四层架构 / 2:68 Metrics / 3:103 TokenBurnRate(CANONICAL) / 4:137 SurpriseIndex / 5:179 HardwareProbe+AutoConfig / 6:252 OSMemoryGuard / 7:268 MonitorMemoryPressure / 8:299 LogLevel / 9:307 TraceContext / 10:319 DecisionLog / 10.1:316 PerformanceDrift / 11:366 Langfuse / 14:382 (SOFT)降级 / 15:399 依赖
## 0-bis. 职责边界

| M3 **是** | M3 **不是** |
|-----------|-------------|
| 全链路追踪基础设施（OTel + Prometheus + slog） | 安全决策者（M11） |
| Token_Burn_Rate 和 Surprise_Index 基础版的指标暴露 | 质量评判者（M12） |
| HardwareProbe 启动自检 + Tier 分级 | 具体业务指标的定义者（各模块自行暴露 Prometheus 指标） |
| OSMemoryGuard 三级内存压力监控与降级触发 | 降级执行者（M13 ResourceGovernor 联合执行） |
| DecisionLog 审计轨迹记录 | 可视化仪表盘（Tier 1+ Web UI） |
| Trace Context 跨模块传播（gRPC/HTTP/MCP/Go channel） | 隐私数据清洗（M11 PIIGuard） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M3_01 | Span 仅存元数据——payload 在 EventLog，通过 trace_id join 复原 | OTel SampledSpanProcessor 配置审计 |
| inv_M3_02 | TokenBurnRate CANONICAL SOURCE 在 M3——所有消费者（M4/M11/M13）从此单源读取，禁止独立采样 | CI `burn_rate_source_lint` |
| inv_M3_03 | SurpriseIndex 基础版（两组件）始终在线——完整版 staleness >60s 自动回退基础版 | M3 staleness 监控 |
| inv_M3_04 | KillSwitch 阶段变迁由 M11 唯一触发——M3 仅推送 TokenBurnRate 和 stage3_triggered Counter | XR-01 跨模块规则 |
| inv_M3_05 | 新增信号在 experimental 阶段仅旁路展示（Gauge + dashboard），不参与熔断/路由决策 | 新增指标审批流程 |
| inv_M3_06 | CardinalityGuard 标签基数硬上限 cap=500——禁止 session_id/task_id/trace_id 作为标签 | CI `TestCardinalityLimits` |

---

## 1. 四层架构 + gen_ai.* Span 属性

```
L4 可视化(Jaeger/Grafana/Langfuse) ← L3 Metrics(Prometheus+熔断) ← L2 Tracing(OTel) ← L1 slog(JSON)
```

### 1.1 LLM 调用 (gen_ai.chat)

请求: gen_ai.system, gen_ai.request.{model, max_tokens, temperature}, gen_ai.{provider, input_tokens, task_type, route_tier}
响应: gen_ai.response.{output_tokens, cache_hit_tokens, cost_usd, latency_ms, finish_reason}

### 1.2 工具调用 (tool.call)

属性: tool.name, tool.capability, tool.risk_level, tool.source

### 1.3 记忆操作 (memory.{read|write|consolidate|forget})

属性: memory.layer (episodic/semantic/procedural), memory.operation

### 1.4 Span 层级

```
Session Span
  ├── gen_ai.chat (Perceive/Plan/Reflect)
  ├── tool.call | memory.{write,read}
  └── state.transition
```

### 1.5 动态分级采样

- 基础: `TraceIDRatioBased(0.1)`
- AlwaysSample: Surprise_Index ≥ 0.6 | Token_Burn_Rate 异常 | DecisionLog.Route 变更 | 错误响应
- 低流量补充 (LeakyBucket, Tier 0 关键): 每 60s 至少保留 1 条完整 trace（独立于比例采样）。按 session 粒度: 每个 session 前 10 条请求和全部错误响应 AlwaysSample
- Payload: > 4KB → `sha256(payload)[:16]`, 完整写入本地 Decision Log (rotating 100MB capped)

---

## 2. Prometheus Metrics

| 指标名 | 类型 | 标签 |
|--------|------|------|
| `polaris_agents_active` | Gauge | — |
| `polaris_llm_calls_total` | CounterVec | model, tier, status |
| `polaris_llm_call_latency_ms` | HistogramVec | model (ExponentialBuckets 100ms→51.2s) |
| `polaris_tokens_consumed_total` | CounterVec | type (input/output/cache_hit/cache_miss) |
| `polaris_kv_cache_hit_ratio` | GaugeVec | model, provider |
| `polaris_api_cost_usd_total` | CounterVec | provider, model, call_type (llm/embedding) |
| `polaris_llm_cache_hit_rate` | GaugeVec | provider, model | EMA 滑动窗口缓存命中率（进程内指导自适应） |
| `polaris_embedding_tokens_total` | CounterVec | provider, model | Embedding 专用 token 计数 |
| `polaris_embedding_batch_size` | HistogramVec | provider | 批量大小分布 |
| `polaris_token_burn_rate` | Gauge | — 熔断信号 CANONICAL SOURCE |
| `polaris_token_burn_stage3_triggered_total` | Counter | — Stage 3 FULLSTOP 边沿驱动 |
| `polaris_surprise_index` | Gauge | task_type 路由信号 |
| `polaris_tool_calls_total` | CounterVec | tool_category, status, sandbox_tier |
| `polaris_tool_call_latency_ms` | HistogramVec | tool_category |
| `polaris_memory_ops_total` | CounterVec | layer, operation |
| `polaris_task_success_rate` | GaugeVec | task_type |
| `polaris_sandbox_executions_total` | CounterVec | tier |
| `polaris_policy_denials_total` | CounterVec | policy |
| `polaris_goroutines` | Gauge | — |
| `polaris_memory_alloc_mb` | Gauge | — |
| `polaris_ffi_memory_estimate_mb` | Gauge | — 含 FFI 侧内存估算 |
| `polaris_surprise_index_staleness_seconds` | Gauge | — 距上次成功计算 |
| `polaris_surprise_embedding_dropped` | Counter | — LoadShedder 丢弃 |
| `polaris_surprise_async_failures` | Counter | — 异步连续失败 |

### 2.1 CardinalityGuard — 标签基数硬上限 (cap=500)

LRU 缓存 (cap=500); 满时新值 → `<overflow>` 桶。禁止标签: session_id, task_id, trace_id, request_id。受控映射: tool_name→tool_category (builtin/mcp/skill/a2a), agent_id→agent_role (planner/executor/reviewer)。CI: `go test -run TestCardinalityLimits`

---

## 3. Token_Burn_Rate — 熔断信号 (CANONICAL SOURCE)

### 3.1 计算

```
streamAccumulator: 每活跃 LLM 流 (cumulativeTokens, lastUpdateTime)

每 TCP chunk (N 个新 token):
  1. cumulativeTokens += N, 记录 timestamp

每 1s tick:
  2. 瞬时速率 = deltaTokens / 1s
  3. EMA_5s  (α=0.33, 窗口 ~5s)
  4. EMA_30s (α=0.06, 窗口 ~30s)
```

设计原理: TCP Nagle 和缓冲导致 token 到达呈间歇性断崖与爆发——相邻 chunk 间隔可从 100ms 骤降至 1ms，直接 deltaTokens/deltaT 产生虚假加速度。EMA 平滑将速率计算从微秒级 chunk 间隔升级为秒级滑动窗口，消除网络抖动伪影。

### 3.2 熔断

```
EMA_5s  > baseline.P95 × 2.0 → Stage 1 THROTTLE (KillThrottle)  [KillSwitch]
EMA_30s > baseline.P95 × 3.0 → Stage 2 HARD STOP (KillFullStop) [KillSwitch]
EMA_30s > baseline.P95 × 10.0 → Stage 3 KillSwitch FULLSTOP (publishes polaris_token_burn_stage3_triggered_total Counter)
WARN: ema_5s_rate, ema_30s_rate, baseline_p95, action="auto-throttle"
```

**BurnRate baseline 冷启动策略 (HE-Rule-1)**:
- 前 50 个 LLM 调用 → 固定保护值 baseline.P95 = 200 tokens/s（保守上限）
- 50-500 个调用 → EWMA 学习 baseline，保持双值（学习值 vs 保护值），熔断用 min(学习值, 保护值) 以防止失控爆发（HE-Rule-1 保底）
- >500 个调用 → 完全使用动态学习值（绝对上限强制卡死 5000 tokens/s）

---

## 4. Surprise_Index — M3 可观测侧定义

M3 提供两层 SurpriseIndex 计算，M4 按优先级消费：

### 4.0 双层计算架构

**基础计算器 (M3 内置，始终在线)**:
两组件简化版：`0.55 × embeddingCosineDistance + 0.45 × toolSequenceDivergence`
- embedding 余弦距离：基于 M1 Embedding API 当前推理输出 vs 历史同类 task_type 的质心向量
- 工具序列偏离度（toolSequenceDivergence）：当前任务工具调用序列 vs 同 task_type 历史成功序列的归一化 Levenshtein 距离（≤1.0），与完整版同名同义
- 冷启动 (<10 条历史) → 固定 0.5。架构影响：强制导致 M4 走 System 1.5（0.3-0.6），避免了极度缺乏数据时过载触发 System 2，也防止了错误进入 System 1。
- 计算开销：~100-300ms（仅 embedding API 调用），无 M9 依赖
- 代码位置：`pkg/substrate/observability/metrics.go`（L0，不依赖 pkg/swarm/）

**完整计算器 (M9 异步推送，已上线)**:
三组件完整版：`0.4 × embeddingCosineDistance + 0.35 × toolSequenceDivergence + 0.25 × MEMFMatchScore`
计算公式、BoundedWorkQueue 和 LoadShedder 的权威实现位于 M9 §2.0。基础版与完整版的两个共有组件（embeddingCosineDistance / toolSequenceDivergence）实现完全同源，仅完整版多 MEMFMatchScore 一项 M9 依赖。

**M4 消费优先级**: 优先读取 M9 推送的 `polaris_surprise_index`（完整版）。staleness > 60s 时回退到 `polaris_surprise_index_basic`（M3 基础版）。两者均不可用 → 0.5 (System 1.5)。

### 4.1 Prometheus 指标

```
polaris_surprise_index          Gauge   // 完整版 (三组件), M9 异步推送
polaris_surprise_index_basic    Gauge   // 基础版 (两组件), M3 本地计算, 始终可用
polaris_surprise_index_staleness_seconds Gauge  // 距上次成功计算
polaris_surprise_embedding_dropped        Counter // LoadShedder 丢弃计数 (M9 上报)
polaris_surprise_async_failures          Counter // 异步连续失败计数 (M9 上报)
```

### 4.2 Staleness 监控

```
完整版 staleness > 60s → 自动回退基础版 + WARN
基础版 staleness > 120s → WARN
完整版 staleness > 300s → 路由退化到 task_type 级缓存 (最近 24h EMA) → 无缓存 → 基础版 0.5
```

M4 读取 `polaris_surprise_index` Gauge (fallback → `polaris_surprise_index_basic`) 进行 System 1/1.5/2 路由，M3 负责指标暴露和 staleness 告警。

---

## 5. Hardware Capability Probe + AutoConfig + FeatureGate

> 代码见 `pkg/substrate/observability/`（hardware_probe.go, auto_config.go, feature_gate.go, memory_probe_linux.go, memory_probe_darwin.go）。

### 5.1 启动期: HardwareProbe → AutoConfig

```
memoryProbe() → 跨平台探测系统总内存 + 可用内存
       ↓
HardwareProbe → computeTier(): ≥64GB→T3, ≥24GB→T2, ≥16GB→T1, ≥8GB→T0
       ↓
FeatureGate → 计算 15 个特性的 FeatureState (Enabled/Degraded/Disabled)
       ↓
AutoConfig.computeConfig() → 生成完整配置:
  - computeInferenceConfig(): Provider选择 + 自动检测与配置（默认 DeepSeek 系列为主）
  - computeSandboxConfig(): L3平台自动选择(Firecracker/VZ/WSL2) + Wasm并发数
  - computeTrainingConfig(): QLoRA模型大小(1-3B/7B) + PRM启用判断
  - computeStorageConfig(): 引擎选择(SurrealDB-Core全Tier + SurrealDB-Core/SurrealDB-Core HT1+)
  - computeMemoryBudget(): 内存预算按Tier分配 + 可用内存不足时等比缩放
  - computeTierParameters(): 20个数值参数按Tier自动选择(见§5.3)
```

### 5.2 运行时: OSMemoryGuard → FeatureGate.Reassess

OSMemoryGuard 每秒探测 free memory → 三级水位触发 MemoryPressureCallback:

| 水位 | 空闲内存 | 自动动作 |
|------|---------|---------|
| L1 预警 | <1.5GB | QLoRA→Degraded, 禁止新Wasm沙箱 |
| L2 紧急 | <1.0GB | QLoRA/大模型→Disabled, LogicCollapse暂停 |
| L3 临界 | <512MB | Tier-3本地模型卸载, 全部非关键特性禁用 |

恢复后自动清除 Override，特性重新启用。256MB 迟滞防抖动。

### 5.3 TierParameterTable（桶C — 按Tier自动选择）

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

### 5.4 FeatureGate 特性清单（15项）

| 特性 | 门控规则 | 依赖 |
|------|---------|------|
| FeatureLocalInference | ≥Tier3, ≥16GB free | — |
| FeatureLocalEmbedding | ≥Tier0, ≥256MB free | — |
| FeatureQLoRA | ≥Tier1, ≥4GB free | — |
| FeaturePRMTraining | ≥Tier2, ≥8GB free | — |
| FeatureL3Sandbox | ≥Tier0, ≥512MB free, 平台检测 | — |
| FeatureL2Sandbox | ≥Tier0, ≥128MB free | — |
| FeatureGraphRAGFull | ≥Tier0, ≥1.5GB free | — |
| FeatureSurrealDB-CoreFTS | ≥Tier0, ≥256MB free | — |
| FeatureSurrealDB-CoreGraph | ≥Tier0, ≥512MB free | — |
| FeatureLargeLocalLLM | ≥Tier3, ≥24GB free | LocalInference |
| FeatureLogicCollapse | ≥Tier1, ≥1GB free | L2Sandbox |
| FeatureComputerUseGUI | ≥Tier0, ≥512MB, hasDisplay() | — |
| FeaturePresidioPII | ≥Tier1, ≥512MB free | — |
| FeatureWebUI | ≥Tier1, ≥128MB free | — |
| FeatureActivationSteer | ≥Tier1, ≥1.5GB free | LocalInference |

调用方一行代码检查: `observability.GlobalFeatureGate().IsEnabled(observability.FeatureQLoRA)`

---

## 6. OSMemoryGuard — 绝对空闲内存兜底

OSMemoryGuard 与 M13 ResourceGovernor 共享统一三级资源降级体系。**阈值权威定义来源**: `spec/state.yaml §thresholds.memory_pressure`（全局单一来源，M3 和 M13 均读此节，禁止在各模块配置文件中独立硬编码）。M3 `criticalThresholdMB` / `warningThresholdMB` / `cautionThresholdMB` 三个结构体字段在启动时从 `spec/state.yaml §thresholds.memory_pressure` 加载；M13 ResourceGovernor 的对应阈值同样来源于此节，通过 `config.LoadThresholds(dataDir)` 获取，阈值通过 `Thresholds.M3Observability` 字段读取，不使用各自 `~/.polarisagi/polaris/config/m3_observability.toml` / `~/.polarisagi/polaris/config/m13_interface.toml` 的本地副本。两者对同一阈值独立采样，任一触发即执行降级。

实现见 `pkg/substrate/observability/hardware_probe.go` (OSMemoryGuard)。结构体字段: criticalThresholdMB(512MB, **L3 临界**) / warningThresholdMB(1.0GB, **L2 紧急**) / cautionThresholdMB(1.5GB, **L1 预警**) / slopeWindow(4次采样环形缓冲区) / slopeThreshold(-100MB/s) / slopeInterval(5s)。

CheckAndProtect:
1. 获取可用内存 (ReadMemStats + sysinfo)
2. 更新 slopeWindow
3. 斜率快速通道: dV/dt < -100MB/s → 提前预警降级 (禁止新 Wasm 沙箱 + 暂停后台自进化)。即使当前空闲 > 1.5GB，若 4s 内下降 400MB 预判 OOM
4. available < 512 MB → L3 临界: Tier-3 卸载本地模型, 暂停后台自进化, 关闭 SurrealDB-Core cache, runtime.GC() + FreeOSMemory(), 告警
5. available < 1.0 GB → L2 紧急: 限制并发 Agent < 2, 禁止 Logic Collapse, 挂起 Consolidation
6. available < 1.5 GB → L1 预警: 禁止新 Wasm 沙箱, 提高上下文压缩阈值, 暂停后台自进化

---

## 7. MonitorMemoryPressure + FFIMemoryController

### 7.1 MonitorMemoryPressure (后台 goroutine, 每 30s)

```
1. 获取可用系统内存
2. 压力 = 1.0 - available/TotalRAM
3. 抗抖动滞后: 60s 滑动窗口 (最近 2 次采样)
   - 全部 > 80% → Tier 降级 (Tier-3 卸载本地模型, 关闭 SurrealDB-Core 缓存, 暂停后台)
   - 全部 < 50% → Tier 恢复
   - 不一致 → 维持 + DEBUG
4. 写入窗口
```

### 7.2 FFIMemoryController

Go GC 对 purego/Rust FFI 分配的堆外内存无可见性。C/Rust 侧分配 2GB 堆时 Go runtime 不触发 GC。 [Tier-0-Limit]

```
- GOGC: 默认 50, Rust FFI 密集阶段 (M10 GraphRAG, M2 SurrealDB-Core, Wasm 编译) → 25
- 手动 GC 触发 (runtime.GC() + debug.FreeOSMemory()):
  * M10 GraphBuildPipeline 每 50 文档后
  * M6 Logic Collapse Wasm 编译后
  * M2 SurrealDB-Core compaction 后
- GOMEMLIMIT = TotalRAM - 2GB (OS) - 1GB (缓冲)
```

**OS 级兜底** (推荐，非强制): 部署时通过 cgroups v2 `memory.max` / `memory.high` 对 Polaris 进程设置硬内存上限。OSMemoryGuard 以 5s 间隔做应用层斜率检测，cgroups 作为 OS 级最后防线——应用层未及时响应时由内核 OOM killer 按 cgroup 边界精确终止，不影响宿主其他进程。Tier 0 建议 `memory.max = 7.5GB`。

---

## 8. Dynamic Log Level

`POST /_admin/log-level?module=cognition&level=debug` (Unix Domain Socket / 127.0.0.1)

atomicLevelVar: atomic.Int32 无锁存储 slog.Level; levelHandler: slog.Handler 装饰器按 Level 过滤。debug → 30min 定时器 → 自动降回 info (防磁盘写满), 已有定时器先取消。

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

## 10. Decision Log

SQLite decision_log 表 append-only [Storage-SQLite] [HE-Rule-6] [MutationBus]

DDL 见 `internal/protocol/schema/006_decision_log.sql`。

实现见 `pkg/substrate/observability/metrics.go` (DecisionLogger)。Log() 将单条路由决策写入 decision_log 表（append-only, [MutationBus] 串行写）。Analyze() 对 session 内决策做聚合分析，返回路由分布和按 Tier 分层的平均延迟。

---

## 10.1 `[PerformanceDrift]` — 运行时任务质量漂移检测

> 区别于 M12 `RegressionDetector`（CI 离线触发）—— PerformanceDrift 是**运行时滑窗检测**，与 [TokenBurnRate] / [SurpriseIndex] 并列的一等公民漂移信号。

**问题**: M9 自演化（PromptOptimizer / Skill 沉淀）+ Provider 模型版本更新 + 用户画像变化 → 任务成功率可能悄然漂移，CI 离线检测发现时已晚。

**度量**:
- 滑窗: [Window-Quality-10min] (10 分钟, 与连续采样监控共享)
- 维度: `task_type × tier` 二维分布
- 指标:
  - `polaris_task_success_rate` Gauge (10min 内 success/total per task_type)
  - `polaris_task_drift_sigma` Gauge (与 RollingBaseline 偏差的 σ 倍数)

**RollingBaseline**:
- 24h EMA 基线（α=0.05，慢更新避免 RollingBaseline 自身漂移）
- 冷启动 (<100 任务): 基线固定为该 task_type 的 Eval Suite 期望值（M12 §5）
- 每日 04:00 自动归档前一日 baseline 至 `decision_log`，供 M12 ShadowExecutor 对比

**告警阈值**:
| 偏差 | 等级 | 响应 |
|------|------|------|
| > 2σ | WARN | M3 metric + `polaris_drift_warn_total` Counter |
| > 3σ | CRITICAL | M11 候选 [KillSwitch] Stage 1 THROTTLE + 候选 [ESCALATE] HITL |
| > 3σ 持续 30min | KILL | 强制 KillSwitch Stage 1 + M9 PromptOptimizer 当 task_type 候选自动撤回（rollback 最近 staging 批次） |

**与 M11 [FactualityGuard] 联动**:
- D6 抽样率随 drift 信号动态上调（漂移期 ×2，最高 20%）
- 漂移期临时关闭 [BestOfN] N>1（防止漂移污染聚合结果）

**实现锚点**: `pkg/substrate/observability/drift.go` (PerformanceDriftDetector)
- Hook 至 M4 状态机 S_COMPLETE/S_FAILED 转移（捕获 success/fail 信号）
- 与 ContinuousSamplingMonitor (M12 §9) 共享 10min 窗口（避免重复扫描）

**HT0**: 滑窗内存约 200KB（task_type×tier × 10min × 元数据），默认开启。

---

## 11. Langfuse — 隐私门控 + PII 红化

前置条件 (强制):
1. Privacy mode: `local_only` → 完全禁用 (nil, 非 error)
2. PII 红化: 非 local_only → 所有 Prompt/Response 经 M11 PIIGuard.Detect → RedactReplace [PIIGuard]。实体 → 会话 token (如 [NAME_1]), 原始映射不出本地
3. 本地模式: `LANGFUSE_HOST=http://localhost:3000` 仅本地回路
4. 截断: Input ≤ 4096 chars, Output ≤ 2048 chars, 完整仅本地 audit log

导出: (0) local_only → return nil (1) PIIGuard.Redact (2) 截断 (3) Trace(SessionID) (4) Generation (5) gen.End()

---

> [HE-Rule-1] 物理实现: 从第 0 行代码起全链路可追溯——每次 token, tool call, memory 读写必须可追溯。

---

## 14. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| OTel collector 阻塞 | 丢弃非关键 span + WARN，保持 metrics + 关键日志 | collector 恢复后自动重连 |
| Prometheus metrics 文件满 | 循环覆盖旧 series（保留最近 72h） | 用户清理磁盘 |
| slog 写入磁盘失败 | 降级到 stderr (ring buffer 128KB) | 磁盘恢复后切回文件 handler |
| HardwareProbe 启动失败 | 固定 Tier0 最低配置启动 | 重启重新探测 |
| BurnRate EMA 计算线程崩溃 | 熔断器降级为原始速率（无 EMA 平滑，保守超标触发） | suture 自动重启计算线程 |

与 OSMemoryGuard 协同: M3 MonitorMemoryPressure 每 30s 推送压力等级 → Tier 自动降级/恢复，阈值与 M13 ResourceGovernor 共享。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m3_observability`。

## 15. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | TokenBurnRate 流式 token 数据来源、gen_ai.* Span 属性注入 | M1 §5 |
| M2 Storage | DecisionLog 写入（MutationBus 串行写）、EventLog 审计轨迹 | M2 §2.1 |
| M4 Agent Kernel | SurpriseIndex consumer（System 1/1.5/2 路由）| M4 §5 |
| M9 Self-Improve | SurpriseIndex 完整版 producer（MEMF 依赖）、prometheus Gauge 异步推送 | M9 §2.0 |
| M11 Policy Safety | KillSwitch 熔断信号（M3 → M11 推送 TokenBurnRate）| M11 §4.3 |
| M13 Interface | ResourceGovernor 三级降级共享阈值 | M13 §2.0 |
| 接口定义 | TokenBurnRate/SurpriseIndex Prometheus 指标 | pkg/substrate/observability/metrics.go |
| 全局字典 | HE-Rule-1 可观测优先、TokenBurnRate/SurpriseIndex 完整定义、Window-* 时间窗常量 | 00-Global-Dictionary §2, §3, §10 |
| 时序图 | KillSwitch 触发链（M3 BurnRateDetector 的角色）| DIAGRAMS.md#killswitch |
