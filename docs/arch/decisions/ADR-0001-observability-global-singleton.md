# ADR-0001: observability 一等公民指标使用包级全局变量（R1.3 豁免）

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 模块：M3 `internal/observability`

## 决策一：控制信号保留包级全局单例，豁免 R1.3

`GlobalSurpriseIndex` / `GlobalKillswitchStage`（[ADR-0007](./ADR-0007-taint-level-five-tier.md) 控制信号）保留包级全局单例，显式豁免 `docs/specs/00-Constitution.md R1.3`。

豁免边界（四条须同时满足，任一不满足必须遵守 R1.3）：
- 已在 `00-Global-Dictionary §3` 登记的一等公民指标，或经 ADR 明确豁免的全局控制信号
- 全程序生命周期，无生命周期管理需求
- 内部 `atomic`/`sync.Mutex` 守护并发安全
- 提供 `NewXxx()` 构造函数，测试可构造独立实例

后果边界：
- 依赖注入需全链路（M1/M4/M11/M13）接口签名膨胀，故选全局单例；测试隔离由 `NewXxx()` 保证。
- 新增豁免须逐条对照四条边界，不得比照已有豁免扩大。

## 重新评估触发条件

四条边界任一被打破（单例失去 `NewXxx()` 致测试无法隔离；需跨进程/跨会话生命周期管理）。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版 |
| 2026-08-09 | 追记重评触发条件 |

## 引用代码

`internal/observability/metrics/metrics.go`
