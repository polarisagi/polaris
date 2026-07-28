# ADR-0012: state.yaml ↔ Go 代码一致性回归测试设计

- **状态**: Accepted（已执行完毕）| **日期**: 2026-05-16 | **模块**: 全系统级 / `internal/protocol`

## 决策

新增 `internal/protocol/spec_consistency_test.go`，CI 强制门控守护 [ADR-0006](./ADR-0006-state-yaml-ssot.md) SSoT 一致性。**采用机制 A：显式断言映射**（非反射，非代码生成）——测试加载 `state.yaml` 反序列化后与 Go 枚举/常量精确对照，双向集合等值。

Tier 1（必测，CI fail-closed）：M4 状态枚举/转移、TaintLevel 五级、KillSwitch 三阶段。Tier 2（warning）：Blackboard 时间窗等数值阈值。

不用反射（无 stringer 工具链，维护成本更高）；不用代码生成（编辑 state.yaml 后必须跑生成器才能编译，IDE 路径复杂）。

## 反例守护

拒绝为减少维护成本移除本测试——测试维护成本就是 SSoT 守护的物理体现。拒绝用宽松断言替代精确等值——会让漂移重新成为可能。

## 引用代码

`internal/protocol/spec_consistency_test.go`
