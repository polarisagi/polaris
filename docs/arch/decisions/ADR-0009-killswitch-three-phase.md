# ADR-0009: KillSwitch 熔断与恢复合集（三阶段熔断 + 进程内活恢复模型，含原 ADR-0072/0073）

- **状态**: Accepted | **日期**: 2026-05-16（恢复路径修正 2026-07-23，合并 2026-07-28）| **模块**: M11 `internal/security/killswitch.go`

## 决策一：三阶段熔断 + `.fullstop` 持久状态防重启循环（原决策）

三阶段（阈值详见 [M11 §4](../M11-Policy-Safety.md)）：Stage1 THROTTLE（`EMA_5s > P95×2.0`）限流 → Stage2 PAUSE（`EMA_30s > P95×3.0`）暂停新任务 → Stage3 FULLSTOP（`EMA_30s > P95×10.0` 或连续 10 次安全防线失败）写 `.fullstop` + 密封模式。

Stage1/2 自动回落；Stage3 仅经人工恢复端点显式解锁（见决策二）并删 `.fullstop`。守护进程重启检测到 `.fullstop` → 直接密封，禁自动 unseal。阶段变迁唯一触发点 = M11 KillSwitch FSM（[XR-01](../00-Global-Dictionary.md)）。

## 决策二：恢复路径统一为进程内活恢复模型（原 ADR-0073，对决策一恢复路径细节的补充修正）

采纳"进程内活恢复"，放弃"重启进程重放事件日志"模型——此前文档（`KILLSWITCH.md`/`state.yaml`）与代码（`recoveryCallback` 机制）三方矛盾，且 Tier-0（2GB VPS）通常缺乏自动重启编排，重启恢复不可控。新增管理端点 `POST /_admin/unseal`（需有效 `POLARIS_API_KEY`，鉴权中间件放行路径但不免鉴权）触发活恢复。`writeFullStopFile` 磁盘 IO 移出锁范围。文档同步订正。

## 反例守护

拒绝任何自动 unseal 路径——持续失控时会陷入循环。拒绝 M4/M8/M13 各自判定阶段——违反 XR-01。拒绝恢复到"重启进程重放事件日志"模型——已证实与 Tier-0 场景及既有代码路径三方矛盾。

## 引用代码

`internal/security/killswitch.go`
