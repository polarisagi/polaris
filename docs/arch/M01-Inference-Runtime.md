# 模块 1：Inference Runtime —— 推理运行时

> **定位**：LLM 推理统一入口；API 优先，Provider Router 为核心；本地推理默认关闭，仅显式启用（ADR-0109）。
>
> **语言**：Go　|　**代码**：`internal/llm/`
>
> **相关约束**：[HE-Rule-1]、[HE-Rule-2]、[HE-Rule-3]、[HE-Rule-4]、[HE-Rule-5]、[HE-Rule-6]、[Module-Topology]、[Code-Package-Mapping]、[Tier-0-Limit]、[Tier-1-Limit]
<!-- §跳读: 0:12 职责 / 0-ter:25 不变量速查 / 1:40 默认模型 / 2:46 Provider接口 / 3:52 Adapter / 4:64 Router / 4.4:98 ComplexityDeterminer / 4.5:107 Route方法 / 5:121 Token预算 / 6:182 SemanticCache / 7:204 Fallback / 8:234 363 本地推理local_only / 9:277 416 ModelVersion / 12:292 453 (SOFT)降级 / 10:309 470 凭证池+速率追踪 / 13:325 490 依赖 -->

---

## 0. 职责边界

| M1 **是** | M1 **不是** |
|-----------|-------------|
| LLM 推理统一入口、Provider 路由与适配 | 会话管理器（M4）、Prompt 构建器（M4 PromptFn） |
| 结构化输出强制（JSON Schema + GBNF） | 业务逻辑决策者、预算策略制定者（M11） |
| Token 计数与流式成本追踪 | 模型训练（M9）、推理质量评估（M12） |
| 流式 SSE 帧归一化（Anthropic/OpenAI/DeepSeek → 统一 StreamEvent） | 业务侧多媒体采集（M13 Gateway） |
| 多模态预处理（`media_opt`，`Infer`/`StreamInfer` 入口统一执行） | — |
| 向量化引擎选择与进程内 ONNX 嵌入器（`internal/llm/embedonnx`，ADR-0109）；语音 STT/TTS 引擎（`internal/llm/stt`、`tts`、`audiorun`，ADR-0110） | — |

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

> 进入此模块前必读：`docs/specs/09-LLM-Agent-Production.md`（A-01/A-04/A-05 + P-1/P-2/P-7：LLM 超时保护 + Failover 全量遍历）。

---

## 1. 默认模型

Provider-agnostic。`configs/defaults.toml [inference]` 推荐 DeepSeek V4 系列（default=`deepseek-flash`，reasoning=`deepseek-v4-pro`）；任何符合 §2 接口的实现均可替换。选型依据 → ADR-0020。

---

## 2. Provider Interface

接口：`internal/protocol/interfaces_llm.go`（`Provider`，含 `ModelID()` 用于系统提示词注入真实模型身份）。类型：`internal/protocol/types.go`（`InferRequest` / `InferResponse` / `StreamEvent` / `ProviderCapabilities` / `TokenizerAdapter`）。

---

## 3. Provider Adapter

Adapter 位于 `internal/llm/adapter/`：`anthropic` / `openai` / `deepseek` / `google`（Agent Platform）/ `ollama`（OpenAI 兼容协议接本机 Ollama）/ `local`（同进程 FFI，见 §8.1）。外部 SDK 仅在 Adapter 内引用。装配入口 `cmd/polaris/server_provider_loader.go`（按 provider_catalog 的 type 构造）。

