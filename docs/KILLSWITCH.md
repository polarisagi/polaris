# KILLSWITCH — 紧急停止协议

## 激活

向 polaris 进程发送 `SIGUSR2` 信号，或向 `/_admin/kill` 发送 POST 请求。

## 三阶段升级

### 阶段 1：节流 (THROTTLE)
- 最大并发数减半
- 拒绝新的代理（agent）会话
- 允许进行中的任务完成（最多 30 秒宽限期）
- 触发条件：代币消耗率 (Token_Burn_Rate) > 持续 60 秒达到 P95 的 2 倍

### 阶段 2：暂停 (PAUSE)
- 挂起所有进行中的代理任务
- 将挂起的事件刷新到事件日志 (event log)
- 保持会话打开（不接受新消息）
- 触发条件：阶段 1 在 60 秒内未解决，或者可用内存 < 512MB

### 阶段 3：完全停止 (FULL STOP)
- 终止所有代理 goroutines
- 将检查点写入事件日志
- 关闭所有沙箱 (sandboxes)
- 退出进程（退出码 1）
- 触发条件：手动管理员命令，或 OSMemoryGuard 突破临界阈值

## 恢复

发送 `POST /_admin/unseal`（需 `Authorization: Bearer <POLARIS_API_KEY>`，请求体含 `reason` 字段）触发进程内恢复，无需重启进程；恢复动作会写入审计日志。

## 不可侵犯

此协议**不能**被任何代理或 LLM 输出绕过。作为编译时常量实现。
