# ADR-0020: LLM Provider 选型、推理路由与上下文溢出/spill 故障语义

> 状态：Accepted ｜ 首次：2026-06-08 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0022、ADR-0100 ｜ 模块：M01 / M04 / M05 / M09 / M12

## 旧引用映射

| 旧引用 | 新节 |
|---|---|
| 0020 原决策 | 决策一 |
| 0020 决策二 / 原 ADR-0022 | 决策二（档位规则已被 ADR-0105 决策二取代，见沿革） |
| 0100 决策一 | 决策三 |
| 0100 决策二 | 决策四 |
| 0100 决策三/四 | 决策五 |

## 决策一：DeepSeek V4 为全系统默认核心模型（原 ADR-0020 原决策）

- DeepSeek V4（Flash/Pro）为 Tier-0 默认与权威基准 provider（缓存命中输入 0.02 元/百万 token，海外头部模型的 1/50~1/1000）；后台高频任务（记忆压缩/自进化）不再设预算卡点，"省钱降频"设计放弃。
- 本地模型（Local-SLM）不是省钱降级方案，而是 Tier-3（64GB）的高级特权，仅用于：<10ms 微决策、数据主权（`local_only`）、断网灾备。
- 多 provider 共存是配置选项而非默认值；不得把 OpenAI GPT-4 / Claude 设为全系统默认。

## 决策二：ThinkingMode 取代 BestOfN/MCTS 多候选（原 ADR-0022；档位触发规则已被取代）

- 废弃 BestOfN/ParallelSampler/MCTS（从未实现，DeepSeek V4 Pro 原生 thinking 已等效覆盖；成本 3 路→1 路降 67%），改由 `ThinkingMode`（`pkg/types/enums_llm.go`）驱动 Provider 原生 thinking。
- thinking 启用时 temperature 强制为 0；`reasoning_content` 须随 assistant 消息跨轮回传（`StateContext.LastReasoningContent`）。
- 档位选择现行规则：`metrics.SelectPlanTier`（`internal/observability/metrics/metrics_handler.go`）→ ADR-0105 决策二。原 `SelectThinkingMode` 三档规则（SI<0.3→Disabled / 0.3~0.6→High / ≥0.6、replanCount>0 或 TaintLevel≥3→Max）已删除。
- 反例：重新引入 BestOfN 并发候选。

## 决策三：上下文溢出是请求侧故障，路由只做容量感知 failover（原 ADR-0100 决策一）

- `ShouldCompress` 类错误（上下文超限 / payload 过大）不说明 Provider 不健康：`recordAttempt` 与 `trackedProvider` 均不计入熔断器、成功率、模型注册表。
- failover 只转向 `MaxContextTokens` 严格大于失败方的 Provider（先同池，后不限 role 全局；窗口未知不参选；窗口严格递增保证终止）。无更大窗口或更大窗口也失败 → 返回包装 `protocol.ErrContextOverflow` 的错误。
- 实现 `internal/llm/router_request_fault.go`（`isRequestFault` / `recordAttempt` / `overflowFailover` / `streamOverflowFailover`）。
- 动因：此前超限请求被逐个发给池内每个 Provider、打开健康 Provider 熔断器、报 `ErrAllProvidersFailed` 并累加 `ProviderSuspendCount`（满 5 次触发"provider 耗尽" HITL）。

## 决策四：Agent 确定性修剪一次后重试，不走 LLM 摘要（原 ADR-0100 决策二）

- `streamInferWithOverflowRecovery` 收到 `ErrContextOverflow` 后，对固定前缀（开头连续 system 消息，同 `compact.SplitPinnedHead`）之外的非 system 消息做"注水"首尾保留截断，可修剪字节降到 50%（单条下限 512B），最大的消息先截，短消息（本轮意图）原样保留；PII 令牌化之前修剪、之后重新令牌化。
- 只重试一次，须证明请求确已缩小；无可修剪内容则如实上抛。指标 `polaris.agent.context_overflow_recovery_total`。
- 不走 LLM 摘要：Stage 2 摘要会把 TaintHigh 围栏内数据改写为 assistant 角色消息（污点提权，HE-2/HE-7），首尾截断保留原角色与围栏标记。
- 遗留（未修复）：热路径 Stage 2（`agent_context_compaction.go` hotPathCompact 与网关 SessionCompressor）仍把含 TaintHigh 的 body 摘要为 assistant 消息，需单独评估。
- 代价：修剪有损，被省略的中段（召回记忆、历史）不可取回。

## 决策五：执行结果 spill 统一到可取回通道，截断首尾保留（原 ADR-0100 决策三、决策四）

