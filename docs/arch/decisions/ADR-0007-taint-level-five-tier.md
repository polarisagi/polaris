# ADR-0007: TaintLevel 五级 + 只升不降 + Sanitizer 受控降级

- **状态**: Accepted | **日期**: 2026-05-16（合并 ADR-0045/ADR-0047，2026-07-28）
- **模块**: M11 `internal/security/taint/`
- **实现详情**: [M11 §2.3-2.5](../M11-Policy-Safety.md) | [00-Dict §4 TaintLevel/Taint-Prop/Taint-Sanitizer](../00-Global-Dictionary.md)

## 决策

五级 TaintLevel（`None=0`/`Low=1`/`Medium=2` LLM 摘要硬地板/`High=3`/`UserReviewed=4`），自然传播 `output = max(inputs)`，只升不降。四种受控降级路径：模式验证（→None）、LLM 摘要（→Medium 硬地板）、确定性转换（降一级）、用户确认（→UserReviewed）。

## 反例守护

拒绝对 LLM 输出做 keyword/regex 过滤即降级——概率过滤非物理边界。拒绝按 Provider 信任度降级——不能消除结构化注入风险。

## 已确认的降级路径实施（原 ADR-0045/ADR-0047）

- **保留五级传播不简化**（原 ADR-0045，GD-13-004 否决简化提案 / GD-14-003 重申采纳）：三级或 Boolean 方案粒度不足，无法表达 LLM 摘要中间态。
- **taint_sanitizer 二级降级接入 S_VALIDATE**（原 ADR-0047，已执行）：复用既有 `ExemptionVault` 而非新建存储；4 个降级函数中 `SanitizeByDeterministicTransform` 复核后确认恢复接入（[ADR-0062](./ADR-0062-deadcode-final-settlement.md) 复核结果），其余 3 个已生产接线。

## 引用代码

`internal/security/taint/taint.go`、`internal/security/taint/taint_sanitizer.go`
