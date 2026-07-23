# ADR-0065: S_REPLAN 扩展激活重试与降级标记（A-3）

## 状态
Accepted（已执行，回填）

> 本文件为 2026-07-23 复核时回填——原始改动提交于本轮 35 个提交中的 `63cced3
> feat(agent): add S_REPLAN extended activation degraded flag (A-3)`，但当时未
> 同步创建对应 ADR 文件（`local_playground/upgrade/UPGRADE-PROMPT-ADVANCED.md`
> 已明确要求本项落码时同步写 `ADR-0065`）。本文件按实际代码复核后回填，内容以
> 代码现状为准。

## 背景 (Context)
`internal/agent/fsm/state_machine.go` 的 `S_REPLAN` 分支此前用 `SafeGo` 异步调用
`sm.activator.FindAndActivate` 按需激活扩展，超时（`replanExtActivationTimeout`，
默认 3s）或失败仅 `slog.Warn` 后照常 dispatch `TriggerReplanDone`——无重试、无降
级信号。Agent 因此可能在扩展未真正激活、工具仍缺失的情况下强行重规划，大概率
再次命中 `capability_gap` 并空转。

## 决策 (Decision)
不新增 FSM 状态（不动 `state.yaml` 状态数，避免联动 Phase D 的状态数统一工作），
改为"重试 + 降级标记"：

1. 新增 `StateMachine.activateExtWithRetry(ctx, goal) ([]ExtActivatedHint, bool)`
   （`state_machine.go:451`），内部对 `FindAndActivate` 做有限重试，重试耗尽后
   通过 `metrics.GlobalReplanExtActivationDegradedTotal.Add(1)` 计数
   （`state_machine.go:473`，指标定义见 `internal/observability/metrics/metrics.go:34-35`）。
2. `sessionCtx`（`state_machine.go:149-150`）新增字段：
   ```go
   // ReplanExtActivationDegraded 标记本轮 S_REPLAN 的按需扩展激活已降级
   // （超时/失败且重试耗尽）。上层重规划据此对 capability_gap 快速失败，
   // 避免缺工具空转。
   ReplanExtActivationDegraded bool
   ReplanExtActivationAttempts int
   ```
   降级发生时在 S_REPLAN 分支加锁写入 `sCtx.ReplanExtActivationDegraded = true`
   （`state_machine.go:393-398`），随后仍照常 dispatch `TriggerReplanDone`（不阻塞
   FSM 主流程，符合 HE-5 状态机持控制流）。
3. 消费点：`internal/agent/agent_execute_dag.go:364-368`——DAG 执行遇
   `"tool not found"` 错误时，若 `a.sCtx.ReplanExtActivationDegraded` 为真，直接
   `apperr.Wrap(apperr.CodeInvalidInput, "capability_gap with extension activation
   degraded", err)` 快速失败并给出明确原因，而非放任 Agent 继续空转重试。

## 结果 (Consequences)
- **正面**：扩展激活失败不再被静默吞掉，重规划链路在真正无望的场景下能快速止损，
  避免无谓的 LLM 调用与步数消耗；降级次数可观测（Prometheus 计数器）。
- **负面**：重试引入的额外延迟受 `replanExtActivationTimeout` 预算封顶控制，正常
  场景无感知；异常场景下用户会看到更早的失败而非更多次重规划尝试——这是设计
  的预期取舍（快速失败优于空转）。
- **验证**：`internal/agent/fsm/state_machine_ext_test.go` 覆盖恒超时（断言降级
  标记+指标 +1）与首次失败次次成功（断言无降级标记）两类场景，均通过。
