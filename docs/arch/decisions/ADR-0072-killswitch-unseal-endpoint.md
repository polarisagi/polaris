# ADR-0072: KillSwitch 恢复路径统一

## 状态
Accepted（已执行）

## 背景
根据 `01-architecture-designs.md` (B1. KillSwitch 恢复路径统一) 决议：
在原有设计中，关于系统发生紧急停止 (FullStop/sealed) 后的恢复路径，文档描述存在三方矛盾：
1. `docs/KILLSWITCH.md` 描述要求“重启进程”。
2. `internal/security/killswitch.go` 源码实际实现了活恢复模型 (`ManualRecover`/`OnRecovery`)，且无任何 `os.Exit`。
3. `state.yaml` 声明“close listener”，但这与通过 HTTP 管理端点发送 `admin_unseal` 的机制逻辑自反。

由于 Tier-0 场景运维条件有限，采用 HTTP 鉴权端点进行活恢复可以显著降低 MTTR，是更为合理的设计。

## 决策
1. **统一采用进程内“活恢复”模型**，彻底抛弃“重启进程”模型。
2. **新增管理端点 `POST /_admin/unseal`**，作为唯一的安全解封入口。
3. **鉴权与放行**：
   - 中间件放行 `/_admin/unseal`，允许它穿过 Stage 3 拦截。
   - `/_admin/unseal` 强制校验有效 API Key（即使系统允许匿名访问，此端点也不可豁免），仅管理员可调用。
   - 必须记录审计日志（AuditLog）。
4. **锁外化优化**：`KillSwitch.transitionLocked` 仅返回 `needsFullStop` 信号，实际文件写出操作 `.fullstop` 从锁内移至解锁后执行，降低磁盘 IO 对关键锁的阻塞。
5. **文档对齐**：统一修订 `KILLSWITCH.md`、`state.yaml`，修正此前互相冲突的恢复方式描述。

本 ADR 构成对 ADR-0009（三阶段协议）中恢复路径的修正和补充，并不推翻原三阶段的核心协议。

## 后果
- 运维人员可以在保留服务进程运行的情况下，通过 HTTP 端点快速恢复业务。
- 消除了系统恢复期的磁盘 IO 热点锁竞争。
- 确保所有的最高权限解除操作都有强校验和不可篡改的审计追踪。