每个 Adapter 内置：
- Pre-flight token count、Post-flight usage recording、结构化错误（4xx/5xx/rate limit/timeout）、指数退避 + jitter 重试。
- **KV Cache 路由（system_and_3，Anthropic）**：4 断点——system prompt（PromptCaching 启用时转 text array 注入 cache_control）/ tools 末项 / 倒数第 2 条非 system 消息 / 最后 1 条非 system 消息。3+4 缓存会话历史前缀，多轮输入成本约 -75%；与 M5 上下文组装顺序（ImmutableCore → Procedural → Episodic，stable 先于 volatile）耦合以最大化前缀命中。缓存经济学 → ADR-0105。
- **SSE 帧归一化**：Anthropic SSE（`event:` 行 + JSON）/ OpenAI SSE（`data: [DONE]`）/ DeepSeek JSON 行流 → 统一 `chan StreamEvent`（上层仅见 4 类事件）。
- **凭证安全**（inv_M1_06）：Anthropic 用 `keyInjectRT` RoundTrip 拦截器（`Header.Del("x-api-key")`）；OpenAI/DeepSeek/Google 用 `setAuthHeader` + defer cleanup（`Header.Del("Authorization")` + `clearBytes`）。

---

## 4. Provider Router

### 4.1 三层递进路由

| 层 | 机制 | 延迟 | 命中率目标 |
|----|------|------|----------|
| L1 规则路由 | task_type + token 长度，纯规则，零 LLM 零 ML | <1ms | 90% |
| L2 复杂度评估 | 启发式打分（ToolCount + outputEstimate EMA）输出 0.0–1.0，**非 ML 分类器** | ~5ms | 9% |
| L3 级联路由 | L1+L2 低置信度时调用 Budget Pool LLM 推荐 provider slot | ~50ms | <1% |

L3 输出的槽位由 `Route()` 确定性函数验证（预算门控、quota、CircuitBreaker）后采纳；LLM 填空，Go Router 持最终决策权 [HE-Rule-5]。L1/L2 严格零 LLM 调用（inv_M1_03）。

> **路由硬约束**：①90% 走 L1 ②质量优先 → 延迟 → cost tiebreaker ③仅 L3 可用 LLM ④保留 Pre-flight 成本估算 ⑤Cache 一等公民 ⑥3 级软限流（告警不阻断）⑦CircuitBreaker 必备

### 4.2 Role Pool

`ProviderRegistry`（`internal/llm/provider_registry.go`）按 `role` 分组，`BestForRole(role)` 返回最高 healthScore 的可用 Provider。

| Role | 候选示例 | 用途 |
|------|---------|------|
| `default` | flash 档（DeepSeek Flash / Claude Haiku 等） | 默认主路径（感知、直答、摘要、简单工具） |
| `reasoning` | pro 档（DeepSeek V4 Pro / Claude Opus / o 系列等） | 复杂规划、重规划、长链推理 |
| `general` | 路由层派生，不单独配置 | `poolRoles("general") = [default, general]`：先取对话模型，`role=general` 的备用模型仅兜底 |

Pool 级联降级链（`internal/llm/router.go` `poolFallbackChain`）：reasoning→general→default→budget；general→default→budget；default→budget。跨 Pool 降级沿用已失败条目跳过集（`tryPoolFallback`，`internal/llm/router_failover.go`）。catalog 只写 default/reasoning 并只补空缺（`internal/gateway/server/provider/catalog.go`）。历史：旧版 catalog 为 general 复制模型行，致降级重试同一模型（2026-09-25 订正为派生）。

本地模型不属普通设备的默认策略（ADR-0109）；`ollama` / `llama_local` 类型 Provider 未填 role 时默认 `general`，避免空 role（costTier=0）与 default 同档竞争而吞掉后台调用（ADR-0109 D6，`cmd/polaris/server_provider_loader.go`）。

### 4.3 多模态请求预处理

`media_opt`（`internal/llm/media_opt.go`）在所有推理入口统一执行（Gateway/Kernel/MCP 工具结果/Swarm）：图片长边 >1568px 等比缩放；PNG/GIF 转 JPEG(q=85)；不改文本、工具定义、路由参数；非图片 Part 零开销。MCP 返回的 `protocol.ImagePart` 自动经此压缩。

### 4.4 ComplexityDeterminer

