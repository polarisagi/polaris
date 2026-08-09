# ADR-0065: S_REPLAN 扩展激活重试与降级标记（A-3）

- **状态**: Accepted（已执行，回填）| **日期**: 2026-07-23 | **模块**: `internal/agent/fsm/`

## 决策

`S_REPLAN` 分支异步调用 `FindAndActivate` 按需激活扩展，此前超时/失败仅 `slog.Warn`，Agent 可能在工具仍缺失时强行重规划并再次空转命中 `capability_gap`。不新增 FSM 状态（不动 `state.yaml` 状态数），改为"重试+降级标记"：`activateExtWithRetry` 有限重试，耗尽后 `metrics.GlobalReplanExtActivationDegradedTotal.Add(1)` 计数并写入 `sessionCtx.ReplanExtActivationDegraded`，仍照常 dispatch `TriggerReplanDone`（不阻塞 FSM，符合 HE-5）。消费点：DAG 执行遇 `"tool not found"` 时若该标记为真，直接快速失败而非放任空转重试。

## 引用代码

`internal/agent/fsm/state_machine.go`、`internal/agent/agent_execute_dag.go`

> 2026-08-09 追记：重新评估触发条件——若"重试+降级标记"策略在生产中被证明
> 不足以防止空转（如降级标记消费点覆盖不全导致仍有反复重规划案例），先扩大
> 消费点覆盖面，而非默认新增 FSM 状态——`state.yaml` 状态数变更需走独立评估。