- `spillExecResult`（`internal/agent/agent_execute_result.go`）对超过观察上限（`ObservationMaxBytes`=4096）的结果调用 `ToolRefOffloader.Offload` 一次；`ExecuteResult`（≤8KB）与观察（≤4KB）各自投影、共享同一卸载副本，首行 `[output truncated: N bytes total; full output: read_tool_ref(task_id=..., id=...)]`。
- 未注入卸载器或卸载失败时首行声明 `full output not retained`，不给出取不回的引用；`logs/exec_results/` 写入已删除。4–8KB 结果也会写一次工作区文件并登记 `workspace_vfs`。
- 截断原语 `pkg/util.ElideMiddle`：头 3/4、尾 1/4、UTF-8 边界对齐、标记长度按最坏情况预留；执行结果预览、观察截断、溢出修剪共用。
- 反例：截断只留头部；给模型一个没有工具能读回的引用 id。

## 已驳方案（原 ADR-0100 对 DeepSeek Harness `dsh` 的评审）

| 方案 | 驳回理由 |
|---|---|
| 超限按普通失败 failover 到同容量 Provider / 超限计入熔断 | 必然再次失败且计费；误开健康 Provider 熔断器（2026-09-25） |
| Cordis 微内核 + YAML profile 组合替换 agent loop | 与 HE-5（Go FSM 持有控制流）冲突；安全与预算门控挂在 FSM 转移上，保持 `cmd/polaris/boot_*.go` 手工装配（ADR-0088） |
| 事件溯源会话日志作请求唯一真源 | 每阶段 prompt 含非确定性召回，需把召回落为会话事件并改 5 个 Build*Context；`WriteLLMCallEvent` 已覆盖审计与崩溃回放，代价收益不匹配 |
| 系统提示词作 surface 第 0 号节点 / `request/header` 快照 | ImmutableCore 已 stable→volatile 分层且确定序；多阶段 FSM 前缀在阶段指令处分叉是固有代价 |
| repeat-tool-reminder（同工具同参数第 3/5/8 次注入提醒） | 属新增模型可见 Prompt，HE-4 要求 Eval 门控；ReplanGuard 与观察回灌兜底，无复现证据 |
| `dsh_session_log` 随请求向 Provider 上传会话日志 | 与隐私承诺冲突 |
| Agent 级持久化 `llm/retry` 事件 + 适配器单次尝试 | 重试在路由层（`DefaultBackoff` + 凭证轮换 + failover），无当前缺陷驱动 |

## 重新评估触发条件

- DeepSeek V4 成本/质量优势被后继模型明显超越（价格不再是 1/50~1/1000 量级或质量代差）→ 重议默认 provider。
- 本地模型因硬件普及（消费级 NPU 算力大幅提升）不再局限于 Tier-3 特权场景 → 重议其角色；单纯价格波动不改变"非省钱降级方案"定位。
- `polaris.agent.context_overflow_recovery_total` 持续增长且重试后仍 `ErrContextOverflow` 占比 >20% → 超限源在固定前缀（ImmutableCore + AmbientContext），按 Provider 实际窗口动态设定 ContextWindowManager 容量，而非加大修剪力度。
- 需要逐字节回放模型请求（离线 Prompt 回归 Eval 以生产请求为样本）→ 重提"事件溯源会话日志作请求真源"。
- 出现同工具同参数重复调用耗尽 max_steps 的复现样本 → 配套 Eval 后重提重复调用提醒。

## 沿革

| 日期 | 变更 | 原编号 |
|---|---|---|
| 2026-06-08 | 确立 DeepSeek V4 为默认 provider | 0020 |
| 2026-06-13 | ThinkingMode 三档路由取代 BestOfN/MCTS | 0022 |
| 2026-07-28 | 0022 合入 0020 | — |
| 2026-08-01 | 勘误：`SelectThinkingMode` 实际在 M3 `metrics_handler.go`，M4 仅调用方 | 0020 |
| 2026-09-25 | 三档规则被 `SelectPlanTier` 四级阶梯取代（用户输入恒 TaintHigh，原规则退化为常量）；门控 `TestPlanEffect_CheapFirstCascade` | 0101 决策三/七、0102 决策六 |
| 2026-09-25 | 溢出请求侧故障 + spill 统一 | 0100 |
| 2026-10-12 | 0100 并入本文，按代码核实 | — |

## 引用代码

- `internal/llm/router_request_fault.go`、`internal/llm/router.go`、`internal/llm/router_failover.go`、`internal/llm/router_stream.go`、`internal/llm/provider_registry.go`、`internal/llm/adapter/deepseek.go`
- `internal/protocol/errors.go`（`ErrContextOverflow`）
- `internal/agent/agent_overflow_recovery.go`、`internal/agent/agent_execute_result.go`、`internal/agent/agent_execute_effect.go`
- `internal/agent/fsm/observations.go`、`pkg/util/elide.go`、`pkg/types/enums_llm.go`
- `internal/observability/metrics/metrics_handler.go`（`SelectPlanTier`）