**已删除**：`determineComplexity`（工具数 + 输出预估的 1-3 级评分）被单层 healthScore 路由 + Role Pool + ThinkingMode 三档完全覆盖，代码已清除（ADR-0020）。ThinkingMode 选的是「思考强度」而非「Provider」（§5.2-bis），与路由是并行机制，不可互相替代。

### 4.5 Route 方法

实现：`internal/llm/router.go`（`InferenceRouter`）。Provider 选择 = healthScore 降序 + CircuitBreaker 状态 + 多模态能力过滤；失败 Failover，全部不可用返回 `ErrAllProvidersFailed`。成功调用及 failover 后产出 topic `llm.call.recorded` 事件至 events 表（成本数据唯一生产者）。

**熔断器由 `ProviderRegistry` 直接持有**（无独立 `FallbackExecutor`）：
- `circuitBreaker`（`internal/llm/circuit_breaker.go`）：连续失败次数 3 态（Closed/Open/HalfOpen）。
- `windowCircuitBreaker`（`internal/llm/window_breaker.go`）：滑动窗口错误率，与前者并行生效；窗口内请求数达 `minRequests` 后按 `errorRateThreshold` 判定。

### 4.5 路由配置参数

| 参数 | 默认值 |
|------|--------|
| L1 目标命中率 | `spec/state.yaml §m1_router.l1_target_hit_rate` |
| L1 / L2 / L3 超时 | <1ms / ~5ms / ~50ms |
| CircuitBreaker 连续失败阈值 / 冷却 / 半开探测上限 | `spec/state.yaml §m1_router.circuit_breaker_failure_count` / `circuit_breaker_cooldown_seconds` / `circuit_breaker_half_open_max` |
| MaxStreamBufferSize | `spec/state.yaml §m1_router.max_stream_buffer_kb`（Tier 1+ 可配至 1MB） |

`NewProviderRegistry(cfg)` 接受 `M1RouterThresholds`，熔断参数经配置注入，TOML 热覆盖生效。

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

Token 经济（缓存稳定前缀、cheap-first、账本）→ ADR-0105。

### 5.2 Reasoning Budget Scheduling

实现：`internal/llm/stream_guard.go`（`StreamGuard`）。三模式：`fixed`（MaxReasoningSteps=5, MaxThinkingTokens=4096）/ `adaptive`（`min(16384, 4096×(1+SI×3))`）/ `batch`（32K，夜间 2-6am，非交互）。[TokenBurnRate] 处于 Stage1 THROTTLE 时降一档：`batch`→`adaptive`→`fixed`→`256`。

### 5.2-bis Test-Time Compute — `[ThinkingMode]`

对齐 DeepSeek V4 Pro / Claude thinking 原生推理；MCTS/BestOfN/多候选路由已废弃（ADR-0020 决策二）。权威定义 00-Global-Dictionary §9-ter `[ThinkingMode]`。

`SelectPlanTier(escalation, complexity, surpriseIndex)`（`internal/observability/metrics/metrics_handler.go`）同时决定规划的模型池与思考档（ADR-0105，由 M4 `internal/agent/fsm/transitions_respond.go` `planEffect` 调用）；Adapter 翻译为 Provider 字段（`ReasoningEffort` + `*ThinkingConfig`，`internal/llm/adapter/client.go`）：

| level | 模型池 | 档位 | 进入条件 |
|------|------|------|---------|
| 0 | `m4_kernel.model_pool.plan_initial`（默认 default） | `m4_kernel.thinking.plan_initial`（默认 high） | 默认 |
| 1 | 同上 | 上调一档（≤low→high，high→max） | SI ≥ 0.3；或一次能力类失败 |
| 2 | `m4_kernel.model_pool.plan_replan`（默认 reasoning） | `ThinkingHigh` | Complexity ≥ 0.7（`m4_kernel.plan.reasoning_complexity`，Perceive 由 LLM 判定）；规划模型自评 `escalate`；或累计能力类失败 |
| 3 | 同上 | `ThinkingMax` | 以上叠加 SI ≥ 0.6 或继续失败 |

