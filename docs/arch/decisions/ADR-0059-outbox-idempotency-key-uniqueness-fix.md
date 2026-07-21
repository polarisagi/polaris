# ADR-0059：Outbox 幂等键唯一性修复（非 BuildIdempotencyKey 统一迁移）

## 状态

已接受，已实现。

## 背景

deadcode 审计任务 #14 原计划为"将 `pkg/types.BuildIdempotencyKey` 统一迁移到全部
`protocol.NewOutboxEvent` 调用点"。`BuildIdempotencyKey(engine, entityType,
entityID, operation string, version int) IdempotencyKey` 实现完整、有测试，
但零生产调用点（deadcode 标记 `unreachable func`）。

审查全部 13 处真实 `NewOutboxEvent` 调用点后发现：`BuildIdempotencyKey` 的
`version int` 参数不对应任何调用点的真实业务概念——没有一处幂等键构造语义
天然带"版本号"。若强行迁移，等同于为每个调用点臆造一个不存在的"version"
语义，属于 R1（禁止臆测业务语义）违规。

因此本任务的正确形态不是"迁移到 BuildIdempotencyKey"，而是逐一审查每个
调用点现有幂等键的**真实构造是否唯一**，修复真正存在的 bug。

## 核心发现

`internal/protocol/schema/002_outbox.sql` 中 `idempotency_key TEXT NOT NULL
UNIQUE`；`internal/store/outbox_worker.go` 的 `Write()` 是裸 `INSERT`（无
`ON CONFLICT`），约束冲突时返回 error。多个调用点用 `_ = outbox.Write(...)`
丢弃该 error——幂等键一旦不唯一，冲突写入被**静默吞掉**，不是"缺少幂等
保护"这么轻的问题，是"真实事件被悄悄丢弃且无人知晓"。

逐一审查 13 处调用点，确认 7 处存在真实的幂等键退化：

| 文件 | 修复前的键 | 退化条件 | 影响 |
|---|---|---|---|
| `cmd/polaris/boot_agent.go` | 空字符串 `""` | 每次进程启动都相同 | 部署生命周期内仅第一次 killswitch_recovery 事件能写入 |
| `internal/knowledge/ingester.go` | 空字符串 `""` | 每次摄入都相同 | 仅*全局第一份*摄入文档触发 GraphBuild，此后每份新文档静默丢失 |
| `internal/tool/builtin/skill_tools.go` | 空字符串 `""` | 每次触发都相同 | 仅全局第一次显式技能合成触发能到达 GapFillWorker |
| `internal/agent/agent_execute_effect_helpers.go`（exec/perceive/plan/reflect 投影 + consolidate 触发，共 5 处） | `"{sessionID}:{阶段}:{agentID}"` | sessionID/AgentID 在会话全生命周期内不变（`NewAgent`：`sCtx.SessionID = id = AgentID`） | **多轮对话场景下，perceive/plan/exec/reflect 记忆投影与记忆蒸馏触发实际上只有会话第一轮能成功写入 outbox，第二轮起全部因 UNIQUE 冲突被静默丢弃** |
| `internal/agent/agent_execute_util.go`（`writeEpisodicWithExtract` 语义抽取触发） | `ev.ID + ":extract"` | `ev.ID` 由 `fsm.StateMachine.NextEventID` 生成，形如 `"{sessionID}:{seq}:{eventType}"`；`seq`（`sm.eventSeq`）是每个 Agent 实例的私有计数器，新实例重置为 0（**按设计**，满足 inv_M4_02 崩溃恢复重放确定性——见 ADR-0057）；Pool 为同一 sessionID 的每一轮新对话构造全新 Agent 实例，跨轮次的 seq 序列因此重复，`ev.ID` 本身也重复 | 与上一行同一根因的下游连锁：`TopicEpisodicExtract` 语义抽取触发从会话第二轮起同样被静默丢弃 |

其中最严重的是 `agent_execute_effect_helpers.go` + `agent_execute_util.go`
两处：多轮对话是主干场景（不是边缘情况），意味着**几乎所有生产会话从第
二轮起，记忆投影、语义抽取、记忆蒸馏三条链路实际从未被真正触发过**，
是一个隐蔽但影响面覆盖全部核心记忆管线的静默数据丢失 bug。

## 决策

1. **不修改 `NextEventID`/`ev.ID` 本身**：其确定性是崩溃恢复重放（ADR-0057）
   的必要不变量（"同 session+seq → 同 ID，不依赖 wall clock"），不可动。
2. 在 outbox 幂等键**构造这一层**追加真正的唯一性后缀，不触碰上游语义。
3. 新增 `outboxUniqueSuffix()`（`internal/agent/agent_execute_effect_helpers.go`）：
   纳秒时间戳 + 进程内单调原子计数器 `outboxSeqCounter`。单独使用
   `time.Now().UnixNano()` 不足以保证唯一——同一 goroutine 内背靠背调用
   （reflect 投影紧接着触发 consolidate）可能落在同一时钟粒度内返回相同值
   （已被 `TestOutboxUniqueSuffix_UniqueUnderTightLoop` 实测命中并修复）。
4. 所有调用点均为同步执行一次、无重试语义，因此只需保证"不与历史记录
   冲突"，不需要引入跨 Agent 实例的全局单调序号或更复杂的幂等状态机制。
5. `types.BuildIdempotencyKey` 保持不动、继续 DEFER：其 `version int`
   参数不适配任何真实调用点，强行迁移属于臆造语义，违反 R1。

## 验证

- `go build ./...`、`go vet ./...`（仅剩预置的 FFI `unsafe.Pointer` 警告）
- `make lint`：主 lint + wasip1 子 lint 均 `0 issues`
- `go test ./...`：全量通过
- `go test -race ./internal/agent/...`：通过
- `deadcode ./cmd/polaris/...`：`outboxUniqueSuffix`/`outboxSeqCounter` 未被
  标记（确认已接入调用链）；`BuildIdempotencyKey` 仍标记 `unreachable`
  （符合"刻意 DEFER"预期，非遗漏）
- 新增测试：
  - `TestRecordLLMFillEffectMemory_OutboxIdempotencyKeyUniqueAcrossTurns`：
    模拟同一 sessionID 两轮对话各触发一次 `S_PERCEIVE_DONE`，断言两轮产生
    的全部 outbox 幂等键互不相同
  - `TestRecordLLMFillEffectMemory_ReflectAndConsolidateIdempotencyKeysUniqueAcrossTurns`：
    同上，针对 `S_REFLECT_DONE`（含 reflect 投影 + 语义抽取 + consolidate
    触发三条写入）
  - `TestOutboxUniqueSuffix_UniqueUnderTightLoop`：1000 次紧邻调用无重复

## 影响范围

`cmd/polaris/boot_agent.go`、`internal/knowledge/ingester.go`、
`internal/tool/builtin/skill_tools.go`、
`internal/agent/agent_execute_effect_helpers.go`、
`internal/agent/agent_execute_util.go`。均为幂等键构造逻辑的局部修复，
不涉及 outbox 消费端（Worker/Handler）或 schema 变更。
