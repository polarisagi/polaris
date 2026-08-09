# ADR-0087: 降级必须显式——三个 fail-closed 实例 + 通用规则

- **状态**: Accepted（已执行）| **日期**: 2026-08-01 | **模块**: M7 `internal/sandbox/`、M11 `internal/security/policy/`、M11 `internal/security/guard/`

## 上下文

阶段03（R-01/R-02/R-05）review 中反复出现同一类问题：某条件不满足时代码悄悄切换到另一条路径，但切换本身不可观测（无 counter、无 Error 级日志、无配置开关），使得"系统正在按降级模式运行"这一事实对运维/未来维护者不可见。本 ADR 把三个独立发现的具体实例统一到一条通用原则下，作为横切沉淀。

## 决策

**三个 fail-closed 实例**：

1. **沙箱可信来源 InProcess 回退需显式 opt-in**（阶段03 R-05）：可信来源（TrustOfficial 及以上）在 Wasm/Container/Remote 均不可用时，默认**不**允许降级到 InProcess 执行（`allow_trusted_inprocess_fallback=false`）。可信 ≠ 稳定，可信来源代码仍可能死循环/内存爆炸/panic。完整决策见 `ADR-0008-sandbox-three-tier-platform-fallback.md` 决策六（本 ADR 不重复承载，避免同一决策两处维护）。

2. **cedarLeaks 用时间窗而非累计阈值**（阶段03 R-01）：`internal/security/policy/gate.go` 的 Cedar FFI 泄漏计数改为滑动时间窗（`cedarLeakWindow=30分钟`）内的有效泄漏数，而非进程生命周期累计值——累计值会把"进程存活很久、早期偶发几次泄漏"误判为"持续故障"并触发不必要的 KillSwitch。窗口内达到 `cedarLeakKillSwitchThreshold=5` 才触发 KillSwitch；`cedarLeaksTotal`（只增不减）单独暴露为 Prometheus Gauge `polaris_cedar_ffi_leaks_total` 供长期趋势观测，两个计数器职责分离，互不干扰判定逻辑。窗口内淘汰采用惰性衰减（每次记录时顺带清理过期项），不引入额外后台 goroutine。

3. **PII 映射 LRU + 分区回收**（阶段03 R-02）：`internal/security/guard/pii_desensitizer.go` 的 original→fake 映射表原为无界增长（`Clear()` 全仓生产代码零调用，唯一调用在测试）。改为按分区（通常=SessionID）的有界 LRU：单分区 `piiMappingMaxEntries=10000` 条，超限淘汰最久未使用映射（一致性仅在窗口内保证，被淘汰的原值再次出现会得到新假值）；同时保留的分区数上限 `piiPartitionMaxEntries=256`，超限整体淘汰最久未用分区。无法提供 SessionID 时落入 `piiGlobalPartition="global"` 兜底分区，仍受条目数上限约束但无法被 `ReleasePartition` 精确回收。淘汰事件按 `evictLogSampleN=100` 采样打 Warn 日志（避免长会话刷屏），但 `metrics.RecordPIIMappingEviction` 计数器每次淘汰都记录，不采样。

**通用规则**：任何 fallback 必须有 counter + 明确日志，禁止静默。新代码引入任何降级路径前，必须先回答："谁能看见它发生了？"——如果答案是"没有人，除非翻源码"，该降级路径不合格。

## 反例守护

本轮发现的全部"静默降级"实例，作为反面样本：

```
- sandbox_router.go 可信来源 Wasm 不可用 → 静默 InProcess（findings GR-6-002）
- ollama StreamInfer 静默跳过 TokenBurnRate（findings Batch 2 #4）
- summary_gen 静默写 taint_level=0（findings GR-7-007）
- 22 处 `_ =` 静默吞没（findings Batch 1/5/6/7/9）
共同模式：降级路径没有 counter、没有 Error 级日志、没有配置开关。
新代码引入任何 fallback 前，必须先回答：谁能看见它发生了？
```

## 后果

- **正向**：三处降级路径从"不可观测"变为"有 counter + 有日志 + 可配置"，运维可通过 Prometheus 告警提前发现降级正在发生，而非事后翻日志排查。
- **负向**：新增少量计数器与配置项维护面（`cedar_gate`/`pii`/`sandbox` 三个 state.yaml 阈值组，见阶段06 §6 登记）。
- **反例守护**：拒绝任何"静默切换执行路径/静默降低安全等级/静默丢弃错误"的实现——凡符合上述反例列表中任一模式的新代码，审查时一律驳回，除非同时补上 counter + Error 日志 + （如适用）显式开关。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| cedarLeaks 继续用进程生命周期累计值，只是提高阈值 | 治标不治本——长期运行进程仍会在累计值缓慢逼近阈值后触发误判性 KillSwitch，时间窗才是正确的语义（"最近是否密集失败"而非"历史上失败过多少次"） |
| PII 映射表不设上限，定期整表 `Clear()` | `Clear()` 会清空所有分区（含仍活跃会话），且原计划的调用点从未真正接入生产路径；分区级 LRU 才能做到"只回收真正过期的会话，不影响活跃会话" |
| 三个实例各自独立成 ADR | 三者虽分属不同模块，但共享同一条"降级必须显式"的治理原则，合并为一份横切 ADR 更利于未来同类审查直接引用（README 合并原则：紧耦合决策合入锚点文件） |

## 引用代码

- `internal/sandbox/sandbox_router.go`（可信来源降级判定，决策承载于 ADR-0008 决策六）
- `internal/security/policy/gate.go`（`cedarLeakWindow`/`cedarLeakKillSwitchThreshold`/`recordCedarLeak`/`cedarLeaksTotal`）
- `internal/security/guard/pii_desensitizer.go`（`lruMapping`/`piiMappingMaxEntries`/`piiPartitionMaxEntries`/`piiGlobalPartition`/`evictLogSampleN`）
- `local_playground/upgrade/98-rejected-findings.md`（相关驳回条目交叉索引）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-01 | 初稿，阶段06 ADR 归档补录（对应阶段03 R-01/R-02/R-05 已落地实现） |
| 2026-08-09 | 追记：重新评估触发条件——任何新降级路径引入前须先回答"谁能看见它发生了"；`cedarLeaks` 提高阈值而非用时间窗、PII 映射表去上限改整表 `Clear()`，两个被驳回方案的重提须先证明本 ADR 记录的原始驳回理由（误判性 KillSwitch / 影响活跃会话）已不成立。 |
