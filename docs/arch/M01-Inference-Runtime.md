# 模块 1：Inference Runtime —— 推理运行时

> **一句话定位**：LLM 推理的统一入口，API 优先架构，Provider Router 为核心，本地推理仅作隐私/离线备选。
>
> **实现语言**：Go　|　**代码位置**：`internal/llm/`
>
> **相关约束**：[HE-Rule-1]、[HE-Rule-2]、[HE-Rule-3]、[HE-Rule-4]、[HE-Rule-5]、[HE-Rule-6]、[Module-Topology]、[Code-Package-Mapping]、[Tier-0-Limit]、[Tier-1-Limit]
<!-- §跳读: 0:12 职责 / 0-ter:26 不变量速查 / 1:41 默认模型 / 2:47 Provider接口 / 3:55 Adapter / 4:82 Router / 4.4:98 ComplexityDeterminer / 4.5:107 Route方法 / 5:160 Token预算 / 6:236 SemanticCache / 7:285 Fallback / 8:349 本地推理local_only / 9:402 ModelVersion / 12:439 349(SOFT)降级 / 13:474 依赖 -->

---

## 0. 职责边界

| M1 **是** | M1 **不是** |
|-----------|-------------|
| LLM 推理的统一入口 | 会话管理器（M4） |
| Provider 路由与适配 | Prompt 构建器（M4 PromptFn） |
| 结构化输出强制（JSON Schema + GBNF（GGML BNF，GGML 巴科斯范式）） | 业务逻辑决策者 |
| Token 计数与流式成本追踪 | 预算策略制定者（M11） |
| 本地推理侧车管理（离线/隐私 fallback） | 模型训练（M9） |
| 流式 SSE 帧归一化（Anthropic/OpenAI/DeepSeek → 统一 StreamEvent） | 推理结果质量评估（M12） |
| 多模态请求预处理（图片降采样/格式归一，`Infer`/`StreamInfer` 入口统一执行） | 业务侧多媒体采集（M13 Gateway） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M1_01 | 所有 LLM 调用经 Provider Router，禁止裸 HTTP 调用 | CI `provider_lint` 扫描 |
| inv_M1_02 | 每次 `Infer`/`StreamInfer` 须写 EventLog（全文 + usage） | M2 events 表审计 |
| inv_M1_03 | L1/L2 路由严格零 LLM 调用——仅 L3 级联路由可用 LLM 判定 | Code Review 强制 |
| inv_M1_04 | 流中断时 EventLog 追加 `streaming_interrupted: true` 字段，禁止静默丢弃 | EventLog 字段审计 |
| inv_M1_05 | CircuitBreaker 全熔断返回 `ErrAllProvidersFailed`，不静默降级 | 集成测试 |
| inv_M1_06 | API Key 经 `credentialFn() []byte` JIT 获取，RoundTrip 返回后立即 `Header.Del` + `clearBytes` 清零原始切片；Anthropic 适配器用 `keyInjectRT` transport，其余适配器用 `setAuthHeader` + defer cleanup（含 `Header.Del`） | Code Review |

> 进入此模块前必读 **LLM 超时保护 + Failover 全量遍历工程化阵阱**：`docs/specs/09-LLM-Agent-Production.md`（HE-Rule-1/5 实例: A-01/A-04/A-05 + P-1/P-2/P-7）

---

## 1. 默认模型

Provider-agnostic 设计。`configs/defaults.toml` 推荐组合：DeepSeek V4 系列（已在 Tier-0 长程验证）；备选 Claude Sonnet 4.6 / GPT-5.x 等任何符合 §2 Provider Interface 的实现。

---

## 2. Provider Interface

接口定义见 `internal/protocol/interfaces.go`（`Provider`），包含 `ModelID() string` 支持真实模型身份认知的系统提示词注入。

类型定义见 `internal/protocol/types.go`（`InferRequest` / `InferResponse` / `StreamEvent` / `ProviderCapabilities` / `TokenizerAdapter`）。

---

## 3. Provider Adapter

每个 Adapter 是 Provider 接口的具体实现，直接位于 `internal/llm/`。外部 SDK（如 `openai-go` / `anthropic-sdk-go`）仅在 Adapter 内引用，不暴露至上层。

每个 Adapter 内置：

- Pre-flight token count
- Post-flight usage recording
- **KV（Key-Value，键值）Cache 路由与注入（system_and_3 策略）**：
  Anthropic Adapter 实现 4 断点策略：
  1. system prompt（PromptCaching 启用时转为 text array 注入 cache_control）
  2. tools 定义末项（会话内不变，命中率高）
  3. 倒数第 2 条非 system 消息
  4. 最后 1 条非 system 消息
  
  断点 3+4 缓存会话历史前缀，多轮对话输入 token 成本可降约 75%。与 M5 上下文组装顺序（ImmutableCore → Procedural → Episodic）耦合，stable 层先于 volatile 层，最大化 prefix 命中率。
