# internal/swarm — 模块规范

> 对应架构文档：`docs/arch/M08-Multi-Agent-Orchestrator.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

多 Agent 协同层（Arch-L3）。管理 Blackboard（任务黑板）、Supervisor Tree、
多模式编排（swarm/sequential/csv_fanout）以及后台常驻 Agent。
不包含 Agent 内部执行逻辑，只负责任务分发与生命周期协调。

## 权力边界 [MUST]

### 拥有
- Blackboard 任务的 PostTask / ClaimTask / Complete / Fail / Reaper 全生命周期管理权
- Supervisor Tree 的 Fork/Join 协调权
- 跨 Agent 编排策略的决策权（swarm / sequential / csv_fanout）

### 禁止 [MUST NOT]
- **[MUST NOT]** 持有单个 Agent 的内部执行状态（Agent 内部状态属于 `agent` 包）
- **[MUST NOT]** 直接 import `internal/agent` 的具体实现（防止 Arch-L3 → Arch-L2 实现层循环）
- **[MUST NOT]** 通过 Go Channel 或共享内存做跨 Agent 状态同步
  （跨 Agent 通信必须通过 Blackboard 事件或 OutboxWriter 接口）
- **[MUST NOT]** 在 Reaper 扫描路径（HeartbeatInterval=15s）中调用 LLM
- **[MUST NOT]** 在 Fork/Join 期间阻塞等待子 Agent 完成（子 Agent 异步完成后写 Blackboard，
  父 Agent 通过轮询或事件感知）

## 消费端接口声明位置

`internal/swarm/provider.go` — 已声明：OutboxWriter、LLMInfer、SwarmMetrics。
新增外部依赖时先在此文件声明接口，由 `bootstrap` 注入。

## 防死锁约束

任务槽位分配必须区分新任务配额与续传配额（参考 polaris-agent executor Asymmetric Pool 设计）：
预留 20% 的 Worker 槽位给"继续中的子任务"回调，防止新任务占满所有槽位导致父子任务死锁扇入。
