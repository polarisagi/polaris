# pkg/edge/ (L3 接口: M13 调度与降级)

> Canonical arch doc: [M13-Interface-Scheduler.md](../../docs/arch/M13-Interface-Scheduler.md)

**硬约束**:
1. 优先级: Priority-0(交互)最高, Priority-3(背景)可被抢占
2. HITL: 所有 ESCALATE 协议必经 HITLGateway, 禁绕过
3. 流量拆分: ProgressiveRollout(1%→100%) 由 M9 决策, M13 仅执行
4. 中断 SLO: POST `/v1/agent/{id}/interrupt` 必 <200ms 传 Cancel 至 M4
5. 资源阈值: 与 OSMemoryGuard 共享 (L1: 1.5G, L2: 1.0G, L3: 512M)
6. 依赖单向: 禁 import L2 内部实现, 仅过 protocol 接口; 禁 import pkg/gateway

**高频陷阱**:
- 所有出站 HTTP 必经 SafeDialer (XR-06)
- Cedar 评估必须在每个外部请求边界
- KillSwitch 阶段仅读 gauge, 禁独立判定变迁
- 根层 `scheduler.go` 已 Deprecated, 禁作标杆

**文件索引**:
- [标杆] `scheduler/scheduler.go`: ResourceGovernor (三级降级)
- [标杆] `hitl/gateway.go`: GatewayImpl (ESCALATE 落地)
- [参照] `scheduler/queue.go`: SQLiteScheduler (即席任务)
- [参照] `cli.go`: CLI 入口 (交互边界)

**跨模块**:
- 读 L0~L2 通过 `internal/protocol/`
- 公开 HTTP/gRPC API 契约变更视同 B5 破坏性变更
