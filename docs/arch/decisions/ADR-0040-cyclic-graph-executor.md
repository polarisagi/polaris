# ADR-0040: 受控循环图执行器 (CyclicGraphExecutor)

## 1. Context
目前在 `internal/agent/pattern` 下支持 Micro-DAG 和 `PatternDAGExecutor`。这些执行器主要是基于有向无环图 (DAG) 的静态流转，限制了复杂逻辑（如多轮尝试、分页抓取、错误重试循环）。
在 ADR-0037 中，曾提议过一种全局循环图方案被驳回，原因是容易导致死循环与状态难以控制，以及当时缺乏有效的计算配额控制机制。

### 与 ADR-0037 的差异点（前提假设变更）
1. **控制粒度**：ADR-0037 提议的是不限制边界的无限状态机。本方案采用"严格受控"（Bounded）循环，明确定义 `MaxIterations`，到达上限强制终止。
2. **算力与成本感知**：当前的 M4Kernel 已经具备了完整的 Budget 计算配额管理。通过挂载 `CostThreshold` 和 `IterationBudget`，在每条回溯边（Back-edge）流转时扣减算力。
3. **退化降级**：ADR-0037 中的异常节点会导致整图崩溃，本设计要求每条回溯边必选定义 fallback 路径，确保无论何种跳出都能提供残缺或局部结果。

## 2. Decision
引入 `CyclicGraphExecutor` 作为 `PatternDAGExecutor` 的超集。它在解析静态执行图时支持有向循环，但对其执行机制附加如下约束：

### 2.1 边类型扩展 (Back-edge)
- 在原有 DAG 定义中引入 `EdgeType: BackEdge` 概念。
- 回溯边必须带有 `Condition` 和 `MaxIterations`。每次通过回溯边将特定上下文的计数器加 1。

### 2.2 协议层 state.yaml 扩展
在 `docs/arch/spec/state.yaml` 的 `thresholds` 扩展循环限制：
```yaml
thresholds:
  m8_orchestrator:
    cyclic_graph_max_iterations: 5
    cyclic_graph_timeout_sec: 180
```

### 2.3 执行引擎关系 (替代/并存)
`CyclicGraphExecutor` **不替代** `PatternDAGExecutor`。
- `PatternDAGExecutor` 维持原样，专为确定性一次流转的廉价任务提供极低开销执行。
- `CyclicGraphExecutor` 则提供给重型多步骤自动化工具（如大规模网络爬虫、递归归纳任务），二者在 `internal/agent/pattern` 下并存，按 Task Type 进行选择。

## 3. Consequences
- **Positive**: 极大地扩展了工具链的能力组合，能够支持自动化任务（如 auto-heal, auto-pagination）。
- **Negative**: 会增加系统运行时内存消耗，并在调试轨迹追踪（Trace）上带来一定复杂性（同一个节点会存在不同 Iteration 下的多次快照）。
- **Mitigation**: 限制回溯层的最大深度，并在 UI/Trace 日志中强制附加 `IterationIndex` 标签。
