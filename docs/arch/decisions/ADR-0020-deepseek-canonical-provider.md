# ADR-0020: LLM Provider 选型与推理路由合集（DeepSeek V4 默认 + ThinkingMode 三档路由，含原 ADR-0022）

- **状态**: Accepted | **日期**: 2026-06-08（路由方案 2026-06-13，合并 2026-07-28）| **模块**: M01/M04/M05/M09/M12

## 决策一：确立 DeepSeek V4 为全系统默认核心模型（原决策）

DeepSeek V4（Flash/Pro）确立为 Tier-0 默认及权威基准 provider——缓存命中输入低至 0.02 元/百万 token，是海外头部模型的 1/50~1/1000，使记忆压缩/自进化等后台高频任务的预算卡点逻辑大幅简化，"省钱降频"设计被放弃。

本地模型（Local-SLM）不是"省钱降级方案"而是"处理极端场景的高级特权"——仅限 Tier-3（64GB）用于：物理延迟极限（<10ms 微决策）、数据主权/物理隔离（`local_only` 模式）、可用性灾备（断网生存套件）。

## 决策二：ThinkingMode 三档路由取代 BestOfN/MCTS 多候选方案（原 ADR-0022）

废弃 BestOfN/ParallelSampler/MCTS 多候选路由（从未真正实现，且 DeepSeek V4 Pro 原生 extended thinking 已等效覆盖），改用 `ThinkingMode` 三档驱动 Provider 原生 thinking：

| 档位 | 触发条件 | 映射 |
|------|---------|------|
| `ThinkingDisabled` | SI<0.3 且 replanCount=0 且 TaintLevel<3 | 无 thinking 字段 |
| `ThinkingHigh` | 0.3≤SI<0.6 | `reasoning_effort=high` |
| `ThinkingMax` | SI≥0.6 或 replanCount>0 或 TaintLevel≥3 | `reasoning_effort=max` |

`SelectThinkingMode(replanCount, maxTaint, surpriseIndex)` 由 M4 `transitions.go` 在 LLM 调用前调用。
> 位置勘误（2026-08-01）：本 ADR 原文将该函数定位在 M4 `transitions.go`，实际定义在 M3
> `internal/observability/metrics/metrics_handler.go`；M4 仅为调用方。决策内容不变。

thinking 启用时 temperature 强制为 0；`reasoning_content` 须随 assistant 消息跨轮回传（`StateContext.LastReasoningContent`）。

## 反例守护

拒绝将 OpenAI GPT-4/Claude 设为全系统默认 provider——多 provider 共存是配置选项而非默认值。拒绝重新引入 BestOfN 并发候选——原生 thinking 已覆盖需求且更简洁；成本降 67%（3 路→1 路）。

## 引用代码

`internal/llm/`、`internal/llm/adapter/deepseek.go`、`internal/agent/agent_execute_effect.go`（原 `agent_execute.go` 已拆分为多个 `agent_execute_*.go` 文件）

> 2026-08-09 追记：重新评估触发条件——① DeepSeek V4 的成本/质量优势若被后继模型
> 明显超越（价格不再是 1/50~1/1000 量级或质量出现代差），重议默认 provider；
> ② 本地模型定位若因硬件普及（如消费级设备 NPU 算力大幅提升）不再局限于
> Tier-3 高级特权场景，重议其角色，但当前"非省钱降级方案"的定位不因单纯价格
> 波动而改变。
