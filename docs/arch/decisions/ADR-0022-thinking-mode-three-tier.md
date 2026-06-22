# ADR-0022: ThinkingMode 三档路由取代 BestOfN/MCTS 多候选方案

- **状态**: Accepted
- **日期**: 2026-06-13
- **决策者**: 架构组
- **相关模块**: M01 (Inference), M04 (Agent Kernel)

## 上下文

M1 §5.2-bis 原设计了 `[BestOfN]` + `[SelfConsistency]` 的 `ParallelSampler` 路径：并发生成 N 路候选答案后做 MajorityVote/Verifier 聚合，用于提升推理质量。该方案存在以下问题：

1. `ParallelSample` 从未在 `internal/llm/` 中实现，路由层仅支持单路推理
2. N 路并发调用成本倍增（cost×N），在 DeepSeek V4 Pro 之前的 Provider 价格下尚可接受，但架构复杂度高
3. DeepSeek V4 Pro 提供原生 extended thinking（`reasoning_effort` + `thinking.type="enabled"`），其单次调用内部已做多步推理链展开，效果等价于 BestOfN，且无需调用方编排

## 决策

**废弃 BestOfN/ParallelSampler/MCTS 多候选路由方案，改用 `ThinkingMode` 三档驱动 Provider 原生 thinking。**

`SelectThinkingMode(replanCount, maxTaint, surpriseIndex)` 在 M4 `transitions.go` 中决定三档（实际签名：`func SelectThinkingMode(replanCount int, maxTaint protocol.TaintLevel, surpriseIndex float64) protocol.ThinkingMode`）：

| 档位 | 触发条件 | DeepSeek V4 Pro 映射 |
|------|---------|----------------------|
| `ThinkingDisabled` | SI < 0.3 且 replanCount=0 且 TaintLevel < 3 | 无 thinking 字段 |
| `ThinkingHigh` | 0.3 ≤ SI < 0.6 | `reasoning_effort="high"` + `thinking.type="enabled"` |
| `ThinkingMax` | SI ≥ 0.6 或 replanCount > 0 或 TaintLevel ≥ 3 | `reasoning_effort="max"` + `thinking.type="enabled"` |

附加约束：
- thinking 启用时 temperature 强制为 0（DeepSeek V4 Pro API 要求）
- 多轮工具调用序列中，`reasoning_content` 必须随 assistant 消息回传至下一轮——Adapter 负责从 `resp.Choices[0].Message.ReasoningContent` 提取写入 `ProviderResponse.ReasoningContent`，M4 通过 `StateContext.LastReasoningContent` 跨轮持有
- `ThinkingMax` 在 HT0 下可正常运行（V4 Pro 成本低，无预算门控）

## 后果

- **正向**: 架构大幅简化——删除 `ParallelSample` 编排逻辑、多候选 goroutine 管理、MajorityVote/Verifier 聚合；推理质量提升由 Provider 侧保证，调用方零额外工作
- **负向**: Provider 绑定性增强（依赖 DeepSeek/Claude 的 thinking 字段）；非 thinking Provider（普通 GPT-4o 等）仅走 `ThinkingDisabled`，对这类 Provider 无质量提升手段
- **反例守护**: 未来若有人提议"重新引入 BestOfN 并发候选"，引用本 ADR 拒绝——原生 thinking 已覆盖该需求，且实现路径更简洁

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留 BestOfN，与 ThinkingMode 并存 | 架构双轨维护成本高；在 DeepSeek V4 Pro 下 BestOfN 收益边际为零 |
| MCTS 外部树搜索 | V4 Pro 原生 extended thinking 内部已实现等效的多步展开，外部 MCTS 增加网络延迟和代码复杂度，无额外收益 |
| ReasoningEffort {low/medium/high} 三档（旧方案） | 粒度与 M4 路由信号（SurpriseIndex/replanCount/TaintLevel）解耦，且 low/medium/high 语义模糊；ThinkingMode 与 M4 路由信号直接绑定，语义更清晰 |

## 引用代码

- `internal/llm/adapter/deepseek.go`（thinking 字段写入 + ReasoningContent 提取）
- `internal/llm/adapter/client.go`（`OpenAIRequest.Thinking` / `ReasoningEffort` 字段）
- `internal/agent/agent_execute.go`（`SelectThinkingMode` 调用 + `LastReasoningContent` 存入）
- `internal/agent/fsm/state_machine.go`（`StateContext.LastReasoningContent` 字段）
- `docs/arch/M01-Inference-Runtime.md §5.2-bis`
- `docs/arch/M04-Agent-Kernel.md §5`

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-13 | 初稿 |
| 2026-06-17 | 修正 SelectThinkingMode 参数顺序：ADR 原写 (SI, replanCount, TaintLevel)，实际签名为 (replanCount, maxTaint, surpriseIndex) |