其余阶段（Perceive/Reflect/Respond/L3 看门狗）由 `m4_kernel.model_pool.*` / `thinking.*` 直接配置，不走阶梯（ADR-0105）。

档位映射：`ThinkingLow` → DeepSeek `reasoning_effort="low"`；`ThinkingHigh` → `"high"` / Claude `budget_tokens=4096`；`ThinkingMax` → `"max"` / `16384`；`ThinkingDisabled` → DeepSeek `thinking.type=disabled`。

> **能力类失败**（仅此类累计 escalation，`fsm.RecordFailure`）：S_VALIDATE L0 结构错误、规划输出有内容但不可解析、第二次起的工具报错、规划模型自评超纲。安全拒绝、瞬时故障（超时/网络/限流）、观察—再规划不升级；污点不参与（旧规则下用户输入恒 TaintHigh 致每轮恒为 Pro + Max）。
>
> **约束**：DeepSeek V4 Pro thinking 启用时温度强制为 0；多轮工具调用中 `reasoning_content` 须随 assistant 消息回传（Adapter 写入 `ProviderResponse.ReasoningContent`，M4 经 `StateContext.LastReasoningContent` 跨轮持有）；HT0 下 `ThinkingMax` 可正常运行。

**`[ReasoningTokens]`**：`InferResponse.Usage` 分字段 `reasoning_tokens`，计入 [TokenBurnRate]；M3 导出 `polaris_reasoning_tokens_total` Gauge。

### 5.3 StreamBudgetGuard

实现：`internal/llm/stream_guard.go`（`StreamBudgetGuard` + `TokenBurnDetector`）。GuardChunk 摊销检查（每 100 chunk 或首 chunk）：
- **L1**：剩余预算 <= 0 → WARN（不阻断）
- **L2**：加速度检测（5s 窗口，3 采样点，accel > 3× baseline → BurnAlert → FatalStreamAbort 硬阻断）
- **L3**：预算耗尽 → 硬阻断

TokenBurnDetector 仅做单流检测；系统级燃烧速率从 M3 `polaris_token_burn_rate` Gauge 单源读取。

### 5.4 trackStreamCost

实现：`StreamGuard` 流成本追踪。正常结束记录精确 usage；FatalStreamAbort → 丢弃输出 → M4 `S_REPLAN`（禁 JSONRepair——失控 LLM 截断语义不可靠）；超最大缓冲（256KB）→ 写 workspace 临时文件 + `ErrResponseTooLarge`；正常中断 → JSONRepair + 双重安全校验。

### 5.5 JSONRepair

`StreamGuard` 内栈式括号匹配 → 自动闭合 → 移除不完整 key-value；确定性，<1ms。**双重安全校验**：①`required` 字段完整 ②`SideEffects > read_only` 且 `DiscardedKeys` 非空 → 强制拒绝并触发 `S_REPLAN`。

---

## 6. Semantic Cache

### 6.1 EmbeddingBatcher & OpenAICompatibleAdapter

`EmbeddingBatcher`（`internal/store/search/embedding_batcher.go`）+ `OpenAICompatibleEmbeddingAdapter`（`internal/llm/adapter/embedding.go`）经 OpenAI `/v1/embeddings` 标准接口接入云端/端侧 Embedding。引擎选择见 §8.1 向量化。

- **双通道独立 flush**（ADR-0109 / ADR-0105 沿革；原「单循环混装 + 20% 防饥饿 + Low 升 High」已取消，因交互请求排在后台大批之后致 30s 超时）：High 通道（SurpriseIndex、交互查询）与 Low 通道（GraphRAG、Consolidation）各自 flush 循环；Low 单批上限 `low_max_batch_size=8`；`batchWindow=10ms`、`maxBatchSize=100`、每次下游调用 `call_timeout_s=30` 独立超时（值以 `spec/state.yaml §m1_router.embed_*` 为准）。
- **背压**：High 队列 ≥80% → 指数退避（50ms 起，max 2s）；Low 队列 ≥80% → 强制排队 30ms。
- **文本去重**：相同文本重复入队只占一个槽位，结果扇出给全部等待者；dedup 按通道隔离。
- `search.Embedder.Embed(ctx, text)` 携带调用方 ctx。
- 发往远程 API 前文本必须经 [PIIGuard] 红化。

