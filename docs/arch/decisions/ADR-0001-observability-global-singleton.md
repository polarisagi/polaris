# ADR-0001: observability 一等公民指标使用包级全局变量（R1.3 豁免）

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M3 `internal/observability`

## 决策

`GlobalSurpriseIndex` / `GlobalKillswitchStage`（[ADR-0007](./ADR-0007-taint-level-five-tier.md) 控制信号）保留包级全局单例，显式豁免 `docs/specs/00-Constitution.md R1.3`。

## 豁免边界（四条须同时满足，任一不满足必须遵守 R1.3）

1. 已在 `00-Global-Dictionary §3` 登记的一等公民指标，或经 ADR 明确豁免的全局控制信号
2. 全程序生命周期，无生命周期管理需求
3. 内部 `atomic`/`sync.Mutex` 守护并发安全
4. 提供 `NewXxx()` 构造函数，测试可构造独立实例

## 后果

依赖注入需全链路（M1/M4/M11/M13）接口签名膨胀，选择全局单例保持调用点简洁；测试隔离由 `NewXxx()` 保证不受影响。未来任何"为方便访问加全局变量"的提议，四条边界缺一不可，不得比照扩大。

## 引用代码

`internal/observability/metrics/metrics.go`
