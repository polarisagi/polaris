# ADR-0002: skill 子包内本地接口/类型消除（R1.4 合规）

- **状态**: Accepted（已执行完毕 2026-05-16）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M6 / `pkg/cognition/skill`

## 上下文

`pkg/cognition/skill/skill.go` 同时定义两套并行的 Registry 接口：

```go
// 本地接口（39-47 行）
type Registry interface {
    Register(ctx context.Context, skill *Skill) error
    LookupByName(ctx context.Context, name string) (*Skill, error)
    // ... 使用本地 *Skill 类型
}

// protocol 实现（72-184 行）
var _ protocol.SkillRegistry = (*RegistryImpl)(nil)
// 使用 protocol.SkillMeta 类型
```

外部代码 `grep` 结果（`skill\.Registry|skill\.RegistryImpl|skill\.Skill\b|skill\.NewSkill|skill\.LogicCollapse|skill\.Trajectory`）**零外部消费者**——本地 `Registry` / `Skill` / `LogicCollapse` / `Trajectory` / `Step` 接口/类型均为内部死代码。

`RegistryImpl` 内部使用本地 `Skill` 存储 + `metaToSkill` / `skillToMeta` 双向转换，导致：

- 死类型 + 死接口隐藏在 11.6KB 实现文件中
- 字段语义损失：`metaToSkill` 中 `Description` 被硬编码为 `meta.Name`、`Precondition`/`Postcondition` 硬编码为 `"true"`/`"done"`
- 双向同步维护负担：未来 SkillMeta 字段变更需双向适配

违反 `00-Constitution.md R1.4`（接口定义在实现方）。

## 决策

**消除 skill 子包内所有本地接口/类型，统一使用 `protocol.SkillMeta`。**

依据：

- 本地 `Registry` 接口零外部消费，纯死代码——R1.4 违例无任何收益对冲
- 双向转换（metaToSkill / skillToMeta）是纯成本，且导致字段语义损失
- `protocol.SkillMeta` 字段已满足 `RegistryImpl` 所有内部需求（Name/Version/Capabilities/Benchmarks/RiskLevel/Deprecated/SignatureValid）
- 移除后 `RegistryImpl.skills` 由 `map[string]*Skill` 直接改为 `map[string]*protocol.SkillMeta`，无信息损失

## 后果

- **正向**: 消除 R1.4 违例；删除约 200 行死代码；字段语义保真；维护面收缩
- **负向**: 无（已确认外部零消费、测试已对齐）
- **反例守护**: 未来类似"本地接口 + protocol 接口并行"模式直接拒绝。本案显示此模式无任何收益对冲——继续允许将令 R1.4 沦为软规则

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留双接口 + ADR 豁免 R1.4 | 双接口零外部消费，豁免无依据；连续豁免将侵蚀 R1.4 规则力 |
| 反向迁移：把 protocol.SkillRegistry 改用本地 Skill | 违反 R1.4 方向更严重；protocol 是瘦类型集散地，禁止吸收实现细节 |
| 全部移入 protocol 包（含 helpers） | 过度迁移；`riskGT` / `hasCapability` 是包内私有功能，不构成跨模块契约 |

## 引用代码

- `pkg/cognition/skill/skill.go`（重构目标）
- `pkg/cognition/skill/skill_test.go`（测试已对齐 protocol.SkillMeta，无需修改）
- `internal/protocol/interfaces.go §407-443`（SkillRegistry / SkillMeta / SkillSelector 权威定义）
- `docs/specs/00-Constitution.md R1.4`（被维护的规则）
- `docs/specs/07-Reference-Implementation.md`（`pkg/cognition/skill` canonical 行依赖本 ADR）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿；Accepted；代码执行待 B 阶段批准 |
| 2026-05-21 | 执行完毕：skill.go 本地接口/类型全部删除，RegistryImpl 统一使用 protocol.SkillMeta |