- 结构化错误（4xx / 5xx / rate limit / timeout）
- 自动重试（exponential backoff + jitter）
- **SSE 帧归一化**：Anthropic SSE（`event:` 行 + JSON data）/ OpenAI SSE（`data: [DONE]` 哨兵）/ DeepSeek JSON 行流 → 统一 `chan StreamEvent`，上层仅见 4 类事件。
- **凭证安全获取与清理**：
  API Key 通过 `credentialFn() []byte` JIT 获取，RoundTrip 返回后立即 `Header.Del` + `clearBytes` 清零。两种实现并存：
  - Anthropic 适配器使用 `keyInjectRT` RoundTrip 拦截器（`Header.Del("x-api-key")`）；
  - OpenAI/DeepSeek/Google 等适配器使用 `setAuthHeader` + defer cleanup（`Header.Del("Authorization")` + `clearBytes`）。
  两种模式均满足 inv_M1_06。

---

## 4. Provider Router

### 4.1 三层递进路由

| 层 | 机制 | 延迟 | 命中率目标 |
|----|------|------|----------|
| L1 规则路由 | 启发式分派（task_type + token 长度，纯规则，零 LLM 零 ML 模型）| <1ms | 90% |
| L2 复杂度评估 | 启发式打分（ToolCount + outputEstimate EMA（Exponential Moving Average，指数移动平均））输出 0.0–1.0 复杂度，**非 ML 分类器** | ~5ms | 9% |
| L3 级联路由 | L1+L2 低置信度时调用 Budget Pool LLM 推荐 provider slot | ~50ms | <1% |

L3 LLM 输出 provider 推荐槽位，由 `Route()` 确定性函数验证（预算门控、quota 可用性、CircuitBreaker 状态）后采纳。

LLM 填空，Go Router 持有最终决策权，符合 [HE-Rule-5]。

L1/L2 严格零 LLM 调用——L2 的“复杂度打分”是基于 ToolCount/outputEstimate 的纯启发式公式，不是机器学习分类器。

> **约束（路由硬约束）**：
> 1. 90% 走 L1
> 2. 质量优先 → 延迟 → cost tiebreaker
> 3. 仅 L3 级联可用 LLM 判定
> 4. Pre-flight 成本估算保留
> 5. Cache 一等公民
> 6. 3 级软限流（告警不阻断）
> 7. CircuitBreaker 必备

### 4.2 三个核心 Role Pool

实际实现：`ProviderRegistry`（`router.go`）按 `role` 字段分组注册，`BestForRole(role)` 返回最高健康分可用 Provider。

| Role | 候选模型示例 | 默认用途 |
|------|------------|---------|
| `general` | `<flash-class>`（DeepSeek V4 Flash 等）| 默认主路径（分类、摘要、路由判断、简单工具）|
| `default` | `<standard-class>`（DeepSeek V4 / Claude Sonnet 4.6 等）| 代码生成、多步推理、复杂工具编排 |
| `reasoning` | `<reasoning-class>`（DeepSeek V4 Pro / o-系列等）| 复杂架构决策、长链推理、自反思 |

> **设计决策**：`configs/defaults.toml` 选 DeepSeek V4 系列组合（价格优势与 Tier-0 极速响应兼具）。系统已不再为普通设备维护本地模型策略（本地模型仅限 Tier-3 超高配置节点独立启用）。

### 4.3 多模态请求预处理

多模态请求预处理（`internal/llm/`，`media_opt` 模块）在所有推理入口统一执行，覆盖 Gateway、Kernel、MCP（Model Context Protocol，模型上下文协议）工具结果、Swarm 全部调用路径：

- 图片降采样：长边 > 1568px 等比缩放，满足主流 Vision Provider token 预算上限
- 格式归一：PNG/GIF 转 JPEG（quality=85），减少传输体积
- 不修改文本内容、工具定义、路由参数，对非图片 Part 零开销

调用方无需手动处理；MCP 工具返回的 `protocol.ImagePart` 由此路径自动压缩。

### 4.4 ComplexityDeterminer

**已废弃并移除**：项目初期曾设计 `determineComplexity`（基于工具数量 + 预估输出 token 给出 1-3 级复杂度评分）作为 L2 路由辅助。