### 6.2 SemanticCache

实现：`internal/store/search/semantic_cache.go`（`SemanticCache`，含 `CacheStore` 接口与 LRU）。向量后端 `internal/store/search/surreal_cache_store.go`（`SurrealCacheStore`，SurrealDB-Core HNSW）；`cmd/polaris/boot_substrate.go` 在 `surrealStore != nil` 时构造并经 `llm.WithSemanticCache` 注入 `InferenceRouter`，store=nil 为 SurrealDB 未启用时的正常降级。

- **三重匹配**：RequestHash + Namespace + SystemPromptHash；`hashRequest` = `SHA-256(Namespace + SystemPromptHash + ContextHint.Fingerprint + ActiveControlVectorLabels + TaskType + MessageContents)`。
- 缓存满时 LRU 批量淘汰 MaxEntries/10。参数见 `spec/state.yaml §m1_router.semantic_cache_similarity_threshold` / `semantic_cache_max_entries` / `semantic_cache_ttl_hours`。
- `types.WithSemanticCacheHints` 已作为死代码删除（ADR-0062 D3；`StreamInfer` 对称缓存分支未落地）。`internal/agent/fsm/epoch.go` 仍逐条计算指纹但无下游消费；重新接入须先在 ADR 中论证 `StreamInfer` 对称缓存设计。

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
| Token Limit | 400 | 压缩 context 后重试（请求侧故障语义见 §10 `ClassifyError`） |

### 7.3 CircuitBreaker

三态 Closed→Open（`failureThreshold` 次连续失败）→HalfOpen（冷却 `cooldownPeriod` 后探测）→探测成功回 Closed；参数 `spec/state.yaml §m1_router.circuit_breaker_*`。全部 Provider 不可用返回 `ErrAllProvidersFailed`（inv_M1_05）。无独立 `FallbackExecutor`：健康判断在 `ProviderRegistry`，推理 failover 在 `InferenceRouter`（`internal/llm/router_failover.go`）。

**StreamInfer Failover**：与 `Infer` 同等纳入 CircuitBreaker；每次调用记录延迟并更新 healthScore，Provider 报错触发切换（`internal/llm/router_stream.go`）。流式错误中断时 EventLog 追加 `streaming_interrupted: true`（inv_M1_04）。

**Provider 恢复事件**：`ErrAllProvidersFailed` 触发后，任一 Provider 完成 HalfOpen→Closed 时，`ProviderRegistry` 恢复回调（`InjectRecoveryHandler`，`internal/llm/router.go`）向 M2 Outbox 写 `MutationIntent{Table:"outbox", Op:OpInsert, Payload:{target_engine:"m4_provider_recovery", provider_id, recovered_at}}`，经 Outbox Worker 投递至 M4（M4 §8 Provider 恢复唤醒路径），唤醒 `Suspended(suspend_reason=provider_exhausted)` 的任务；多 Provider 同时恢复时同 batch 内按 `task_id` 去重。

### 7.4 HealthScorer

`providerEntry.healthScore()`（`internal/llm/provider_registry.go`）= 可用性 40%（最近 N 次成功率）+ 延迟 30%（P95 趋势）+ 成本 20%（实际 vs 预估偏差）+ 质量 10%（token 截断率、finish_reason 分布）。低于阈值降权。

---

## 8. 本地推理与向量化

