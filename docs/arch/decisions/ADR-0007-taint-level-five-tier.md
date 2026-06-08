# ADR-0007: TaintLevel 五级 + 只升不降 + Sanitizer 受控降级

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M11 / `pkg/substrate/taint.go`
- **实现详情**: [M11 §2.3-2.5](../M11-Policy-Safety.md) | [00-Dict §4 TaintLevel/Taint-Prop/Taint-Sanitizer](../00-Global-Dictionary.md)

## 上下文

LLM 输出可能含 prompt injection / 跨语言编码混淆。完全禁止 LLM 输出进特权操作不现实,完全信任则不安全。需要量化"数据置信度"机制——能表达"半信任"、防概率过滤被当物理边界、与 Cedar 集成。

## 决策

**五级 TaintLevel + 只升不降自然传播 + 四种 Sanitizer 受控降级。**

五级 + 传播规则 + Sanitizer 路径完整定义见 [00-Dict §4](../00-Global-Dictionary.md):
- 五级: `None=0` / `Low=1` / `Medium=2`(LLM 摘要硬地板) / `High=3` / `UserReviewed=4`
- 自然传播: `output = max(inputs)`,只升不降
- 受控降级路径: 模式验证(→None) / LLM 摘要(→Medium 硬地板) / 确定性转换(降一级) / 用户确认(→UserReviewed)

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| 3 级(clean/tainted/critical) | 粒度不足;无法表达 LLM 摘要中间态 |
| Boolean(clean/tainted) | 无法表达任何中间态;过度简化 |
| 任意 Sanitizer 路径自由降级 | 概率过滤会被误用为物理边界 |
| 单向只升、无降级路径 | 数据永远只升,系统僵化 |

**反例守护**:
- 未来如有人提议"对 LLM 输出做 keyword/regex 过滤就降到 Low"—本 ADR 拒绝。keyword/regex 是概率过滤,非物理边界
- 未来如有人提议"信任度高的 Provider 输出可降为 Low"—本 ADR 拒绝。Provider 信任度不能消除结构化注入风险