但在实际验证中发现，当前的单层 HealthScore 路由 + Role Pool（`general`/`default`/`reasoning`）+ ADR-0022（Architecture Decision Record，架构决策记录） ThinkingMode 三档设计已完全能覆盖“简单任务便宜模型、复杂任务深度思考”的核心诉求。为避免引入不必要的判断分支和复杂度，`determineComplexity` 及其相关代码已被彻底删除。

注意与 ADR-0022（ThinkingMode 三档路由，`internal/observability/metrics/metrics.go` 的 `SelectThinkingMode`）区分：后者选的是“思考强度”而非“Provider”，两者是并行机制，不能互相替代。

### 4.5 Route 方法

实现见 `internal/llm/`（`InferenceRouter`）。Provider 选择按健康分降序 + CircuitBreaker 状态 + 多模态能力过滤；失败则 Failover，全部不可用返回 `ErrAllProvidersFailed`。

**CircuitBreaker 独立实现于 `fallback.go`**（非 Router 内部），`FallbackExecutor` 持有 `*CircuitBreaker` 引用，Router 通过 `FallbackExecutor` 执行降级链。

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

> ✅ `NewProviderRegistry(cfg)` 已接受 `M1RouterThresholds`，熔断器参数通过配置注入，TOML 热覆盖生效。

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
| 后台任务 LLM 调用 | — | Stage1 THROTTLE → 挂起；Stage2 PAUSE → 跳过 |

### 5.2 Reasoning Budget Scheduling

实现见 `internal/llm/`（`StreamGuard`）。

支持三种模式：
- `fixed`：MaxReasoningSteps=5, MaxThinkingTokens=4096
- `adaptive`：`min(16384, 4096×(1+SI×3))`
- `batch`：32K, 夜间 2-6am, 非交互

[TokenBurnRate] 处于 Stage1 THROTTLE 时 → 降一档：`batch` → `adaptive`，`adaptive` → `fixed`，`fixed` → `256`。

### 5.2-bis Test-Time Compute — `[ThinkingMode]`

> **设计决策**：对齐 DeepSeek V4 Pro / Claude Sonnet thinking 原生推理范式。MCTS/BestOfN/多候选路由已废弃——Provider 原生扩展思考（native extended thinking）覆盖相同能力。

**`[ThinkingMode]`** —— `internal/observability/metrics/metrics.go` 中 `SelectThinkingMode(replanCount, maxTaint, surpriseIndex)` 三档驱动（由 M4 `transitions.go` 调用），Adapter 翻译为 Provider-specific API 字段（`ReasoningEffort string` + `*ThinkingConfig`，见 `internal/llm/adapter/client.go`）：

| 档位 | 触发条件 | DeepSeek V4 Pro 映射 | Claude 映射 |
|------|---------|----------------------|-------------|
| `ThinkingDisabled` | SI < 0.3 且 replanCount=0 且 TaintLevel < 3 | 无 thinking 字段 | 无 thinking 字段 |
| `ThinkingHigh` | 0.3 ≤ SI < 0.6 | `reasoning_effort="high"` + `thinking.type="enabled"` | `thinking.budget_tokens=4096` |
| `ThinkingMax` | SI ≥ 0.6 或 replanCount > 0 或 TaintLevel ≥ 3 | `reasoning_effort="max"` + `thinking.type="enabled"` | `thinking.budget_tokens=16384` |

> **约束**：
> - DeepSeek V4 Pro thinking 启用时温度强制为 0（API 要求）
> - 多轮工具调用序列中，`reasoning_content` 必须随 assistant 消息回传至下一轮 prompt——Adapter 负责从响应中提取并写入 `ProviderResponse.ReasoningContent`；M4 通过 `StateContext.LastReasoningContent` 跨轮持有
> - HT0 下 `ThinkingMax` 可正常运行（DeepSeek V4 Pro token 成本低，无预算门控）

**`[ReasoningTokens]`** 计量：`InferResponse.Usage` 分字段 `reasoning_tokens`，计入 [TokenBurnRate]；M3 导出 `polaris_reasoning_tokens_total` Gauge。

### 5.3 StreamBudgetGuard

实现见 `internal/llm/`（`StreamBudgetGuard` + `TokenBurnDetector`）。

GuardChunk 摊销检查（每 100 chunk 或首 chunk）：
- **L1**：剩余预算 <= 0 → WARN（不阻断）
- **L2**：TokenBurnDetector 加速度检测（5s 窗口，3 采样点，accel > 3× baseline → BurnAlert → FatalStreamAbort 硬阻断）
- **L3**：预算耗尽 → 硬阻断

TokenBurnDetector 仅做单流加速度检测，系统级燃烧速率从 M3 `polaris_token_burn_rate` Gauge 单源读取。

