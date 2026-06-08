# 模块 1: Inference Runtime

> API 优先架构。Provider Router 为核心。本地推理仅隐私/离线备选。
> Go 实现 | [HE-Rule-1] [HE-Rule-2] [HE-Rule-3] [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
> [Module-Topology] [Code-Package-Mapping] [Tier-0-Limit] [Tier-1-Limit]
> **§跳读**: 0:10 职责 / 0-ter:24 不变量速查 / 1:37 默认模型 / 2:43 Provider接口 / 3:49 Adapter / 4:64 Router / 5:129 Token预算 / 6:218 SemanticCache / 7:238 Fallback / 8:281 本地推理local_only / 9:338 ModelVersion / 12:364 349(SOFT)降级 / 13:384 依赖

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

### 4.2 五个 Model Pool

| Pool | 候选模型示例 | 默认用途 |
|------|------------|---------|
| Budget | `<flash-class>`（DeepSeek V4 Flash / Gemini Flash / Qwen-Turbo 等）| 默认主路径（分类、摘要、路由判断、简单工具）|
| Standard | `<standard-class>`（Claude Sonnet 4.6 / GPT-5.x / Gemini Pro 等）| 代码生成、多步推理、复杂工具编排 |
| Reasoning | `<reasoning-class>`（Claude Opus 4.6 / DeepSeek V4 Pro / o-系列等）| 复杂架构决策、长链推理、自反思 |
| Local-SLM | 3-8B GGUF 本地 | local_only / 离线 fallback |
| Local-Reasoning | 14B+ 本地 | HT2+ local_only |

> **默认推荐**：`configs/defaults.toml` 选 DeepSeek V4 Flash + Pro 组合（价格 / 质量 / 中文友好综合最优，Tier-0 长程验证）。用户可在 Web UI Settings 切换至任何兼容 Provider。

### 4.3 多模态请求预处理

`InferenceRouter.normalizeInferRequest`（`pkg/substrate/inference/media_opt.go`）在 `Infer`/`StreamInfer` 入口统一执行，覆盖所有调用路径（Gateway、Kernel、MCP 工具结果、Swarm）：

- 图片降采样：长边 > 1568px 等比缩放，满足主流 Vision Provider token 预算上限
- 格式归一：PNG/GIF 转 JPEG（quality=85），减少传输体积
- 不修改文本内容、工具定义、路由参数，对非图片 Part 零开销

调用方无需手动处理；MCP 工具返回的 `protocol.ImagePart` 由此路径自动压缩。

### 4.4 ComplexityDeterminer

实现见 `pkg/substrate/inference/router.go:InferenceRouter`。

- outputEstimate: EMA α=0.3, window=100；冷启动默认 1024(simple) / 4096(code/research)
- ToolCount > 5 OR outputEstimate > 4096 → Reasoning Pool
- ToolCount > 1 OR outputEstimate > 1024 → Standard Pool
- ELSE → Budget Pool

### 4.4 Route 方法

实现见 `pkg/substrate/inference/router.go:Route()`。
Provider 选择按 priorityOrder 遍历: tokenizer 预计算 token 数 → 成本估算 → budgetGate 准许 → quota 可用 → 返回 provider。全部不可用 → `ErrAllProvidersExhausted`。

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

### 5.2-bis Test-Time Compute — `[ReasoningEffort]` `[BestOfN]` `[SelfConsistency]`

> 对齐 2025-2026 推理模型范式 (o3 / DeepSeek R / Claude thinking)。与 [System-1/1.5/2] **正交**，详见 [00-Global-Dictionary §9-ter]。

**[ReasoningEffort]** — Provider 接口一等公民字段:
```
InferRequest.ReasoningEffort ∈ {low, medium, high}
```
Adapter 映射:
- OpenAI/o-系列 → `reasoning_effort`
- DeepSeek R → `reasoning_budget` (low=2k / med=8k / high=32k)
- Claude → `thinking.budget_tokens` (low=1k / med=4k / high=16k)
- Local-SLM → ignored（不支持）
- Local-Reasoning (HT2+) → 内部 `MaxThinkingTokens` 映射

ReasoningEffort 默认: System 1.5 → low; System 2 → medium; 用户显式 + Cedar permit → high。

**`[ReasoningTokens]`** 计量:
- InferResponse.Usage 分字段: `prompt_tokens` / `completion_tokens` / `reasoning_tokens`
- 三者均计入 [TokenBurnRate]（同等权重）
- M3 独立导出 `polaris_reasoning_tokens_total` Gauge

**`[BestOfN]` + `[SelfConsistency]`** — M1 ParallelSampler:
```
ParallelSample(req, N) → ([]InferResponse, error)
  1. CapGate: Cedar permit context.task_priority >= 1 (低优任务拒绝)
  2. 预算检查: estimated_cost × N <= remaining_session_budget (否则降级 N=1)
  3. N 路 goroutine 并发调 Provider, temperature 各异 (0.3/0.5/0.7 默认)
  4. ctx cancel 传播 (UserInterrupt / KillSwitch 立即终止全部)
  5. 聚合:
     - 结构化输出 → SelfConsistency.MajorityVote (相同 schema 多数投票)
     - 自由文本 → BestOfN.Verifier (M11 FactualityGuard 抽样打分 → 取最高)
  6. 返回最终 + 全部 reasoning_tokens 计入 burn rate
```

**HT0 默认配置**: N=1（关闭）。`FeatureGate.FeatureTestTimeCompute` 检查（≥Tier1 + 任务 priority>=1 + remaining_budget 充足）→ 启用。

**Cedar 策略** (M11 §3.1 增补):
```
permit call_tool when resource.action == "parallel_sample" AND
  context.task_priority >= 1 AND context.tier >= 1 AND
  context.estimated_cost <= context.remaining_budget * 0.3
```

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

`[接口预留][实现依赖 SurrealDB-Core HNSW，当前版本未激活]` 类型定义见 `pkg/substrate/semantic_cache.go`（SemanticCache struct / CacheStore 接口 / Embedder 接口 / CacheEntry struct）。当前无 Get() / Put() / LookupSimilar() / TTL 淘汰方法实现，不参与推理路由。

缓存设计意图（SurrealDB-Core HNSW 可用后实现）: SimilarityThreshold / MaxEntries / TTL 见 `spec/state.yaml §m1_router.semantic_cache_similarity_threshold` / `semantic_cache_max_entries` / `semantic_cache_ttl_hours`。三重匹配: RequestHash + Namespace + SystemPromptHash。hashRequest: SHA-256(Namespace + SystemPromptHash + ContextHint.Fingerprint + ActiveControlVectorLabels + TaskType + MessageContents)。满时 LRU 淘汰 MaxEntries/10。

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
| Qwen3-3B-Q4_K_M | LLM 推理 | ~2GB | local_only / 离线 |
| Qwen3-8B-Q4_K_M | LLM 推理 | ~5GB | Tier 1+ local_only |
| bge-reranker-base-Q4_K_M | Cross-encoder 重排 | ~50MB | 懒加载，首次 rerank 请求时 |
| BGE-small-Q4_K_M | 本地 Embedding | ~100MB | local_only 或隐私 embedding 模式。输出 384-dim，通过 SurrealDB-Core 双索引隔离表（index_local_384）维持语义检索能力，避免永久降级 BM25。详见 M10 §2 双索引方案 |

### 8.1 LocalProvider

```
NewLocalProvider(config):
  1. SHA-256 校验模型文件
  2. AvailableRAM >= MinRAMBytes + 512MiB
  3. llama.LoadModelFromFile → llama.NewContext
  4. return LocalProvider

Infer(ctx, req):
  1. 构建 prompt (chat template + tools)
  2. context.Completion(prompt, CompletionParams{MaxTokens, Temperature, StopTokens})
  3. 解析输出

InferWithSchema(ctx, req, schema):
  1. grammar = schemaToGBNF(schema)
  2. context.SetGrammar(grammar) → Infer → ClearGrammar()

StreamInfer(ctx, req):
  1. ch = make(chan StreamEvent, 64)
  2. goroutine: context.StreamCompletion → ch ← StreamEvent → close(ch)

Rerank(ctx, query, documents[]):
  → llama.cpp /rerank endpoint (内嵌 server 模式, 同进程 FFI)
  → 返回 [{index, score}] 按分降序
  → 模型: bge-reranker-base.gguf (~50MB), 懒加载
  → 50 文档交叉编码 <50ms (CPU)

EvictKVCache(): context.ClearKVCache()
  时机: Control Vector 变更 / 模型热切换 / Session 重置

Tokenizer(): LlamaCppTokenizer{model}
Capabilities(): Streaming=true, Tools≈70-80%, Thinking=false, Cost=0
```

### 8.2 生命周期

- 懒加载: 首次请求时加载，非 `local_only` 模式默认不加载
- 空闲卸载: `local_only` 模式下 30min 无请求卸载
- Tier 降级: OSMemoryGuard 检测空闲内存 < 1.0GB → 强制卸载
- 热切换: `/model local <model_id>` 卸载当前 → 加载新模型

---

## 9. ModelVersionRegistry

```
ModelVersionEntry:
  Provider, ModelID, Version, Deprecated
  PromptTemplate, OutputFormats, ToolCallStyle
  MaxContext, Capabilities
  Supersedes, ValidatedOn, BreakingChanges
  LastVerifiedAt, CompatibilityScore (0-1)

OnModelUpgrade(ctx, oldVer, newVer):
  1. diffs = diffBehavior(oldVer, newVer)
  2. FOR each skillID IN oldVer.ValidatedOn → runSkillCompatTest(ctx, skillID, newVer)
  3. newVer.ValidatedOn = passingSkills
  4. newVer.CompatibilityScore = computeScore(diffs)
  5. IF CompatibilityScore < 0.8 → WARN

废弃自动迁移:
  1. 检测 X-Deprecation-Date / Sunset header
  2. 查 upgrade_path → 同系列自动升级; 否则查 model_capability_matrix (capability vec 余弦相似度)
  3. CompatibilityScore ≥ 0.9 → 自动切换; 0.7-0.9 → 自动 + WARN + 人工确认; < 0.7 → CRITICAL 禁止自动
  4. 通知 M6 Skill Library 标记 needs_adaptation (P1)
  5. 自动切换后连续 3 次 4xx/5xx → 回退 + CRITICAL 告警

**Embedding 模型废弃迁移**: ModelVersionRegistry 覆盖 EmbeddingModel。当 Embedding 模型 Sunset 时，触发 M2 OnlineReindexer 同时迁移所有依赖该模型的向量索引（M5 全部 episodic_events/semantic_entities、M10 全部 doc_nodes/leaf_chunks/summaries、M6 SkillIndex、M1 SemanticCache）。迁移流程: EmbeddingModel.Deprecated → M2 OnlineReindexer 创建影子表 → 全量重嵌 → Blue-Green swap。
```
## 12. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| 单 Provider 限流/不可用 | EWMA p95 > 阈值 / 5xx 连续 | 仅本 Provider | Fallback：同 Pool → 降级 Pool → 拒绝 | 半开探测 | 同 Pool 全断 → audit severity=warn |
| 全部 Provider 熔断 | CircuitBreaker 全开 | 全模块 LLM 路径 | 返回 ErrAllProvidersExhausted | 冷却期满后半开探测 | 持续 > 5min → audit |
| SemanticCache 满 (10000 entries) | 内存水位触发 | 仅缓存查询 | LRU 淘汰 MaxEntries/10 条目 | 自动 | — |
| StreamBudgetGuard L2 触发 | TokenBurnDetector 加速度检测 (5s 窗口, accel > 3× baseline) | 单流 | FatalStreamAbort 硬阻断 → M4 S_REPLAN | TokenBurnRate 恢复正常后自动解除 | 单 session 反复触发 ≥3 次 → audit |
| 本地模型加载失败（OOM/文件损坏） | llama.LoadModelFromFile err / RSS 检测 | local_only 全模块 LLM 路径 | 降级远程 API（非 local_only）；local_only → ErrLocalModelUnavailable | 空闲内存恢复后重新加载。local_only 模式下若持续 > 30s 无法重载 → 触发 M13 ResourceGovernor local_only 死锁恢复（M13 §2.0）：强制 Rollback Priority >= 2 的非核心任务腾出内存，重试 LLM 重载 | local_only 模式下死锁恢复仍失败 → 必须 HITL |
| Embedding API 不可用 | err return / timeout | 检索路径 | EmbeddingBatcher 返回错误，调用方降级 BM25/FTS5 | API 恢复后自动切回 | 全断 > 1h → audit |
| EmbeddingBatcher High 队列 >80% | 队列水位 | 非交互 embedding 请求 | 指数退避 (50ms→2s) | 队列水位下降后恢复正常 | 持续 > 5min → audit |
| ModelVersionRegistry 废弃迁移失败 | 连续 3 次 4xx/5xx | 单模型 | 保持旧模型 + WARN + 禁止自动切换 | 下一检测周期重试 | 版本 EOL 日迫近 |

与 OSMemoryGuard 协同: 空闲内存 < 1.0GB → 强制卸载本地模型；L3 临界 → 全部 LLM 调用路由至远程 API。


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
