# internal/execute — 模块规范

> 架构文档：`docs/arch/M04-Agent-Kernel.md`（dag）、`docs/arch/M08-Multi-Agent-Orchestrator.md`（orchestrator）；决策：`docs/arch/decisions/ADR-0046-execute-module.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

单/多 Agent 执行引擎层（服务 L1 + L2；整合自原 `agent/dag` 与 `swarm/orchestrator`）。只负责"如何把一份已确定的计划/图跑完"：拓扑调度、并发执行、Saga 补偿、条件路由、扇入语义、四层安全校验。不做决策：目标分解属 `swarm/planner`，单 Agent 控制流属 `agent/fsm`，跨 Agent 编排策略属 `swarm/supervisor`/`swarm/topology`。

```text
internal/execute/
  dag/           单 Agent 内工具链 DAG 执行器 + S_VALIDATE 四层校验（ValidateDAG）。
                 消费方：internal/agent（经 provider.go 的 DAGRunner/DAGValidator）
  orchestrator/  跨 Agent Blackboard（任务黑板 CAS）+ 多模式编排（Sequential/Parallel/MapReduce/
                 PatternDAG/StateGraph/Swarm/Debate/CSV-Fanout）。消费方：internal/swarm、
                 internal/gateway/server/sysadmin/workflowadmin
```

两个子包不合并：`dag` 调度对象是工具调用，生命周期是一次 FSM 步骤内同步执行，失败语义是 Saga 补偿；`orchestrator` 调度对象是 Agent 任务，生命周期跨多个异步 Agent，失败语义是 Blackboard 重试/Reaper。按调度对象边界拆分，不拍平为巨石包（ADR-0046）。

## 权力边界 [MUST]

### 拥有
- DAG/图拓扑校验、并发调度、Saga 逆序补偿的唯一实现权（`dag`）
- Blackboard 任务全生命周期（PostTask/ClaimTask/Complete/Fail/Reaper）的唯一实现权（`orchestrator`）
- S_VALIDATE 四层校验管线（L0 拓扑 / L1 Taint / L1 Policy / L2 Heuristic / L3 LLM 看门狗）的唯一实现权（`dag`）

### 禁止 [MUST NOT]
- 持有"做什么"的决策权（分解目标属 `swarm/planner`；LLM 生成 DAG 内容属 `agent/fsm` 的 S_PLAN）
- 直接 import `internal/agent` 具体实现（`agent` 经 `agent/provider.go` 的 `DAGRunner`/`DAGValidator` 反向注入本模块实现，方向不可颠倒）
- 在 Reaper 扫描路径（HeartbeatInterval=15s）中调用 LLM
- 在 DAG 节点执行/Blackboard CAS 认领路径中做非幂等的静默重试（HE-2：失败必须产生可观测的结构化错误）

## 消费端接口声明位置

- `internal/agent/provider.go` 的 `DAGRunner`/`DAGValidator`：由 `execute/dag.Runner`/`execute/dag.Validator` 实现，`cmd/polaris/boot_agent.go` 构造并注入；二者无状态，可全局共享
- `orchestrator` 的消费方接口就地声明在该子包内，不寄居 swarm 包

## 不可变内核保护

`internal/execute/dag/` 列入 `internal/config.ImmutableKernelPackages()`（承载 HE-2 安全边界 L1 TaintGate + L1 Cedar PolicyGate）。`internal/execute/orchestrator/` 不在名单内。