### 5.4 trackStreamCost

实现见 `internal/llm/`（`StreamGuard` - 流成本追踪）。

流结束时的处理策略：
- **正常结束**：记录精确 API usage。
- **流中断（FatalStreamAbort）**：丢弃输出 → M4 `S_REPLAN`（禁止 JSONRepair——失控 LLM 截断输出语义不可靠）。
- **超过最大缓冲区（256KB）**：写入 workspace 临时文件 + 返回 `ErrResponseTooLarge`。
- **正常中断**：执行 JSONRepair + 双重安全校验。

### 5.5 JSONRepair

实现见 `internal/llm/`（`StreamGuard` - JSONRepair）。

采用栈式括号匹配 → 自动闭合 → 移除不完整 key-value。确定性 Go 实现，延迟 <1ms。

**双重安全校验**：
1. `required` 字段完整性检查。
2. 如果 `SideEffects > read_only` 且 `DiscardedKeys` 非空 → 强制拒绝 → 触发 `S_REPLAN`。

---

## 6. Semantic Cache

### 6.1 EmbeddingBatcher & OpenAICompatibleAdapter

实现见 `internal/llm/`（`EmbeddingBatcher`, `OpenAICompatibleEmbeddingAdapter`）。支持通过 OpenAI `/v1/embeddings` 标准接口接入云端/端侧的各种 Semantic Embedding 服务（Tier 2 Embedding）。

**双优先级队列**：
- `pendingHigh[180]`：SurpriseIndex、交互式查询
- `pendingLow[76]`：GraphRAG、Consolidation

配置参数：`batchWindow=10ms`，`maxBatchSize=100`，保留 20% 槽位给 Low 队列防饥饿。Low 队列等待 >100ms 自动升为 High。

**背压策略**：
- High 队列容量达 80% → 指数退避（50ms 初始，max 2s）
- Low 队列容量达 80% → 强制排队 30ms

**文本去重（dedup）**：相同文本重复入队时，仅占用一个队列槽位，额外等待者追加到扇出列表。API 调用返回后，结果同步广播给所有等待同一文本的调用方。消除并发场景下对相同文本的重复 Embedding API 调用。

文本在发往远程 API 前，必须经过 [PIIGuard]（PII（Personally Identifiable Information，个人可识别信息）：Personally Identifiable Information，个人可识别信息）红化预处理。

### 6.2 SemanticCache

实现见 `internal/store/search/semantic_cache.go`（`SemanticCache`，含 `CacheStore` 接口和 LRU 淘汰逻辑）。

**`[2026-07-21 订正]`**：本节此前写的"向量索引后端待激活/store=nil 时安全空操作"已过期——
`internal/store/search/surreal_cache_store.go` 的 `SurrealCacheStore`（SurrealDB-Core HNSW）
是完整实现，`cmd/polaris/boot_substrate.go` 在 `surrealStore != nil` 时已构造真实
`cacheStore` 并通过 `llm.WithSemanticCache` 注入 `InferenceRouter`；`store=nil` 只在
SurrealDB 未启用时才发生，属正常降级，不是"待激活"状态。

**真正未接入的缺口**：`types.WithSemanticCacheHints(...)`（构造 `ContextHintFingerprint`/
`ActiveControlLabels`/`TaskType`）全仓库生产侧零调用点——`InferenceRouter.Infer`（非流式）
的 Get/Put 逻辑因此永远不会真正执行；且该逻辑只存在于 `Infer`，`StreamInfer`（主对话轮
实际走的路径，`router_stream.go`）完全没有等价的缓存查询/写入分支，即使补齐
`WithSemanticCacheHints` 调用点，主对话仍不会被缓存命中。`ContextHintFingerprint` 所需的
SHA-256 指纹其实已经在 `internal/agent/fsm/epoch.go`（`epochTracker.check`）里逐条计算，
但只返回 epoch 计数器、丢弃了指纹字符串本身，`sCtx.ContextEpoch` 写入后也无任何读取方——
是同一个"生产者/消费者都存在、中间没有真正接线"的模式，接入需要给 `StreamInfer` 补一条
对称的缓存命中/写入分支（含如何在流式通道里返回一次性缓存内容），非一行接线。

**缓存三重匹配**：RequestHash + Namespace + SystemPromptHash。
`hashRequest` 计算公式：`SHA-256(Namespace + SystemPromptHash + ContextHint.Fingerprint + ActiveControlVectorLabels + TaskType + MessageContents)`。

缓存满时执行 LRU 淘汰（批量淘汰 MaxEntries/10）。

