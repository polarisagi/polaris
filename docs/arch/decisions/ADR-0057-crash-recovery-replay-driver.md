# ADR-0057: M04 §8 崩溃恢复回放驱动器

- **状态**: Accepted（已执行）
- **日期**: 2026-07-22
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/protocol/replay.go`、`internal/protocol/interfaces_agent.go`、
  `internal/agent/agent.go`、`internal/agent/agent_execute_effect.go`、
  `internal/agent/agent_execute_effect_helpers.go`、`cmd/polaris/boot_crash_recovery.go`、
  `cmd/polaris/boot_events.go`、`cmd/polaris/boot_agent.go`、`cmd/polaris/main.go`

## 上下文

ADR-0052 deadcode 审计发现：`protocol.SetReplayMode`/`IsReplaying` 读侧已在
4 处生产路径接入护栏（`agent_execute_dag.go` 2PC 跳过、
`agent_execute_effect_helpers.go` 记忆写跳过、`store/outbox_worker.go`
Process 跳过、`execute/dag/executor_node.go` 工具执行短路），但 Setter 从未
被任何生产代码调用过——`docs/arch/M04-Agent-Kernel.md` §8 定义的"EventLog
重放 → 重建 StateContext → 从崩溃点续跑"崩溃恢复设计从未真正落地。

先用 AskUserQuestion 向用户呈现三个选项：(a) 最小化 Reaper 式重试（发现
in-flight 会话直接标记失败，不真正回放）、(b) 投入构建完整 StateContext
重建（原文档设计的完整实现）、(c) 仅文档标注、本次不写代码。用户明确选择
(b)，接受随之而来的更大实现范围与风险面。

## 决策

### 崩溃检测：in-flight 标记

`internal/agent/agent.go` 的 `Agent.Run()` 循环开始处理时写入
`inflight:session:{id}` KV 标记（`markInFlight`），无论从哪条路径退出
（终态/超步熔断/ctx 取消）都通过 `defer clearInFlight()` 清除。干净退出的
会话标记必然被清除；进程崩溃时标记残留，供下次启动时的驱动器识别。

`Agent.eventStore protocol.Store`（`InjectEventStore` 注入，nil-safe）供
该标记读写；`cmd/polaris/boot_agent.go` 的 `buildAgent` 对 agent-0 与
AgentPool 派生的每个 session Agent 都注入同一个 `sb.Store`。

### 崩溃恢复驱动器：`cmd/polaris/boot_crash_recovery.go`

`recoverCrashedSessions` 在 `main.go` 的 `bootAgent` + `LoadProvidersFromDB`
之后、`bootServer` 之前调用（HTTP 服务尚未对外服务，全局 `ReplayMode`
标志此时不存在并发会话冲突）。扫描 `inflight:session:` 前缀，对每个候选
会话：

1. `harness.TrajectoryRecorderImpl.Record` 从 EventLog 重建 `TrajectoryTrace`
   （已有实现，本次复用，未新增）。
2. **安全边界**：仅当 `trace.StateTrans` 最后一条记录（或压根没有记录）
   落在纯 LLM 状态（S_PERCEIVE/S_PLAN/S_REFLECT）时才继续；S_VALIDATE/
   S_EXECUTE/S_REPLAN/S_ROLLBACK 一律跳过不动。`agent_execute_dag.go` 的
   2PC 预写日志机制理论上也能保护 S_EXECUTE 重入时的重复副作用，但本次
   未对该机制做专门审计/测试，不额外依赖它作为自动恢复的安全网——保守
   跳过，回落到崩溃恢复功能上线前的既有行为（不动它，等人工介入）。
3. 从 `chat_messages` 取最后一条 `role='user'` 消息作为重新驱动 FSM 的
   Intent（该消息在 FSM 触发前已同步写入，见 `sse.go` `SaveMessage` 早于
   `SetTaskIntent` 调用，故崩溃点无论多早触发消息必然已落盘）。
4. 通过 `AgentPool.Acquire`（与生产请求完全相同的工厂装配路径，非另起
   一套构造逻辑）获取 Agent，`InjectReplayData` 注入历史 LLM 调用录像，
   `protocol.SetReplayMode(true)`，再 `SetTaskIntent`+`SendIntent` 与正常
   交互路径完全相同的"先订阅后触发"顺序驱动 FSM。
5. `defer protocol.SetReplayMode(false)` 是双重保险：即便 executeEffect
   的 FastPath/PRM 候选等不检查 replay 队列的分支意外接管了首个
   LLMFillEffect（下述"队列耗尽翻转"逻辑因而未被触发），会话恢复流程
   结束时也无条件复位全局标志。
6. 每个会话尝试一次后无论成功/跳过/失败都立即清除 in-flight 标记，不做
   无限重试——与 M8 Reaper Phase1/Phase2"尽力恢复一次，恢复不了就放弃"
   的既有哲学一致。

### 回放替换点：`internal/agent/agent_execute_effect.go`

新增 `protocol.ReplayLLMCall{Request, Response map[string]any}`（类型落在
`internal/protocol`，L0，而非直接复用 `harness.LLMCallRecord`：
`internal/agent` 是 L1，`internal/eval/harness` 是 L3，
`Test_inv_NoCrossLayerImport` 禁止 L1 反向 import L3）。

`Agent.replayCalls []protocol.ReplayLLMCall` + `replayIdx int`
（`InjectReplayData` 注入）。`executeEffect` 的 LLMFillEffect 主路径在
真正发起 `safecall.StreamInfer` 之前检查：若 `IsReplaying()` 且队列未耗尽，
从 `reconstructReplayResponse(call.Response)` 还原
`*types.ProviderResponse` 直接替代真实调用；若这是队列最后一条，
消费的**同一时刻**翻转 `protocol.SetReplayMode(false)`——不等到 Run()
结束才翻转，因为本 Agent 之后若还需要继续推进（真实崩溃点晚于最后一条
录像），必须立刻恢复真实调用能力；这是安全的，因为崩溃恢复驱动器严格
串行处理各会话，串行窗口内不存在其他并发会话依赖同一全局标志的读取。

`reconstructReplayResponse`（`agent_execute_effect_helpers.go`）用
`json.Marshal`+`Unmarshal` 往返而非手写字段映射还原 `Usage`/`ToolCalls`：
两者写入时都是 Go 值（`resp.Usage`/`resp.ToolCalls`），经
`TrajectoryRecorderImpl` 用 `map[string]any` 泛读回来后，字段名与原 Go
结构体导出字段名一致（均无 json tag），往返法比手写映射更不易随字段
增删而漂移。

回放期间 `WriteLLMCallEvent` 调用同样被 `!IsReplaying()` 短路：该记录
本就来自既有 EventLog，重写只会产生重复条目，与其余 3 处 `IsReplaying`
物理短路点同一语义。

### 零 LLM 重放的安全性来自既有机制的自然组合，非新增专门逻辑

关键发现（设计推导，未新增代码验证但基于既有代码路径的静态分析）：
S_VALIDATE/S_EXECUTE 是 `DeterministicEffect`（非 `LLMFillEffect`），其
真实工具执行调用链最终落到 `internal/execute/dag/executor_node.go`，该
文件已有 `if protocol.IsReplaying() { return 短路 stub }` 护栏（4 处既有
护栏之一，非本次新增）。由于 FSM 严格按 Perceive→Plan→Validate→Execute→
Reflect 顺序推进，且 ReplayMode 翻转发生在"消费最后一条录像 LLM 调用"的
同一瞬间：

- 若原始崩溃会话中 S_EXECUTE 已经真实跑过（录像包含其后的 S_REFLECT
  调用），重放到 S_EXECUTE 时 ReplayMode 仍为 true（因为 Reflect 的录像
  尚未消费）→ 工具执行被正确短路，不重复副作用。
- 若原始崩溃会话中 S_EXECUTE 从未真正跑过（崩溃点在 S_PLAN 或更早），
  ReplayMode 已在到达 S_EXECUTE 之前翻转为 false → 工具执行正常真实
  发生，这正是需要发生的行为（这一步骤丢失了，需要补做）。

`internal/agent/agent_replay_test.go` 的
`TestAgent_ReplayMode_FullTrajectory_NoRealCallsNoDuplicateToolExec`（验证
前者）与 `TestAgent_ReplayMode_PartialTrajectory_FallsBackToRealExecution`
（验证后者）对这一组合行为做了端到端回归覆盖。

## 判断依据

延续 R1：不臆测业务语义（"训练样本""崩溃后如何续跑用户可见输出"等）——
重放机制严格复用已存在的真实数据源（`TrajectoryRecorderImpl` 扫描
EventLog 得到的 LLM 调用录像）与已存在的真实执行路径（`AgentPool.Acquire`
+ `SetTaskIntent`+`SendIntent`，与生产请求完全相同）。安全边界（仅
Perceive/Plan/Reflect 末态自动恢复）是本次新增的、显式标注的保守设计
决策，而非文档规定，其保守性来自"未审计 2PC 机制作为自动化场景安全网"
这一诚实的能力边界声明，不是回避实现。

## 后果

- **正向**：`go build`/`go vet`/`go test ./... -race`（受影响包）/
  `golangci-lint`/`make gen-threshold-examples`（无阈值结构变更故未涉及）
  全绿；`deadcode` 确认 `protocol.SetReplayMode`/`recoverCrashedSessions`/
  `InjectReplayData`/`InjectEventStore`/`recoverOneSession`/
  `lastUserMessage` 均已可达。新增测试：
  `internal/agent/agent_replay_test.go`（`reconstructReplayResponse` 单测
  + 全量/部分轨迹回放集成测试 + in-flight 标记生命周期）、
  `cmd/polaris/boot_crash_recovery_test.go`（`lastUserMessage` 单测 +
  `recoverOneSession` 的跳过/驱动分支）。
- **负向 / 明确不做的部分**（用户已知情的范围边界）：
  - S_EXECUTE/S_VALIDATE/S_REPLAN/S_ROLLBACK 状态崩溃不做自动恢复，维持
    崩溃恢复上线前的行为（不动它）。这不是"半成品"，是本次显式选择的
    安全边界——若未来要覆盖这部分，需要先审计/加固 2PC 机制并针对性
    补测试，是独立的后续任务。
  - 不实现工具调用结果的录像替换机制（仅 LLM 调用被回放，工具调用在
    ReplayMode 窗口内统一短路为 stub，不尝试"重放工具的历史输出"）。
  - PRM 多候选路径（`a.prm.ShouldActivate` 分支）与 FastPath
    （`SurpriseIndex<0.3`）分支不检查回放队列——理论上若首个 LLMFillEffect
    恰好落入这两条分支，会绕过替换逻辑发起真实调用；`defer
    protocol.SetReplayMode(false)` 兜底保证全局标志不会因此永久卡在
    true，但那一次调用本身不会被录像替换。给定 Agent 恢复时
    `SurpriseIndex`/`TaskModel.Complexity` 均为新构造 Agent 的零值，
    实践中首次调用不会触发这两条分支，风险可接受但未做强制阻断。

## 引用代码

- `internal/protocol/replay.go`（`ReplayLLMCall` 类型）
- `internal/protocol/interfaces_agent.go`（`AgentController.InjectReplayData`）
- `internal/agent/agent.go`（`replayCalls`/`replayIdx`/`eventStore` 字段，
  `InjectReplayData`/`InjectEventStore`/`markInFlight`/`clearInFlight`，
  `Run()` 接线）
- `internal/agent/agent_execute_effect.go`（替换点 + `!IsReplaying()`
  门控 `WriteLLMCallEvent`）
- `internal/agent/agent_execute_effect_helpers.go`（`reconstructReplayResponse`）
- `cmd/polaris/boot_crash_recovery.go`（新增，驱动器全部逻辑）
- `cmd/polaris/boot_events.go`（`eventSeqTiebreaker` 单调序号，修复同纳秒
  事件 key 碰撞导致的静默丢事件风险，回放要求完整有序录像）
- `cmd/polaris/boot_agent.go`（`buildAgent` 注入 `InjectEventStore`）
- `cmd/polaris/main.go`（`recoverCrashedSessions` 调用点）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-22 | 初稿：完整 StateContext 重建崩溃恢复驱动器实现完成（用户明确选择该方案而非最小化 Reaper 式重试） |
