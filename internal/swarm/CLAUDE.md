# internal/swarm — 模块规范

> 架构文档：`docs/arch/M08-Multi-Agent-Orchestrator.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

多 Agent 协同策略层（L2）。管理 Supervisor Tree（`supervisor/`）、任务分解（`planner/`）、后台常驻 Agent（`agents/`）。不含 Agent 内部执行逻辑，也不含 Blackboard/多模式编排实现——后者唯一实现在 `internal/execute/orchestrator`（ADR-0046，见 `internal/execute/CLAUDE.md`），swarm 是其消费方：只决定"分发策略与生命周期协调"，"如何执行"交给 `internal/execute`。

## 权力边界 [MUST]

### 拥有
- Supervisor Tree 的 Fork/Join 协调权
- 跨 Agent 编排策略的决策权（swarm / sequential / csv_fanout 模式选择）
- 任务分解策略权（`planner`：目标 → 子任务 DAG）

### 禁止 [MUST NOT]
- 持有单个 Agent 的内部执行状态（属 `agent` 包）
- 直接 import `internal/agent` 具体实现（防 L2 → L1 实现层循环）
- 通过 Go Channel 或共享内存做跨 Agent 状态同步（经 Blackboard 事件）
- 在 Reaper 扫描路径（HeartbeatInterval=15s）中调用 LLM
- 在 Fork/Join 期间阻塞等待子 Agent（子 Agent 异步完成后写 Blackboard，父 Agent 轮询或事件感知）
- 重新实现 Blackboard/多模式编排逻辑（只 import `internal/execute/orchestrator` 消费）

## 消费端接口声明位置

本包不设 `provider.go`（原三个接口全仓零消费方，已删；`swarm/agents` 用自己的函数型契约 `LLMInferFunc`）。新增外部依赖时在本包内新建 `provider.go` 声明消费端接口（HE-3），由 `cmd/polaris/boot_*.go` 装配注入。

## 防死锁约束

任务槽位分配须区分新任务配额与续传配额（参考 polaris-agent executor Asymmetric Pool）：预留 20% Worker 槽位给"继续中的子任务"回调，防止新任务占满槽位导致父子任务死锁扇入。