SimilarityThreshold、MaxEntries、TTL 等参数详见 `spec/state.yaml` 中的 `§m1_router.semantic_cache_similarity_threshold` / `semantic_cache_max_entries` / `semantic_cache_ttl_hours`。

---

## 7. Fallback Chain

### 7.1 三级 Fallback

Primary → Secondary（同级备选）→ Tertiary（降级备选）→ GracefulDegradation → [ESCALATE]

### 7.2 失败响应

| 失败模式 | HTTP | 策略 |
|---------|------|------|
| Rate Limit | 429 | Exponential backoff + 换 provider |
| Server Error | 5xx | 立即换 provider + 冷却原 provider |
| Timeout | — | 减少 MaxToken 后重试 |
| Content Filter | 400 | 不重试 |
| Token Limit | 400 | 压缩 context 后重试 |

### 7.3 CircuitBreaker + FallbackExecutor

CircuitBreaker 三态：
- Closed（正常）
- Open（熔断，冷却期拒绝请求）
- HalfOpen（探测）
- 恢复至 Closed

`failureThreshold` 次连续失败触发 Open，冷却 `cooldownPeriod` 后进入 HalfOpen，探测成功恢复 Closed。参数见 `spec/state.yaml §m1_router.circuit_breaker_*`。

FallbackExecutor（`internal/llm/`）：仅检查 Provider 可用性并更新 CircuitBreaker 状态，**不执行实际推理**。全部不可用时返回 `ErrAllProvidersFailed`。

实际推理 failover 由 InferenceRouter 在调用链内处理；两者职责不同，前者负责健康判断，后者负责推理重试。

**StreamInfer Failover**：
`StreamInfer` 与 `Infer` 同等纳入 CircuitBreaker 覆盖。每次 `StreamInfer` 调用记录延迟并更新 HealthScorer，若 Provider 返回错误则触发 Failover 切换至下一可用 Provider（逻辑与 Infer 路径一致）。
流式错误中断时 EventLog 追加 `streaming_interrupted: true` 字段（inv_M1_04）。

**Provider 恢复事件**：
当所有 Provider 均处于 Open 状态（`ErrAllProvidersFailed` 已触发）后，任意一个 Provider 的 CircuitBreaker 完成 HalfOpen→Closed 转换（半开探测成功）时，M1 向 M2 Outbox 写入：

```text
MutationIntent{
    Table: "outbox",
    Op: OpInsert,
    Payload: {
        "target_engine": "m4_provider_recovery",
        "provider_id": <providerID>,
        "recovered_at": <timestamp>
    }
}
```

此事件经 M2 Outbox Worker 投递至 M4（M4 §8 Provider 恢复唤醒路径），M4 据此自动唤醒处于 `Suspended(suspend_reason=provider_exhausted)` 的任务。多 Provider 场景下，若多个 Provider 同时恢复，事件幂等合并（M4 Outbox Worker 在同一 batch 内去重 `task_id`，避免重复唤醒）。

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

隐私/离线备选。不参与 Provider Router 主路由，仅作为 Fallback 最后一级。`llama.cpp` 统一负责 Embedding + LLM 推理 + Rerank，GGUF Q4_K_M。利用 Metal (macOS) / CUDA (Linux) 硬件加速，单 FFI 桥接点，统一 GGUF 量化生态。

模型加载策略：

