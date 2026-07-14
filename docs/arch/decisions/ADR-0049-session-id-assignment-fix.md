# ADR-0049: 修复 sCtx.SessionID 从未赋值的根因 Bug（founding_anchor 生产接线前置条件）

- **状态**: Accepted（已执行）
- **日期**: 2026-07-14
- **决策者**: MrLaoLiAI
- **相关模块**: M04（`fsm.StateContext`）/ M09 §2.5（founding_anchor）/ M12（TrajectoryRecorder）

## 上下文

排查 M9 免疫系统 `founding_anchor` 漂移检测（M09-Self-Improvement-Engine.md §2.5）为何长期建立在空数据上时，追踪到比 founding_anchor 本身更深的根因：`internal/agent/agent.go` 的 `NewAgent` 构造 `fsm.StateContext` 时只设置了 `AgentID`，`SessionID` 字段自始至终从未被赋值，恒为空字符串。

影响面比 founding_anchor 单点更广，因为 `SessionID` 是多处写路径的 key 前缀组成部分：

1. `events:session:{sessionID}:{unixnano}` KV key 方案（`cmd/polaris/boot_events.go` 的 `storeEventWriter.writeEvent` 写入，`harness.TrajectoryRecorderImpl.Record(ctx, sessionID)` 消费）——`sessionID` 恒空导致所有会话事件全部塌缩进同一个 `events:session::` 空前缀桶，`TrajectoryRecorder` 按真实 session ID 查询时永远查不到数据。
2. `tokenVault.ClearTask`、记忆巩固 outbox 触发事件、`withTaskScopeCtx` 的 ctx 任务域注入、PII 快照的 `session_id` 字段——排查确认这些分支此前均依赖非空 `SessionID` 才能进入预期逻辑，此前处于静默降级/跳过状态。

修复前经研究子代理专项验证：确认生产代码中没有任何地方把 `SessionID == ""` 当作有意义的哨兵值使用（例如"未关联会话"的显式判断），排除了"直接赋值会破坏现有语义"的风险。

## 决策

**`NewAgent` 构造 `StateContext` 时补 `SessionID: id`（与 `AgentID` 同源，both 取自调用方传入的会话/构造 ID）。**

依据：

1. 本仓库当前 Agent 生命周期模型中，`AgentID` 与会话 ID 是同一个值（per-session Agent 实例，非跨会话复用），因此直接复用构造参数 `id` 赋给两个字段是符合现状语义的最小修复，不引入新的 ID 生成/传递机制。
2. 这是"发现并修复一个此前从未被注意到的、贯穿多个消费模块的严重 Bug"，符合本仓库 ADR 记录惯例中"影响面广、后人可能重新踩坑"的记录门槛（先例：ADR-0027 BUG-1~4）。
3. 该修复是 founding_anchor 漂移检测（ADR 记录见 M09 §2.5 设计文档新增说明）能够拿到真实轨迹数据的前置条件——若不修复 `SessionID`，即使 founding_anchor 的读侧逻辑再完整，也永远建立在空数据上。

## 后果

- **正向**: `events:session:{id}:` 前缀查询自本次修复起对每个会话真正生效；`founding_anchor` 漂移检测、记忆巩固触发、PII 快照 `session_id` 字段等此前静默降级的分支全部恢复预期行为，无需额外改动。
- **负向**: 修复前所有历史写入的 `events:session::` 空前缀数据在语义上是"已知污染但不可逆"的存量脏数据，无法通过重新计算恢复出真实 session 归属；本 ADR 不包含历史数据回填方案。
- **反例守护**: 未来如有人排查"founding_anchor / TrajectoryRecorder 查不到数据"类问题，先确认 `sCtx.SessionID` 是否真的非空，而不是默认假设读侧逻辑有 Bug——本 ADR 记录的教训是根因常在更上游的构造路径，而非最终消费点。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 只修 founding_anchor 消费端，让它自己重新构造/查询 session ID | 治标不治本，`tokenVault.ClearTask`/记忆巩固/PII 快照等其余消费方仍会继续静默降级；根因在构造侧，应一次性修复 |
| 引入独立的 SessionID 生成器，与 AgentID 解耦 | 当前 Agent 生命周期模型中两者本就同源，人为解耦是不必要的复杂化，且没有已识别的需要二者不同的真实场景 |

## 引用代码

- `internal/agent/agent.go`（`NewAgent`，`sCtx: &fsm.StateContext{SessionID: id, ...}`）
- `internal/agent/agent_session_id_test.go`（`TestNewAgent_SessionIDPopulated`/`TestNewAgentWithDefaults_SessionIDPopulated`）
- `cmd/polaris/boot_events.go`（`storeEventWriter.writeEvent`，`events:session:{id}:` 写入侧）
- `internal/eval/harness`（`TrajectoryRecorderImpl.Record`，读取侧）
- `docs/arch/M09-Self-Improvement-Engine.md §2.5`（founding_anchor，本次同步补充设计说明）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-14 | 初稿，记录 SessionID 根因 Bug 修复与影响面排查结论 |
