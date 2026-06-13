# 模块 1: Inference Runtime

> API 优先架构。Provider Router 为核心。本地推理仅隐私/离线备选。
> Go 实现 | [HE-Rule-1] [HE-Rule-2] [HE-Rule-3] [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
> [Module-Topology] [Code-Package-Mapping] [Tier-0-Limit] [Tier-1-Limit]
> **§跳读**: 0:10 职责 / 0-ter:24 不变量速查 / 1:37 默认模型 / 2:43 Provider接口 / 3:49 Adapter / 4:64 Router / 4.4:98 ComplexityDeterminer / 4.5:107 Route方法 / 5:122 Token预算 / 6:186 SemanticCache / 7:208 Fallback / 8:251 本地推理local_only / 9:278 ModelVersion / 12:285 349(SOFT)降级 / 13:313 依赖

---

## 0. 职责边界

| M1 **是** | M1 **不是** |
|-----------|-------------|
| LLM 推理的统一入口 | 会话管理器（M4） |
| Provider 路由与适配 | Prompt 构建器（M4 PromptFn） |
| 结构化输出强制（JSON Schema + GBNF） | 业务逻辑决策者 |
| Token 计数与流式成本追踪 | 预算策略制定者（M11） |
| 本地推理侧车管理（离线/隐私 fallback） | 模型训练（M9） |
| 流式 SSE 帧归一化（Anthropic/OpenAI/DeepSeek → 统一 StreamEvent） | 推理结果质量评估（M12） |
| 多模态请求预处理（图片降采样/格式归一，`Infer`/`StreamInfer` 入口统一执行） | 业务侧多媒体采集（M13 Gateway） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M1_01 | 所有 LLM 调用经 Provider Router，禁止裸 HTTP 调用 | CI `provider_lint` 扫描 |
| inv_M1_02 | 每次 Infer/StreamInfer 须写 EventLog（全文 + usage） | M2 events 表审计 |
| inv_M1_03 | L1/L2 路由严格零 LLM 调用——仅 L3 级联路由可用 LLM 判定 | code review 强制 |
| inv_M1_04 | 流中断时 usage 标记 `estimated=true`，禁止静默丢弃 | M3 指标 `estimated` 标签 |
| inv_M1_05 | CircuitBreaker 全熔断返回 ErrAllProvidersExhausted，不静默降级 | 集成测试 |
| inv_M1_06 | API Key 经 CredentialVault JIT 获取，`[]byte` 使用后 `subtle.ConstantTimeCopy` + memclr 清零 | CI `key_leak_lint` 扫描 |

---

## 1. 默认模型

Provider-agnostic 设计。`configs/defaults.toml` 推荐组合：DeepSeek V4 系列（已在 Tier-0 长程验证）；备选 Claude Sonnet 4.6 / GPT-5.x 等任何符合 §2 Provider Interface 的实现。

---

## 2. Provider Interface

接口定义见 `internal/protocol/interfaces.go` (Provider)，包含 `ModelID() string` 支持真实模型身份认知的系统提示词注入。类型定义见 `internal/protocol/types.go` (InferRequest/InferResponse/StreamEvent/ProviderCapabilities/TokenizerAdapter)。

---

## 3. Provider Adapter

每个 Adapter 是 Provider 接口的具体实现，封装在 `pkg/substrate/inference/adapters/` 内，外部 SDK（如 openai-go / anthropic-sdk-go）仅在 Adapter 内引用，不暴露至上层。

每个 Adapter 内置:
- Pre-flight token count
- Post-flight usage recording
- **KV Cache 路由与注入（system_and_3 策略）**: Anthropic Adapter（`pkg/substrate/inference/adapter_anthropic.go`）实现 4 断点策略：① system prompt（`WithAnthropicPromptCaching()` 启用时转为 text array 注入 `cache_control`）② tools 定义末项（会话内不变，命中率高）③ 倒数第 2 条非 system 消息 ④ 最后 1 条非 system 消息。断点 3+4 缓存会话历史前缀，多轮对话输入 token 成本可降约 75%。此策略由 `applyMsgCacheControl()` 辅助函数统一处理 string/array 两种 content 格式。与 M5 ContextAssembler 组装顺序（ImmutableCore→Procedural→Episodic）耦合，stable 层先于 volatile 层，最大化 prefix 命中率。
- 结构化错误 (4xx / 5xx / rate limit / timeout)
- 自动重试 (exponential backoff + jitter)
- **SSE 帧归一化**: Anthropic SSE (`event:` 行 + JSON data) / OpenAI SSE (`data: [DONE]` 哨兵) / DeepSeek JSON 行流 → 统一 `chan StreamEvent`，上层仅见 4 类事件
- API Key 通过 `[CredentialVault].Get(providerName)` JIT 获取；使用后立即调用显式 memclr (`for i := range key { key[i] = 0 }`) 清零并 `runtime.KeepAlive(key)` 防止逃逸优化丢失清零；Header 注入路径走 `subtle.ConstantTimeCopy` 避免短路 timing 信号

---

## 4. Provider Router

### 4.1 三层递进路由

| 层 | 机制 | 延迟 | 命中率目标 |
|----|------|------|----------|
| L1 规则路由 | 启发式分派（task_type + token 长度，纯规则，零 LLM 零 ML 模型）| <1ms | 90% |
| L2 复杂度评估 | 启发式打分（ToolCount + outputEstimate EMA）输出 0.0–1.0 复杂度，**非 ML 分类器** | ~5ms | 9% |
| L3 级联路由 | L1+L2 低置信度时调用 Budget Pool LLM 推荐 provider slot | ~50ms | <1% |

L3 LLM 输出 provider 推荐槽位，由 Route() 确定性函数验证（预算门控、quota 可用性、CircuitBreaker 状态）后采纳。LLM 填空，Go Router 持有最终决策权，符合 [HE-Rule-5]。L1/L2 严格零 LLM 调用——L2 的"复杂度打分"是基于 ToolCount/outputEstimate 的纯启发式公式，不是机器学习分类器。

路由硬约束: ① 90% 走 L1 ② 质量优先 → 延迟 → cost tiebreaker ③ 仅 L3 级联可用 LLM 判定 ④ Pre-flight 成本估算保留 ⑤ Cache 一等公民 ⑥ 3 级软限流（告警不阻断）⑦ CircuitBreaker 必备

### 4.2 三个核心 Model Pool

| Pool | 候选模型示例 | 默认用途 |
|------|------------|---------|
| Budget | `<flash-class>`（DeepSeek V4 Flash 等）| 默认主路径（分类、摘要、路由判断、简单工具）|
| Standard | `<standard-class>`（DeepSeek V4 / Claude Sonnet 4.6 等）| 代码生成、多步推理、复杂工具编排 |
| Reasoning | `<reasoning-class>`（DeepSeek V4 Pro / o-系列等）| 复杂架构决策、长链推理、自反思 |

> **强烈推荐**：`configs/defaults.toml` 选 DeepSeek V4 系列组合（价格优势与 Tier-0 极速响应兼具）。系统已不再为普通设备维护本地模型策略（本地模型仅限 Tier-3 超高配置节点独立启用）。

### 4.3 多模态请求预处理

`InferenceRouter.normalizeInferRequest`（`pkg/substrate/inference/media_opt.go`）在 `Infer`/`StreamInfer` 入口统一执行，覆盖所有调用路径（Gateway、Kernel、MCP 工具结果、Swarm）：

- 图片降采样：长边 > 1568px 等比缩放，满足主流 Vision Provider token 预算上限
- 格式归一：PNG/GIF 转 JPEG（quality=85），减少传输体积
- 不修改文本内容、工具定义、路由参数，对非图片 Part 零开销

调用方无需手动处理；MCP 工具返回的 `protocol.ImagePart` 由此路径自动压缩。

### 4.4 ComplexityDeterminer

**[计划中]** — 三层路由（L1规则/L2复杂度/L3 LLM）为设计目标；当前 `InferenceRouter`（`pkg/substrate/inference/router.go`）实现的是**单层 HealthScore 路由**（成功率×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1），不含 L2 复杂度打分和 Budget/Standard/Reasoning Pool 分层。多 Provider 按健康分降序选择，失败时 Failover 至次优。

### 4.5 Route 方法

实现见 `pkg/substrate/inference/router.go:InferenceRouter.Infer()`/`StreamInfer()`。
Provider 选择：`ProviderRegistry.best(req)` 按 healthScore 降序 + CircuitBreaker 状态 + 多模态能力过滤选取最优 Entry → 调用 Provider → 失败则 Failover（跳过已失败 Provider 重新 best 选取）。全部不可用 → `ErrAllProvidersFailed`。

### 4.5 路由配置参数

| 参数 | 默认值 |
|------|--------|
| L1 目标命中率 | `spec/state.yaml §m1_router.l1_target_hit_rate` |
| L1 超时 | <1ms |
| L2 超时 | ~5ms |
| L3 超时 | ~50ms |
| CircuitBreaker 连续失败熔断阈值 | `spec/state.yaml §m1_router.circuit_breaker_failure_count` |
| CircuitBreaker 冷却时间 | `spec/state.yaml §m1_router.circuit_breaker_cooldown_seconds` |
| CircuitBreaker 半开探测上限 | `spec/state.yaml §m1_router.circuit_breaker_half_open_max` |
| MaxStreamBufferSize | `spec/state.yaml §m1_router.max_stream_buffer_kb`（Tier 1+ 可配至 1MB） |

---

## 5. Token 预算与成本控制

### 5.1 控制维度

| 维度 | 默认值 | 超标行为 |
|------|--------|---------|
| 单次请求 MaxTokens | 16384 | 软截断，告警不阻断 |
| 单次请求预算 | $0.50 | 告警 |
| Session LLM 子预算 | 100K tokens | 告警 + 触发上下文压缩 |
| 日/月预算 | 无硬上限 | [TokenBurnRate] 异常检测 |
| Provider 配额 | 可配置 | 自动切换备选 |
| 后台任务 LLM 调用 | — | Stage1 THROTTLE → 挂起; Stage2 PAUSE → 跳过 |

### 5.2 Reasoning Budget Scheduling

实现见 `pkg/substrate/stream_guard.go`。

三模式: `fixed` (MaxReasoningSteps=5, MaxThinkingTokens=4096) / `adaptive` (`min(16384, 4096×(1+SI×3))`) / `batch` (32K, 夜间 2-6am, 非交互)。
[TokenBurnRate] Stage1 THROTTLE → 降一档: batch→adaptive, adaptive→fixed, fixed→256。

### 5.2-bis Test-Time Compute — `[ThinkingMode]`

> 对齐 DeepSeek V4 Pro / Claude Sonnet thinking 原生推理范式。MCTS/BestOfN/多候选路由已废弃——Provider 原生扩展思考（native extended thinking）覆盖相同能力。

**`[ThinkingMode]`** — M4 `SelectThinkingMode` 按以下信号三档驱动，由 Adapter 翻译为 Provider-specific API 字段:

| 档位 | 触发条件 | DeepSeek V4 Pro 映射 | Claude 映射 |
|------|---------|----------------------|-------------|
| `ThinkingDisabled` | SI < 0.3 且 replanCount=0 且 TaintLevel < 3 | 无 thinking 字段 | 无 thinking 字段 |
| `ThinkingHigh` | 0.3 ≤ SI < 0.6 | `reasoning_effort="high"` + `thinking.type="enabled"` | `thinking.budget_tokens=4096` |
| `ThinkingMax` | SI ≥ 0.6 或 replanCount > 0 或 TaintLevel ≥ 3 | `reasoning_effort="max"` + `thinking.type="enabled"` | `thinking.budget_tokens=16384` |

**约束**:
- DeepSeek V4 Pro thinking 启用时温度强制为 0（API 要求）
- 多轮工具调用序列中，`reasoning_content` 必须随 assistant 消息回传至下一轮 prompt——Adapter 负责从响应中提取并写入 `ProviderResponse.ReasoningContent`；M4 通过 `StateContext.LastReasoningContent` 跨轮持有
- HT0 下 `ThinkingMax` 可正常运行（DeepSeek V4 Pro token 成本低，无预算门控）

**`[ReasoningTokens]`** 计量: `InferResponse.Usage` 分字段 `reasoning_tokens`，计入 [TokenBurnRate]；M3 导出 `polaris_reasoning_tokens_total` Gauge。

### 5.3 StreamBudgetGuard

实现见 `pkg/substrate/stream_guard.go:StreamBudgetGuard/TokenBurnDetector`。

GuardChunk 摊销检查（每 100 chunk 或首 chunk）:
- L1: 剩余预算 <=0 → WARN（不阻断）
- L2: TokenBurnDetector 加速度检测（5s 窗口, 3 采样点, accel > 3× baseline → BurnAlert → FatalStreamAbort 硬阻断）
- L3: 预算耗尽 → 硬阻断

TokenBurnDetector 仅做单流加速度检测，系统级燃烧速率从 M3 `polaris_token_burn_rate` Gauge 单源读取。

### 5.4 trackStreamCost

实现见 `pkg/substrate/stream_guard.go:TrackStreamCost`。

流正常结束 → 精确 API usage。流中断: FatalStreamAbort → 丢弃输出 → M4 S_REPLAN（禁止 JSONRepair——失控 LLM 截断输出语义不可靠）；> MaxStreamBufferSize(256KB) → workspace 临时文件 + ErrResponseTooLarge；正常中断 → JSONRepair + 双重安全校验。

### 5.5 JSONRepair

实现见 `pkg/substrate/stream_guard.go:JSONRepair`。
栈式括号匹配 → 自动闭合 → 移除不完整 key-value。确定性 Go 实现 <1ms。
双重安全校验: (1) required 字段完整性；(2) SideEffects > read_only 且 DiscardedKeys 非空 → 强制拒绝 → S_REPLAN。

---

## 6. Semantic Cache

### 6.1 EmbeddingBatcher

实现见 `pkg/substrate/embedding_batcher.go`。

双优先级队列：pendingHigh[180]（SurpriseIndex、交互式查询）/ pendingLow[76]（GraphRAG、Consolidation）。batchWindow=10ms，maxBatchSize=100，保留 20% 槽位给 Low 防饥饿。Low >100ms 升 High。背压：High cap 80% → 指数退避（50ms 初始，max 2s）；Low cap 80% → 排队 30ms。

**文本去重（dedup）**：相同文本重复入队时，仅占用一个队列槽位，额外等待者追加到扇出列表；API 调用返回后，结果同步广播给所有等待同一文本的调用方。消除并发场景下对相同文本的重复 Embedding API 调用。

[PIIGuard] 红化预处理后文本发远程 API。

### 6.2 SemanticCache

实现见 `pkg/substrate/semantic_cache.go`（SemanticCache struct / CacheStore 接口 / Embedder 接口 / CacheEntry struct / Get() / Put() / evictLRU()）。

`[向量索引后端待激活]` CacheStore 接口依赖向量索引后端（设计为 SurrealDB-Core HNSW）提供 `FindClosest`；当 store=nil 时所有操作为安全空操作，不参与推理路由。Get/Put/LRU 淘汰逻辑已完整实现，等向量后端接入后即可启用。

缓存三重匹配: RequestHash + Namespace + SystemPromptHash。hashRequest: SHA-256(Namespace + SystemPromptHash + ContextHint.Fingerprint + ActiveControlVectorLabels + TaskType + MessageContents)。满时 LRU 淘汰 MaxEntries/10。SimilarityThreshold / MaxEntries / TTL 见 `spec/state.yaml §m1_router.semantic_cache_similarity_threshold` / `semantic_cache_max_entries` / `semantic_cache_ttl_hours`。

---

## 7. Fallback Chain

### 7.1 三级 Fallback

Primary → Secondary (同级备选) → Tertiary (降级备选) → GracefulDegradation → [ESCALATE]

### 7.2 失败响应

| 失败模式 | HTTP | 策略 |
|---------|------|------|
| Rate Limit | 429 | Exponential backoff + 换 provider |
| Server Error | 5xx | 立即换 provider + 冷却原 provider |
| Timeout | — | 减少 MaxToken 后重试 |
| Content Filter | 400 | 不重试 |
| Token Limit | 400 | 压缩 context 后重试 |

### 7.3 CircuitBreaker + FallbackExecutor

CircuitBreaker 三态：Closed（正常）→ Open（熔断，冷却期拒绝请求）→ HalfOpen（探测）→ Closed。`failureThreshold` 次连续失败触发 Open，冷却 `cooldownPeriod` 后进入 HalfOpen，探测成功恢复 Closed。参数见 `spec/state.yaml §m1_router.circuit_breaker_*`。

`FallbackExecutor.Execute()` 按注入的 Provider 列表顺序依次检查可用性，选中第一个可用 Provider 并更新 CircuitBreaker 状态；全部不可用时标记失败并返回 `ErrAllProvidersFailed`，触发调用方进入 GracefulDegradation 或 [ESCALATE] 路径。实现见 `pkg/substrate/fallback.go`。

**StreamInfer Failover**：`StreamInfer` 与 `Infer` 同等纳入 CircuitBreaker 覆盖。每次 StreamInfer 调用记录延迟并更新 HealthScorer，若 Provider 返回错误则触发 Failover 切换至下一可用 Provider（逻辑与 Infer 路径一致）。流式错误中断时 usage 标记 `estimated=true`（inv_M1_04）。

**Provider 恢复事件**: 当所有 Provider 均处于 Open 状态（`ErrAllProvidersExhausted` 已触发）后，任意一个 Provider 的 CircuitBreaker 完成 HalfOpen→Closed 转换（半开探测成功）时，M1 向 M2 Outbox 写入：
  `MutationIntent{Table:"outbox", Op:OpInsert, Payload:{target_engine:"m4_provider_recovery", provider_id:<providerID>, recovered_at:<timestamp>}}`
此事件经 M2 Outbox Worker 投递至 M4（M4 §8 Provider 恢复唤醒路径），M4 据此自动唤醒处于 `Suspended(suspend_reason=provider_exhausted)` 的任务。
多 Provider 场景下，若多个 Provider 同时恢复，事件幂等合并（M4 Outbox Worker 在同一 batch 内去重 task_id，避免重复唤醒）。


### 7.4 HealthScorer

| 维度 | 权重 | 指标 |
|------|------|------|
| 可用性 | 40% | 最近 N 次成功率 |
| 延迟 | 30% | P95 延迟趋势 |
| 成本 | 20% | 实际 vs 预估偏差 |
| 质量 | 10% | token 截断率、finish_reason 分布 |

健康度 < 阈值 → 降权，减少路由分配。

---

## 8. 本地推理（local_only 模式）

隐私/离线备选。不参与 Provider Router 主路由，仅作为 Fallback 最后一级。llama.cpp 统一负责 Embedding + LLM 推理 + Rerank，GGUF Q4_K_M。利用 Metal (macOS) / CUDA (Linux) 硬件加速，单 FFI 桥接点，统一 GGUF 量化生态。

模型加载策略:

| 模型 | 用途 | 大小 | 加载条件 |
|------|------|------|---------|
| Qwen3-32B-Q4_K_M | LLM 推理 | ~20GB | Tier 3+ (64GB) local_only 专享 |
| bge-reranker-base-Q4_K_M | Cross-encoder 重排 | ~50MB | 懒加载，首次 rerank 请求时 |
| BGE-small-Q4_K_M | 本地 Embedding | ~100MB | local_only 或隐私 embedding 模式。输出 384-dim，通过 SurrealDB-Core 双索引隔离表（index_local_384）维持语义检索能力，避免永久降级 BM25。详见 M10 §2 双索引方案 |

### 8.1 LocalProvider

LocalProvider 通过 llama.cpp FFI 提供本地 GGUF 模型的 `Infer`/`StreamInfer`/`Rerank` 能力：启动时校验文件完整性与内存余量，懒加载模型，通过 `chat template + GBNF grammar` 支持结构化输出；Rerank 复用同进程 llama.cpp `/rerank` 端点，50 文档交叉编码 CPU < 50ms；`EvictKVCache` 在 Control Vector 变更 / 热切换 / Session 重置时清理 KV。

当前实现状态：**[计划中]** — LocalProvider 接口已在 `internal/protocol/interfaces.go` 定义，但 `pkg/substrate/inference/` 中未见 llama.cpp FFI 桥接实现；本地推理能力待 Tier-3 节点专项激活。

### 8.2 生命周期

- 懒加载: 首次请求时加载，非 `local_only` 模式默认不加载
- 空闲卸载: `local_only` 模式下 30min 无请求卸载
- Tier 降级: OSMemoryGuard 检测空闲内存 < 1.0GB → 强制卸载
- 热切换: `/model local <model_id>` 卸载当前 → 加载新模型

---

## 9. ModelVersionRegistry

`ModelVersionEntry` 持有 Provider/ModelID/版本/废弃状态、PromptTemplate/ToolCallStyle/MaxContext/Capabilities 等元数据，以及 `ValidatedOn`（技能兼容测试通过列表）和 `CompatibilityScore`（0-1）。

`OnModelUpgrade` 流程：对比行为差异 → 重跑关联技能兼容测试 → 更新 ValidatedOn 与 CompatibilityScore，低于 0.8 时发 WARN。废弃迁移按 CompatibilityScore 分三档（≥0.9 自动 / 0.7-0.9 自动+WARN / <0.7 禁止自动）；连续 3 次 4xx/5xx 自动回退。Embedding 模型废弃时触发 M2 OnlineReindexer 全量重嵌（影子表 → Blue-Green swap）。

**[计划中]** — `ModelVersionRegistry` 当前未在 `pkg/substrate/inference/` 中落地，Provider Adapter 通过 `resolveXXXModel()` 函数处理废弃模型名到新名称的映射（已实现：Anthropic/OpenAI/DeepSeek 各有 resolve 函数）；完整的版本管理注册表待实现。
## 12. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| 单 Provider 限流/不可用 | EWMA p95 > 阈值 / 5xx 连续 | 仅本 Provider | Fallback：同 Pool → 降级 Pool → 拒绝 | 半开探测 | 同 Pool 全断 → audit severity=warn |
| 全部 Provider 熔断 | CircuitBreaker 全开 | 全模块 LLM 路径 | 返回 ErrAllProvidersExhausted | 冷却期满后半开探测 | 持续 > 5min → audit |
| SemanticCache 满 (10000 entries) | 内存水位触发 | 仅缓存查询 | LRU 淘汰 MaxEntries/10 条目 | 自动 | — |
| StreamBudgetGuard L2 触发 | TokenBurnDetector 加速度检测 (5s 窗口, accel > 3× baseline) | 单流 | FatalStreamAbort 硬阻断 → M4 S_REPLAN | TokenBurnRate 恢复正常后自动解除 | 单 session 反复触发 ≥3 次 → audit |
| Tier-3 本地模型加载失败 | llama.LoadModelFromFile err / RSS 检测 | local_only 全模块 LLM 路径 | 降级远程 API（非 local_only）；local_only → ErrLocalModelUnavailable | 空闲内存恢复后重新加载。local_only 模式下若持续 > 30s 无法重载 → 触发 M13 ResourceGovernor local_only 死锁恢复 | local_only 模式下死锁恢复仍失败 → 必须 HITL |
| Embedding API 不可用 | err return / timeout | 检索路径 | EmbeddingBatcher 返回错误，调用方降级 BM25/FTS5 | API 恢复后自动切回 | 全断 > 1h → audit |
| EmbeddingBatcher High 队列 >80% | 队列水位 | 非交互 embedding 请求 | 指数退避 (50ms→2s) | 队列水位下降后恢复正常 | 持续 > 5min → audit |
| ModelVersionRegistry 废弃迁移失败 | 连续 3 次 4xx/5xx | 单模型 | 保持旧模型 + WARN + 禁止自动切换 | 下一检测周期重试 | 版本 EOL 日迫近 |

与 OSMemoryGuard 协同: (仅 Tier-3) 空闲内存 < 1.0GB → 强制卸载本地模型；L3 临界 → 全部 LLM 调用路由至远程 API。


## 10. 凭证池与速率限制追踪（已实现）

**CredentialPool**（`pkg/substrate/inference/credential_pool.go`）：多 API Key 线程安全池，支持四种选择策略（FillFirst / RoundRobin / Random / LeastUsed）。`Pick()` 按策略选取冷却期已过的凭证；`RecordResult(err)` 调用 `Classify(err)` 并按 FailReason 设置冷却期（Auth 5min / Billing+RateLimit 60min / AuthPermanent 30天）。`CredFn()` 返回 `func() string`，与各 Adapter 构造函数直接兼容。

**RateLimitTracker**（`pkg/substrate/inference/rate_tracker.go`）：解析 Provider HTTP 响应头中的 12 个速率限制字段（分钟/小时 × 请求/Token），提供 `SuggestDelay()` 供退避决策。`RateLimitCapturingTransport` 作为 `http.RoundTripper` 包装层自动捕获限速头，无需改动各 Adapter。`BackoffConfig.Delay(attempt)` 实现去相关抖动指数退避（防雷群效应）。

**ErrorClassifier**（`pkg/substrate/inference/error_classifier.go`）：`Classify(err)` 提取 HTTP 状态码 + 关键词，分类为 17 种 FailReason，并填充 `Retryable/ShouldCompress/ShouldRotateCredential/ShouldFallback` 四个布尔恢复提示。覆盖 Anthropic/OpenAI/DeepSeek/Gemini/Ollama/阿里云/火山引擎等多 provider 错误体格式。

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m1_router`。

## 13. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M2 Storage | SemanticCache 的 SurrealDB-Core 后端存储 | M2 §1.3 |
| M3 Observability | TokenBurnRate CANONICAL SOURCE（M3 单源持有）、SurpriseIndex consumer | M3 §3, §4 |
| M4 Agent Kernel | Provider.Infer/StreamInfer 消费者（LLM 调用唯一入口）| M4 §10 |
| M9 Self-Improve | PromptOptimizer 使用 LLM 调用（经 Provider 路由）| M9 §1.1 |
| M11 Policy Safety | CredentialVault API Key JIT 获取、SafeDialer 网络出口 | M11 §5.2, §6 |
| 接口定义 | Provider/InferRequest/InferResponse/StreamEvent | internal/protocol/interfaces.go, types.go |
| 全局字典 | TokenBurnRate/SurpriseIndex 完整定义 | 00-Global-Dictionary §3 |
