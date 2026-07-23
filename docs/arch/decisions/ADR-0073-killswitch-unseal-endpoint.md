# ADR-0073: KillSwitch 恢复路径统一 (活恢复模型)

## 状态
Accepted（已执行）

## 背景
在之前的设计中，关于 KillSwitch 的恢复路径存在三份材料互相矛盾的情况：
1. `docs/KILLSWITCH.md` 描述恢复方式为退出进程后重启 Polaris 并重放事件日志。
2. `docs/arch/spec/state.yaml` 描述响应协议包含 `close listener`，但恢复方式又为 `admin_unseal`。
3. `internal/security/killswitch.go` 实际代码既不调用 `os.Exit` 也不关闭 listener，而是支持 `recoveryCallback` 机制进行进程内活恢复，但缺乏对应的触发路由。

此外，由于 Tier-0（2GB VPS）场景通常缺乏自动重启编排，重启进程恢复模式不可控。

## 决策
采纳"进程内活恢复"模型，放弃"重启进程"模型。

1. 新增管理端点 `POST /_admin/unseal`，允许通过有效的 `POLARIS_API_KEY` 触发活恢复。
2. 修复 `internal/gateway/server/middleware_auth.go` 中间件拦截逻辑，放行 `/_admin/unseal` 路径，但不免除鉴权。
3. 优化 `internal/security/killswitch.go` 中的锁使用，将 `writeFullStopFile` 的磁盘 IO 操作移出锁范围。
4. 修订 `docs/KILLSWITCH.md` 和 `docs/arch/spec/state.yaml` 中的过时与矛盾描述。

## 后果
- 统一了文档与代码的实现，消除架构漂移。
- 提升了单人运维场景下故障恢复的便捷性。
- 这是对 ADR-0009 恢复路径细节的补充与修正。
