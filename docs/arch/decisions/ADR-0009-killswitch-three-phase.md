# ADR-0009: KillSwitch 三阶段熔断 + `.fullstop` 持久状态防重启循环

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M11 / `pkg/substrate/killswitch.go`
- **实现详情**: [M11 §4 KillSwitch FSM](../M11-Policy-Safety.md) | [00-Dict §4 KillSwitch + §1-ter XR-01](../00-Global-Dictionary.md) | [DIAGRAMS §2 KillSwitch 触发链](../DIAGRAMS.md)

## 上下文

LLM 系统失控模式:成本爆炸(TokenBurnRate 持续高位)/ 持续失败 / 状态机异常。仅用进程级 panic 会被守护进程重启重新触发 → fork-bomb。需要渐进降级 + 持久止损 + 显式人工恢复。

## 决策

**三阶段 KillSwitch + `.fullstop` 持久状态文件 + 密封模式人工解锁。**

三阶段触发阈值(详见 [M11 §4](../M11-Policy-Safety.md) + [00-Dict §4 KillSwitch](../00-Global-Dictionary.md)):
- **Stage 1 THROTTLE**: `EMA_5s > baseline.P95 × 2.0` → 限流
- **Stage 2 PAUSE**: `EMA_30s > baseline.P95 × 3.0` → 暂停新任务,保留处理中
- **Stage 3 FULLSTOP**: `EMA_30s > baseline.P95 × 10.0` 或连续 10 次安全防线失败 → 写 `.fullstop` + 密封模式

恢复路径:Stage 1/2 自动回落;Stage 3 仅经 `POST /_admin/unseal` 显式人工恢复并删 `.fullstop`。守护进程重启时检测 `.fullstop` → 直接密封(**禁自动 unseal**)。

阶段变迁唯一触发点 = M11 KillSwitch FSM(由 M3 推送 TokenBurnRate Gauge,FSM 切换由 M11 唯一执行,见 [XR-01](../00-Global-Dictionary.md))。

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| 单一 KILL(无渐进) | 缺少限流缓冲;误杀率高 |
| 仅内存状态 | 重启即失效;不防 fork-bomb |
| 时间窗自动恢复(如 1h 后解锁) | 持续失控时陷入循环;与"显式人工干预"哲学矛盾 |
| 多组件独立判定阶段 | 状态机分裂;阶段不一致导致部分模块仍在跑 |

**反例守护**:
- 未来如有人提议"加自动 unseal 路径"—本 ADR 拒绝。任何自动恢复路径在持续失控时陷入循环
- 未来如有人提议"M4/M8/M13 各自判定 KillSwitch 阶段"—违反 XR-01,本 ADR 与 XR-01 联合拒绝