| 模型 | 用途 | 大小 | 加载条件 |
|------|------|------|---------|
| Qwen3-32B-Q4_K_M | LLM 推理 | ~20GB | Tier 3+ (64GB) local_only 专享 |
| bge-reranker-base-Q4_K_M | Cross-encoder 重排 | ~50MB | 懒加载，首次 rerank 请求时 |
| BGE-small-Q4_K_M | 本地 Embedding | ~100MB | 见下方 [BGE-small 补充说明](#bge-small-补充说明) |

#### BGE-small 补充说明
用于 `local_only` 或隐私 embedding 模式。输出 384-dim，通过 SurrealDB-Core 双索引隔离表（`index_local_384`）维持语义检索能力，避免永久降级 BM25。详见 M10 §2 双索引方案。

### 8.1 LocalProvider

**当前实现状态：已实现（P3-1，2026-07-03）。**
`protocol.LocalProvider`（`internal/protocol/interfaces.go`）扩展 `protocol.Provider`，新增 `LoadModel` / `UnloadModel` / `EvictKVCache` / `LocalStatus` 四个生命周期方法。

`internal/llm/adapter/local.go` 的 `LocalAdapter` 是其唯一实现。底层通过 `internal/ffi/llama.go`（purego 懒绑定 + `recover()` 优雅降级，与 `native_sandbox`/`surreal_store`/`cedar_ffi` 同款 FFI 模式）调用 `rust/substrate/src/llama_infer/`（`llama-cpp-2` crate，`tier1` Cargo feature 门控）导出的 `llama_infer_*` 函数。

**架构偏离说明（遵循“系统最优、复用现有代码”原则）**：

- **无独立 `/rerank` HTTP 端点**：Rerank 直接走进程内 FFI 调用（`llama_infer_rerank`），而非“同进程 llama.cpp `/rerank` 端点”——省去端口管理与 HTTP 序列化开销，属于更优的同进程调用路径。Rerank 算法是双塔嵌入余弦相似度（复用 Embed 批量嵌入），而非 cross-encoder 分类头——GGUF/llama.cpp 生态缺乏统一的 cross-encoder 调用约定，双塔相似度是能用任意已加载 embedding 模型实现、无额外模型格式假设的最稳健通用方案。
- **单模型槽位、无 Blue-Green 双模型常驻**：与热切换语义（见 §8.2）对齐，Tier-1/Tier-3 硬件显存/内存有限，多模型同时常驻不具备性价比。
- **不与具体模型型号（Qwen3-32B/bge-reranker-base/BGE-small）绑定**：`LoadModel(modelPath, opts)` 接受任意 GGUF 文件路径，模型选型是调用方（配置/CLI）职责，Provider 层保持模型无关。
- **StreamInfer 非逐 token 流式**：`llama_infer_generate` FFI 协议是 run-to-completion（Rust 侧一次性返回完整文本），`StreamInfer` 当前实现为“整体生成后单帧下发”到 `<-chan types.StreamEvent`。真正的逐 token SSE 需要 Rust 侧改为回调/迭代器式 FFI，记录为已知后续优化点，不影响 `protocol.Provider` 接口契约的满足。

**KV Cache 管理**：
`LocalAdapter` 底层持有一个跨请求复用的 `LlamaContext`（避免每次生成重新分配 KV cache 缓冲区），但不做跨请求增量前缀复用，`generate()` 开头总是显式 `clear_kv_cache()` 保证正确性。`EvictKVCache`（对应 `protocol.LocalProvider.EvictKVCache`）作为独立操作暴露，供会话切换/内存回收场景主动触发。

**GBNF grammar 结构化输出**：
`types.InferOptions.ResponseFormat.Type == "gbnf"` 时，`Grammar` 字段透传给 Rust 侧 `LlamaSampler::grammar()`，在采样阶段强约束输出符合给定语法（`llama-cpp-2` crate 的 `common` feature，默认已启用）。

**硬件门控**：
`FeatureLocalInference`（`internal/observability/probe/feature_gate.go`，`MinTier: Tier1, MinMemoryMB: 2048`）之外，额外叠加 `ffi.LlamaAvailable()` 判断——区分“硬件满足 Tier-1”与“二进制是否以 `--features tier1` 编译”两个独立维度，二进制未编译时不注册一个必然失败的 Provider（`cmd/polaris/boot_substrate.go`）。

**功能对照**：
`internal/llm/adapter/ollama.go`（HTTP 接入本机 Ollama 服务）与 `LocalAdapter`（同进程 FFI）两条本地推理路径并存、不互斥，分别注册为 `ollama-local` / `llama-local`，由 Router 按健康度/延迟自动择优。Ollama 路径部署简单（依赖外部 Ollama 服务），FFI 路径部署更紧凑（单二进制、无额外进程），二者互为备份。

### 8.2 生命周期

- **懒加载**：`LocalAdapter` 构造时不加载任何模型（`ModelID()` 返回 `local:unloaded`），`LoadModel` 由调用方显式触发。
- **热切换**：`LoadModel` 覆盖式替换——若已有模型常驻，先完整 `unload`（释放 `LlamaContext` 与模型内存）再加载新模型（单槽位，`rust/substrate/src/llama_infer/mod.rs` `ModelHolder`）。
- **空闲卸载 / Tier 降级**（OSMemoryGuard 空闲内存 < 1.0GB 强制卸载）：
  Go API（`UnloadModel`）已具备。
  `cmd/polaris/boot_substrate.go` 已启动 `autoConf.RunMemoryWatcher`（5s 轮询）驱动 `internal/observability/auto_config.go` 的 `MemoryPressureCallback`。
  `DegradationCritical`（< 1GB）时会执行 `Gate.Override(FeatureLocalInference, Disabled)` + GC + 终止沙箱，作为过渡性降级措施先阻止新的本地推理请求。但该回调尚未直接调用 `UnloadModel` 主动释放已加载模型内存，“OSMemoryGuard 自动触发 UnloadModel”这一直接对接仍待后续完成。
- **命令面**：`protocol.LocalProvider` 的生命周期方法已在 Go API 层完整可调用（通过 `llm.ProviderRegistry.Get("llama-local")` 取出后类型断言），面向用户的 `/model local <model_id>` 命令解析/HTTP 端点属于命令面对接工作，不在本次 FFI 核心交付范围内。

---

## 9. ModelVersionRegistry

`ModelVersionEntry` 持有 Provider/ModelID/版本/废弃状态、PromptTemplate/ToolCallStyle/MaxContext/Capabilities 等元数据，以及 `ValidatedOn`（技能兼容测试通过列表）和 `CompatibilityScore`（0-1）。

`OnModelUpgrade` 流程：对比行为差异 → 重跑关联技能兼容测试 → 更新 ValidatedOn 与 CompatibilityScore，低于 0.8 时发 WARN。废弃迁移按 CompatibilityScore 分三档（≥0.9 自动 / 0.7-0.9 自动+WARN / <0.7 禁止自动）；连续 3 次 4xx/5xx 自动回退。Embedding 模型废弃时触发 M2 OnlineReindexer 全量重嵌。

**当前实现状态：已实现（P3-2，2026-07-03）。**
DDL SSoT（Single Source of Truth，唯一权威源）：`internal/protocol/schema/033_model_version_registry.sql`（`model_version_entries` 表）。三层结构：

- `internal/protocol/repo.ModelVersionEntry` / `ModelVersionRepository`（接口契约，字段与 DDL（Data Definition Language，数据定义语言）一一对应）
- `internal/store/repo.SQLiteModelVersionRepository`（SQLite 实现，`Get`/`List`/`ListDeprecated`/`FindPredecessor`/`Upsert`/`Delete`）
- `internal/llm/modelregistry.Registry`（业务逻辑层）：
  - `DecideMigration(score)` 三档判定（纯函数，阈值与本节一致）
  - `OnModelUpgrade(ctx, provider, modelID, skillNames, tester)` —— `tester` 为 `SkillCompatTester` 接口（consumer-side，当前无生产实现时可传 `nil`，仅刷新元数据不重测）；score < 0.8 时 `slog.Warn`
  - `RecordCallResult(ctx, provider, modelID, success)` —— 连续失败计数，达到 3 次返回 `shouldRollback=true` + 通过 `FindPredecessor`（查询“谁把 successor_model_id 指向当前模型”）定位的回退目标模型 ID；成功调用清零计数
  - `DeprecateModel(ctx, provider, modelID, successorModelID)` —— 标记废弃 + 记录继任模型；`Capabilities.embedding=true` 时唤醒注入的 `ReindexTrigger` 回调

**架构偏离说明（遵循“系统最优、复用现有代码”原则）**：

文档原描述“触发 M2 OnlineReindexer 全量重嵌（影子表 → Blue-Green swap）”，实际未新建影子表或 Blue-Green 切换机制。

`internal/memory/retrieval/online_reindexer.go` 的 `OnlineReindexer` 本身已是版本差异驱动（比较 `episodic_events.embed_model_version` 与当前 `Embedder.ModelVersion()`）。Embedding 模型切换后，下一次 `Run()` 就会自然重嵌所有旧版本行，Blue-Green 影子表是不必要的重复设计。

`DeprecateModel` 的 `ReindexTrigger` 回调只解决“尽快”语义（`cmd/polaris/boot_memory.go` 中 `startOnlineReindexer` 返回一个可立即调用的 `onlineReindexer.Run` 闭包，取代默认 5min 周期等待），不传时不影响正确性。

**resolveXXXModel() 数据迁移**：

`internal/llm/adapter/{anthropic,openai,deepseek}.go` 的 `resolveXXXModel()` 函数**未被删除或改动**（dev prompt 显式要求保留为无数据库依赖的编译期兜底路径）。

`Registry.SeedFromStaticResolvers(ctx)` 将这些函数内硬编码的废弃名映射（如 `claude-3-opus-20240229 → claude-3-5-sonnet-latest`、`gpt-4 → gpt-4o-mini`、`deepseek-chat → deepseek-v4-flash` 等 10 条）幂等灌入 registry 表（`CompatibilityScore=1.0`，视为已长期生产验证），供需要 DB 侧结构化查询兼容性评分/继任模型的调用方使用。

已存在条目不覆盖，避免抹掉运营中产生的真实评分数据。启动时由 `bootMemory`（`cmd/polaris/boot_memory.go`）自动调用一次。

测试覆盖：`internal/store/repo/repo_modelversion_test.go`（Repository CRUD）、`internal/llm/modelregistry/registry_test.go`（三档迁移判定、兼容性评分更新、Embedding 废弃触发重嵌、连续失败回退、静态映射幂等灌入）。

---

## 12. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| 单 Provider 限流/不可用 | EWMA p95 > 阈值 / 5xx 连续 | 仅本 Provider | Fallback：同 Pool → 降级 Pool → 拒绝 | 半开探测 | 同 Pool 全断 → audit severity=warn |
| 全部 Provider 熔断 | CircuitBreaker 全开 | 全模块 LLM 路径 | 返回 `ErrAllProvidersFailed` | 冷却期满后半开探测 | 持续 > 5min → audit |
| SemanticCache 满 (10000 entries) | 内存水位触发 | 仅缓存查询 | LRU 淘汰 MaxEntries/10 条目 | 自动 | — |
| StreamBudgetGuard L2 触发 | TokenBurnDetector 加速度检测 (5s 窗口, accel > 3× baseline) | 单流 | FatalStreamAbort 硬阻断 → M4 `S_REPLAN` | TokenBurnRate 恢复正常后自动解除 | 单 session 反复触发 ≥3 次 → audit |
| Tier-3 本地模型加载失败 | llama.LoadModelFromFile err / RSS 检测 | local_only 全模块 LLM 路径 | 降级远程 API（非 local_only）；local_only → ErrLocalModelUnavailable | 空闲内存恢复后重新加载。local_only 模式下若持续 > 30s 无法重载 → 触发 M13 ResourceGovernor local_only 死锁恢复 | local_only 模式下死锁恢复仍失败 → 必须 HITL（Human-In-The-Loop，人在回路） |
| Embedding API 不可用 | err return / timeout | 检索路径 | EmbeddingBatcher 返回错误，调用方降级 BM25/FTS5 | API 恢复后自动切回 | 全断 > 1h → audit |
| EmbeddingBatcher High 队列 >80% | 队列水位 | 非交互 embedding 请求 | 指数退避 (50ms→2s) | 队列水位下降后恢复正常 | 持续 > 5min → audit |
| ModelVersionRegistry 废弃迁移失败 | 连续 3 次 4xx/5xx | 单模型 | 保持旧模型 + WARN + 禁止自动切换 | 下一检测周期重试 | 版本 EOL 日迫近 |

与 OSMemoryGuard 协同：(仅 Tier-3) 空闲内存 < 1.0GB → 强制卸载本地模型；L3 临界 → 全部 LLM 调用路由至远程 API。

---

## 10. 凭证池与速率限制追踪（已实现）

以下三个组件均位于 `internal/llm/`：

**CredentialPool**：多 API Key 线程安全池，支持四种选择策略（FillFirst / RoundRobin / Random / LeastUsed）。按策略选取冷却期已过的凭证，失败后按错误类型设置冷却期（Auth 5min / Billing+RateLimit 60min / AuthPermanent 30天）。

**RateLimitTracker**：解析 Provider HTTP 响应头中的 12 个速率限制字段（分钟/小时 × 请求/Token），提供退避延迟建议。以 RoundTripper 包装层自动捕获限速头，无需改动各 Adapter。退避算法采用去相关抖动指数策略（防雷群效应）。

**ErrorClassifier**：提取 HTTP 状态码 + 关键词，分类为 17 种失败原因，输出包含可重试性、上下文压缩、凭证轮换、降级策略在内的恢复提示。覆盖 Anthropic/OpenAI/DeepSeek/Gemini/Ollama/阿里云/火山引擎等多 Provider 错误体格式。

---

## 默认参数

完整阈值与重评触发条件：`spec/state.yaml §thresholds.m1_router`。

---

## 13. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M2 Storage | SemanticCache 的 SurrealDB-Core 后端存储 | M2 §1.3 |
| M3 Observability | TokenBurnRate CANONICAL SOURCE（M3 单源持有）、SurpriseIndex consumer | M3 §3, §4 |
| M4 Agent Kernel | Provider.Infer/StreamInfer 消费者（LLM 调用唯一入口）| M4 §10 |
| M9 Self-Improve | PromptOptimizer 使用 LLM 调用（经 Provider 路由）| M9 §1.1 |
| M11 Policy Safety | SafeDialer 网络出口、Taint 门控 | M11 §5.2, §6 |
| 接口定义 | Provider/InferRequest/InferResponse/StreamEvent | internal/protocol/interfaces.go, types.go |
| 全局字典 | TokenBurnRate/SurpriseIndex 完整定义 | 00-Global-Dictionary §3 |