**本地推理默认关闭（OFF-by-default，ADR-0109）**：不自动拉起任何本地推理子进程，不自动注册本地对话 Provider。本地对话模型仅在用户于 provider_catalog 显式添加 `ollama`（HTTP 接本机 Ollama）或 `llama_local`（同进程 FFI `LocalAdapter`，需二进制以 `--features tier1` 编译，`ffi.LlamaAvailable()` 为真才构造）类型 Provider 时存在。不参与主路由，仅是用户显式选择的备选。

**向量化（Embedding）选择顺序**（`cmd/polaris/boot_embedding.go chooseEmbedding`，ADR-0109 D1 唯一事实源；`[embedding].backend = auto|onnx|ollama|llama_server|none`，默认 `auto`）：
1. `[embedding].base_url` 非空 → 远程 OpenAI 兼容 API（最高优先，绝不再拉起本地 Ollama）
2. `backend = ollama|llama_server` 且 `model` 非空 → 显式本地服务（`internal/llm/ollamamgr`）
3. `auto|onnx` 且 ONNX 可用 → 进程内 ONNX 嵌入器（默认，`internal/llm/embedonnx`，purego 调独立钉版本的 ORT，与语音资产完全解耦）
4. 否则 → 不挂载嵌入器，检索降级 SQLite FTS（BM25）

进程内 ONNX 档位：平衡档 EmbeddingGemma-300M int8（Matryoshka 截断 512 维）/ 轻量档 `bge-small-zh-v1.5` int8（原生 512 维）；两档统一 512 维（HNSW DIMENSION 不变，按 `embed_model_version` 重嵌）；首次启用基准自动降档（结论持久化于 preferences `embed.tier_bench`）；懒加载，空闲 10 分钟卸载。远程路径默认 `inference.embedder_dim=1536`。模型选型演进（Qwen3-0.6B GGUF → ONNX 默认）见 ADR-0109（取代 ADR-0062 相关选型）。本地 384 维隔离见 M10 §2 双索引方案。

**语音（STT/TTS）三档**（ADR-0110；引擎 `internal/llm/stt`、`internal/llm/tts`，生命周期 `internal/llm/audiorun`，资产清单 `internal/llm/audioassets` 钉死 URL/sha256）：

| 档 | 条件 | 能力 |
|---|---|---|
| A | 总内存 <1800MB 或 <2 核 | STT/TTS 均关 |
| B | 总内存 <3600MB（2GB/2 核起） | 仅 STT（SenseVoice int8）；朗读用前端系统语音 |
| C | ≥3600MB 且 ≥2 核 | STT + 服务端 TTS（MeloTTS，首次基准 RTF ≤ 0.8，过慢回系统语音；下载前先以 STT 代理测速） |

资产由守护进程后台串行预置（`[inference.audio].auto_install`，可关），懒加载、空闲卸载（`idle_unload_minutes=10`）；`[inference.tts].provider = sherpa|http`。改下载 URL/资产名须跑 `make audio-nettest`。

### 8.1 LocalProvider

`protocol.LocalProvider`（`internal/protocol/interfaces_llm.go`）扩展 `Provider`：`LoadModel` / `UnloadModel` / `EvictKVCache` / `LocalStatus` / `Probe`（供网络隔离检查与 StartupCheck）。唯一实现 `LocalAdapter`（`internal/llm/adapter/local.go`）→ `internal/ffi/llama.go`（purego 懒绑定 + `recover()` 降级）→ `rust/substrate/src/llama_infer/`（`llama-cpp-2`，`tier1` feature 门控）。

