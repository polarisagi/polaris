# ADR-0009: KillSwitch 三阶段熔断 + 进程内活恢复

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0072/0073 ｜ 模块：M11 `internal/security/killswitch.go`

## 决策一：三阶段熔断 + `.fullstop` 持久状态防重启循环

阈值详见 [M11 §4](../M11-Policy-Safety.md)：
- Stage1 THROTTLE（`EMA_5s > P95×2.0`）限流
- Stage2 PAUSE（`EMA_30s > P95×3.0`）暂停新任务
- Stage3 FULLSTOP（`EMA_30s > P95×10.0` 或连续 10 次安全防线失败）写 `.fullstop` + 密封

Stage1/2 自动回落；Stage3 仅经人工恢复端点解锁并删 `.fullstop`。守护进程重启检测到 `.fullstop` → 直接密封，禁自动 unseal。阶段变迁唯一触发点 = M11 KillSwitch FSM（[XR-01](../00-Global-Dictionary.md)）。

## 决策二：恢复路径为进程内活恢复（原 ADR-0073，修正决策一恢复细节）

放弃"重启进程重放事件日志"：此前文档（`KILLSWITCH.md`/`state.yaml`）与代码（`recoveryCallback`）三方矛盾，且 2GB VPS 通常无自动重启编排。
- 恢复端点 `POST /_admin/unseal`：需有效 `POLARIS_API_KEY`（鉴权中间件放行路径但不免鉴权）；CLI 入口 `cmd/polaris/cli_unseal.go`
- `writeFullStopFile` 磁盘 IO 移出锁范围

## 反例守护 / 已驳方案

- 自动 unseal — 持续失控时陷入循环
- M4/M8/M13 各自判定阶段 — 违反 XR-01
- 重启进程重放事件日志 — 与低配 VPS 场景及既有代码路径矛盾

## 重新评估触发条件

活恢复在生产中被证明不可靠（如恢复端点高负载下不可达）且已排除鉴权/网络因素，才重议重放方案；"无自动重启编排"前提不变则不重议。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版（决策一） |
| 2026-07-23 | 恢复路径修正为活恢复（决策二） |
| 2026-07-28 | 合并 ADR-0072/0073 |

## 引用代码

`internal/security/killswitch.go`、`cmd/polaris/cli_unseal.go`
