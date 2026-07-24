# ADR-0076: Task Checkpoint and Resumption

## Status
Accepted

## Context
在当前的执行模块中，Reaper 在任务超时后将其整体重置为 `Pending` (见 ADR-0057 崩溃恢复，主要覆盖 perceive/plan/reflect 阶段)，但对于 Execute 阶段保守跳过，缺乏节点级别的恢复记录。这导致如果在执行长程 DAG 的部分节点后发生崩溃，重新执行可能会重放非幂等副作用（例如发邮件、转账或外部写操作）。

我们需要补齐执行阶段的容错能力，记录已经完成的 NodeResult，使得在发生崩溃恢复时可以断点续跑，跳过已经成功完成的节点，而不再重复触发它们的副作用。

## Decision
1. 新增 `task_checkpoints` 数据库表，以 `(task_id, node_id, attempt)` 为主键，记录各节点的执行状态 (`pending`, `executing`, `done`, `failed`) 和输出 (`output_json`)。
2. 在 `StateGraphExecutor.Execute` 层面介入执行逻辑：执行每个 Node 前先查表是否已有 `done` 的记录，命中则直接复用 `output_json`。
3. 非幂等动作的双重防御机制：在真正触发不可逆副作用前，必须利用 `idempotency_key` (复用 ADR-0059 生成规则) 走 Outbox 级别的校验路径，双保险防止重放。
4. 在 Reaper 的恢复路径中，通过注入已经 `done` 的节点集合，仅对失败或未执行的节点续跑。

## Consequences
- 执行阶段崩溃时，Agent 可以断点续传，不重跑已成功执行的节点，提高了长程任务和包含高风险副作用节点的系统鲁棒性。
- 对性能有轻微的写库开销（在节点执行前后进行 UPSERT），但在长程大粒度 Node 层面可以忽略不计。
- 本决策正式补齐了 ADR-0057 "保守跳过 execute 阶段" 的局限性，而不是推翻它。
