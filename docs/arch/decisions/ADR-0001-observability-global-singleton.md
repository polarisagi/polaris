# ADR-0001: observability 一等公民指标使用包级全局变量

- **状态**: Accepted
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M3 / `pkg/substrate/observability`

## 上下文

`docs/specs/00-Constitution.md R1.3` 禁止 `pkg/` 下定义全局可变变量。

`pkg/substrate/observability/metrics.go` 以包级 `var` 暴露：

```go
var (
    GlobalTokenBurnRate = NewTokenBurnRate()
    GlobalSurpriseIndex = NewSurpriseIndex()
)
```

技术上违反 R1.3 字面规则。决策点：保留全局单例 + 显式豁免，还是改造为依赖注入。

## 决策

**保留全局单例，对一等公民指标显式豁免 R1.3。**

依据：

- TokenBurnRate / SurpriseIndex 是全程序生命周期、跨模块共享的一等公民指标（`docs/arch/00-Global-Dictionary.md §3`）。改造为依赖注入需传递到 M1/M4/M11/M13 全链路，接口签名膨胀，调用点失去简洁
- 两个变量的指针不可变，仅内部状态可变。并发安全由 `atomic.Int64` + `sync.RWMutex` 在结构体内部保证
- 测试隔离不受影响——测试代码可调用 `NewTokenBurnRate()` / `NewSurpriseIndex()` 构造独立实例，全局单例仅供生产路径使用
- R1.3 的设计意图是"避免共享可变状态导致测试隔离失败"。本场景内部并发原语已守护、且测试可构造独立实例，未触犯精神

## 豁免边界（严格限定）

仅当**同时满足**以下四条时，方可使用包级全局单例：

1. 一等公民指标（在 `00-Global-Dictionary §3` 已登记的 SurpriseIndex / TokenBurnRate / 后续显式追加）
2. 全程序生命周期，无生命周期管理需求
3. 内部 `atomic` 或 `sync.Mutex` 守护并发安全
4. 提供 `NewXxx()` 构造函数，测试可构造独立实例

任一条件不满足 → 必须遵守 R1.3。

## 后果

- **正向**: 避免一等公民指标渗透到全部模块构造函数签名；保持调用点简洁；M3 度量推送路径无侵入
- **负向**: R1.3 出现一个有名豁免点，未来类似豁免须显式 ADR 而非比照本 ADR 推广
- **反例守护**: 未来若有人提议"为 X 模块加全局变量方便访问"，本 ADR 不构成支持。豁免限定四条同时满足，缺一不可

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 依赖注入到所有调用方 | 全链路接口签名膨胀，调用点失去简洁；与 Prometheus 风格一等公民指标的业界惯例相悖 |
| Singleton 私有 + Getter 函数 | 等价于全局变量加一层包装，未改变本质，仅增加间接调用成本 |
| 修改 R1.3 字面规则 | 弱化 R1.3 在其他场景下的约束力，得不偿失 |

## 引用代码

- `pkg/substrate/observability/metrics.go`（`GlobalTokenBurnRate` / `GlobalSurpriseIndex` 定义）
- `docs/arch/00-Global-Dictionary.md §3`（一等公民指标定义）
- `docs/specs/00-Constitution.md R1.3`（被豁免的规则）
- `docs/specs/07-Reference-Implementation.md`（observability canonical 行附注 ADR-0001）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted |
