# ADR-0037: PatternDAG Orchestration

## 1. Context (上下文)

PolarisAGI 在 `M08-Multi-Agent-Orchestrator` 模块中支持了多种编排模式（Supervisor, Hierarchy, Sequential, Parallel, MapReduce, Reflection, Swarm, Pipeline）。
然而，随着跨 Agent 任务规划复杂度的上升，简单串行或并行模式难以描述“部分并行、部分串行、存在交错依赖”的工作流。
此前在 `internal/agent/dag/` 已实现单 Agent 内部工具级（Micro-DAG）的任务图执行。为在宏观 Agent 协调层（Macro-DAG）解决复杂的拓扑依赖，我们需要引入一种新的强类型 DAG 编排引擎：`PatternDAGExecutor`。

## 2. Decision (决策)

1. **引入 PatternDAGExecutor**：新增编排模式 `ModePatternDAG`（对应图规范 `protocol.WorkflowGraphSpec`），以支持跨 Agent 边界的有向无环图调度。
2. **复用通用图校验逻辑**：将单 Agent 内部 `agent/dag` 的图检验逻辑（环检测，深度限制）抽取下沉至 `pkg/graph/dag.go`，并在 `PatternDAGExecutor` 和原 `DAGPlan` 中统一使用该校验组件。
3. **基于黑板的事件驱动执行**：基于 Kahn 的拓扑排序算法思想，通过 Blackboard `Subscribe()` 事件进行驱动。仅将“所有上游依赖已完成”的节点分发给黑板（`PostTask`）。
4. **统一意图与上下文传递**：DAG 中节点的任务 Intent 注入当前节点 ID 与其所有前置依赖的 `output`。
5. **Fail-Fast 与 逆序补偿 (Saga)**：任意一个 DAG 节点失败，立即停止向黑板投递后续就绪节点（Fail-Fast）。针对已经执行完毕并持有 `CompensationAction` 的节点，按照拓扑逆序并发投递补偿任务。复用流水线（Pipeline）的最佳努力（Best-Effort）补偿监控组件。

## 3. Consequences (影响)

- **正向影响**：提升了框架表达能力，允许以统一的原语声明和执行交错的多 Agent 任务。完善了补偿流（Saga），确保强依赖图流产时能回收资源和撤销副作用。统一了微观与宏观 DAG 的图分析算法，减少了冗余代码。
- **负向影响**：黑板（Blackboard）监听事件数进一步增加。必须控制 DAG 的 `MaxDepth`，防止因大图导致调度资源耗尽。

## 4. Status (状态)

**Accepted** (2026-07-09)
