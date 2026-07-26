# ADR-0080: 新增 Debate/Critic 编排模式

**日期**: 2026-07-26  
**状态**: Accepted  
**关联项**: GD-6 (原 GD-14-001)

## 上下文与动机

随着 Polaris 智能体应用向深层次推理发展，现有的 Sequential/Parallel/MapReduce/PatternDAG/StateGraph 编排模式均无法原生表达具有"对抗性辩论/互审"（Debate/Critic）特征的任务流。原架构缺少多 Agent 协同在同一议题下多轮博弈的内建支持，若强行使用 StateGraph 将导致节点逻辑极度膨胀。根据系统审查 GD-6 的要求，需引入原生的 `PatternDebate`。

## 决策方案

1. **引入 `DebateExecutor`**
   新增 `PatternDebate` 编排模式，作为第 11 种调度范式。
2. **复用 Blackboard 与异步挂起**
   - 避免引入新的阻塞轮询机制。
   - 复用 GD-1 设计的 `Checkpoint + Watcher` 异步挂起原语。
   - Judge, Proponent, Opponent 三方通过抛出特定类型 (`agent_handoff:<role>`) 的 Blackboard 事件进行交互。
3. **配置参数**
   引入 `debate.max_rounds` 和 `debate.concurrent_sides` 作为阈值参数。初始阶段强制串行交替，防止多 LLM 实例并发导致 OOM (Tier-0 约束)。

## 后果与影响

**正面收益**
- 丰富了编排原语，原生支持基于对抗辩论的复杂意图澄清、方案择优场景。
- 遵循了零阻塞架构方向，通过 Checkpoint 和 Suspend 保障了系统的可伸缩性与故障恢复力。

**负面影响及缓解**
- 引入新的状态机轮转逻辑，调试难度微增。
- 对 Blackboard 带来了轮转写入的频次提升。通过 `max_rounds` 上限避免死循环。
