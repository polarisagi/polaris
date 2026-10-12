# ADR-0007: TaintLevel 五级 + 只升不降 + Sanitizer 受控降级

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0045/0047 ｜ 模块：M11 `internal/security/taint/`
> 实现详情：[M11 §2.3-2.5](../M11-Policy-Safety.md)、[00-Dict §4](../00-Global-Dictionary.md)

## 决策一：五级传播 + 四种受控降级

`None=0`/`Low=1`/`Medium=2`（LLM 摘要硬地板）/`High=3`/`UserReviewed=4`；自然传播 `output = max(inputs)`，只升不降。降级仅四条受控路径：
- 模式验证 → None
- LLM 摘要 → Medium（硬地板）
- 确定性转换 → 降一级
- 用户确认 → UserReviewed

## 决策二：保留五级不简化（原 ADR-0045）

GD-13-004 否决简化提案、GD-14-003 重申采纳：三级或 Boolean 方案无法表达 LLM 摘要中间态。

## 决策三：taint_sanitizer 二级降级接入 S_VALIDATE（原 ADR-0047）

复用既有 `ExemptionVault`，不新建存储。四个降级函数中 `SanitizeByDeterministicTransform` 经 [ADR-0062](./ADR-0062-deadcode-final-settlement.md) 复核恢复保留（刻意不接入 S_VALIDATE，无统一可验证触发点，deadcode 白名单豁免），其余三个已生产接线。

## 反例守护 / 已驳方案

- 对 LLM 输出做 keyword/regex 过滤即降级 — 概率过滤非物理边界
- 按 Provider 信任度降级 — 不能消除结构化注入风险

## 重新评估触发条件

重提简化方案须证明五级粒度在生产中从未被用到（如 Medium 硬地板与 High 从未产生不同处置分支）。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版 |
| 2026-07-28 | 合并 ADR-0045/0047 |

## 引用代码

`internal/security/taint/taint.go`、`internal/security/taint/taint_sanitizer.go`
