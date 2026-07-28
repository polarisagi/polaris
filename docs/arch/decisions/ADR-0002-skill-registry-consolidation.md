# ADR-0002: skill 子包内本地接口/类型消除（R1.4 合规）

- **状态**: Accepted（已执行完毕）| **日期**: 2026-05-16 | **模块**: M6 `internal/extension/skill`

## 决策

消除 `internal/extension/skill` 内与 `protocol.SkillRegistry`/`protocol.SkillMeta` 并行的本地接口/类型，统一使用 protocol 定义。本地接口零外部消费、且导致字段语义损失（`Description` 硬编码为 `Name` 等），违反 `00-Constitution.md R1.4`（接口定义在实现方）。

## 后果

删除约 200 行死代码，字段语义保真。未来"本地接口 + protocol 接口并行"模式一律拒绝——本案已证明此模式无收益对冲。

## 引用代码

`internal/extension/skill/skill.go`；权威定义见 `internal/protocol/interfaces.go §407-443`