- **Rerank 走进程内 FFI**（`llama_infer_rerank`），算法为双塔嵌入余弦相似度（复用 Embed），非 cross-encoder——GGUF 生态无统一 cross-encoder 调用约定。
- **单模型槽位**，无 Blue-Green 双模型常驻（Tier-1/3 内存有限）。
- **不绑定模型型号**：`LoadModel(modelPath, opts)` 接受任意 GGUF，选型由调用方决定。
- **StreamInfer 非逐 token**：`llama_infer_generate` 为 run-to-completion，整体生成后单帧下发；逐 token 需 Rust 侧回调式 FFI（已知后续优化）。
- **KV Cache**：跨请求复用一个 `LlamaContext`，不做增量前缀复用，`generate()` 起始总 `clear_kv_cache()`；`EvictKVCache` 独立暴露。
- **GBNF**：`types.InferOptions.ResponseFormat.Type == "gbnf"` 时 `Grammar` 透传 `LlamaSampler::grammar()`。
- **硬件门控**：`FeatureLocalInference`（`internal/observability/probe/feature_gate.go`，`MinTier: Tier1, MinMemoryMB: 2048`）+ `ffi.LlamaAvailable()` 两个独立维度，二进制未编译则不注册必败 Provider。

### 8.2 生命周期

- **懒加载**：构造时不加载模型（`ModelID()` 返回 `local:unloaded`），`LoadModel` 显式触发。
- **热切换**：覆盖式替换，先完整 unload 再加载（单槽位，`rust/substrate/src/llama_infer/mod.rs` `ModelHolder`）。
- **内存压力联动**：`cmd/polaris/boot_substrate.go` 启动 `autoConf.RunMemoryWatcher`（5s 轮询）驱动 `internal/observability/auto_config_pressure.go`：Warning → `evictLocalKVCache`（清 `llama-local` KV cache）；Critical（空闲 <1GB）→ Gate 置 `FeatureLocalInference` Disabled → `unloadLocalModel`（3s 超时，fail-open）→ GC → 清沙箱。
- **命令面**：用户面 `/model local <model_id>` 命令解析/HTTP 端点未对接，生命周期方法仅在 Go API 层可经 `ProviderRegistry.Get("llama-local")` 类型断言调用。

---

## 9. ModelVersionRegistry

`ModelVersionEntry` 持有 Provider/ModelID/版本/废弃状态、PromptTemplate/ToolCallStyle/MaxContext/Capabilities，以及 `ValidatedOn` 与 `CompatibilityScore`（0-1）。迁移三档：≥0.9 自动 / 0.7–0.9 自动+WARN / <0.7 禁止自动；`OnModelUpgrade` 评分 <0.8 发 WARN；连续 3 次 4xx/5xx 自动回退；Embedding 模型废弃触发重嵌。

三层结构（已实现）：
- DDL：`internal/protocol/schema/033_model_version_registry.sql`（`model_version_entries`）
- 接口：`internal/protocol/repo/repo_modelversion.go`；SQLite 实现：`internal/store/repo/repo_modelversion.go`
- 业务：`internal/llm/modelregistry/registry.go`（`Registry`）：`DecideMigration(score)`；`OnModelUpgrade(ctx, provider, modelID, skillNames, tester)`（`SkillCompatTester` 为 consumer-side 接口，无生产实现时传 `nil` 仅刷新元数据）；`RecordCallResult` 连续失败 3 次返回回退目标（`FindPredecessor`）；`DeprecateModel` 标记废弃并在 `Capabilities.embedding=true` 时调 `ReindexTrigger`。

**偏离设计**：不建影子表/Blue-Green 重嵌——`OnlineReindexer`（`internal/memory/retrieval/online_reindexer.go`）按 `episodic_events.embed_model_version` 与 `Embedder.ModelVersion()` 差异驱动，切模型后下一次 `Run()` 自然重嵌；`ReindexTrigger` 仅提供「立即」语义（`cmd/polaris/boot_memory.go` `startOnlineReindexer`，取代 5min 周期等待）。

`internal/llm/adapter/{anthropic,openai,deepseek}.go` 的 `resolveXXXModel()` 保留为无 DB 依赖的编译期兜底；`Registry.SeedFromStaticResolvers(ctx)` 幂等灌入其废弃名映射（`CompatibilityScore=1.0`，已存在不覆盖），启动时由 `bootMemory`（`cmd/polaris/boot_memory.go`）调用一次。测试：`internal/llm/modelregistry/registry_test.go`、`internal/store/repo/repo_modelversion_test.go`。

---

## 12. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| 单 Provider 限流/不可用 | EWMA p95 > 阈值 / 5xx 连续 | 仅本 Provider | Fallback：同 Pool → 降级 Pool → 拒绝 | 半开探测 | 同 Pool 全断 → audit severity=warn |
| 全部 Provider 熔断 | CircuitBreaker 全开 | 全模块 LLM 路径 | 返回 `ErrAllProvidersFailed` | 冷却期满后半开探测 | 持续 > 5min → audit |
| SemanticCache 满 (10000 entries) | 内存水位触发 | 仅缓存查询 | LRU 淘汰 MaxEntries/10 条目 | 自动 | — |
| StreamBudgetGuard L2 触发 | TokenBurnDetector 加速度 (5s 窗口, accel > 3× baseline) | 单流 | FatalStreamAbort → M4 `S_REPLAN` | TokenBurnRate 恢复后自动解除 | 单 session 反复触发 ≥3 次 → audit |
| 显式本地模型加载失败 | `LoadModel` err / RSS 检测 | 本地 Provider 路径 | 该 Provider 不可用，Router 按 §7 换 Provider | 空闲内存恢复后重新加载 | local_only 模式下死锁恢复仍失败 → HITL |
| Embedding 不可用 | err / timeout | 检索路径 | EmbeddingBatcher 返回错误，调用方降级 BM25/FTS5 | 恢复后自动切回 | 全断 > 1h → audit |
| EmbeddingBatcher High 队列 >80% | 队列水位 | 非交互 embedding | 指数退避 (50ms→2s) | 水位下降后恢复 | 持续 > 5min → audit |
| ModelVersionRegistry 废弃迁移失败 | 连续 3 次 4xx/5xx | 单模型 | 保持旧模型 + WARN + 禁止自动切换 | 下一检测周期重试 | 版本 EOL 日迫近 |

与内存压力联动：见 §8.2（Critical → 卸载本地模型；L3 临界 → 全部 LLM 调用路由至远程 API）。

---

## 10. 凭证池与速率限制追踪

组件位于 `internal/llm/`：

- **CredentialPool**（`credential_pool.go`）：多 Key 线程安全池，四种策略（FillFirst / RoundRobin / Random / LeastUsed）；失败按错误类型设冷却（Auth 5min / Billing+RateLimit 60min / AuthPermanent 30 天）。
- **RateLimitTracker**（`rate_tracker.go`）：解析 12 个速率限制头字段，去相关抖动指数退避建议；`RateLimitCapturingTransport` 为 RoundTripper 包装。**现为库组件，未接入生产出站链路**（GR-2.2-006：原唯一构造点全仓零读取，已随孤儿字段删除）；限速冷却现由 `ClassifyError` → `CredentialPool`（RateLimit 60min）承担；若要按响应头精确退避，须在 `boot_substrate.go` 的 SafeHTTPClient 外层包 `RateLimitCapturingTransport` 并让选路读 `Tracker.Get`，二者缺一即假接线。
- **`ClassifyError`**（`error_classifier.go`）：HTTP 状态码 + 关键词分 17 种失败原因，返回 `*ClassifiedError`（可重试性、上下文压缩、凭证轮换、降级策略提示），覆盖 Anthropic/OpenAI/DeepSeek/Gemini/Ollama/阿里云/火山引擎错误体。`ShouldCompress` 类（上下文超限/payload 过大）是请求侧故障：不计入熔断与成功率，只 failover 到 `MaxContextTokens` 更大的 Provider，否则以 `protocol.ErrContextOverflow` 交还调用方（`router_request_fault.go`，ADR-0020）。

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
| 接口定义 | Provider/InferRequest/InferResponse/StreamEvent | internal/protocol/interfaces_llm.go, types.go |
| 全局字典 | TokenBurnRate/SurpriseIndex 完整定义 | 00-Global-Dictionary §3 |
