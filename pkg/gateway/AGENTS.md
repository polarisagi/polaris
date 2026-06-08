# pkg/gateway/ (L3 对外网关: HTTP API / WebUI / 多第三方接入适配)

> Canonical arch doc: [M13-Interface-Scheduler.md](../../docs/arch/M13-Interface-Scheduler.md)

**硬约束**:
1. 绑定地址: 默认 127.0.0.1, 远程绑定需显式配置 + TLS + capability + audit
2. LLM 调用: 禁绕过 Provider 直接构造 HTTP; 必走 inference.ProviderRegistry (XR-09)
3. DB 写路径: 业务写入走 MutationBus; CAS 等需原子性的场景走 Store.Put/Txn (XR-04)
4. 出站网络: 禁裸 http.Client, 走 SafeHTTPClient / SafeDialer (XR-06)
5. 安装入口: 写 mcp_servers/extension_instances 必经 marketplace.Manager.InstallExtension (R1.14)
6. 依赖单向: 可引 L0/L1/L2; 禁 import pkg/edge 内部实现（HITL 走 protocol.HITL 接口）

**高频陷阱**:
- ambient skill 注入: injectSystemPrompt() 负责; 上限 4000 字符, 超限按 trust_tier 降序截断
- 工具懒加载: 已安装工具 >40 时只暴露 builtin(trust_tier=4) + search_tools
- Shell Hook 输出: TaintLevel=High, 必经 M11 PolicyGate 再注入 Agent 上下文
- M9 激活 Prompt: 从 prompt_versions 表读取, Activate 回调热更新 (activatedSystemPrompt)
- Cron: 生命周期由 cronCancel 独立控制, 禁在 HTTP handler 内阻塞等待

**文件索引**:
- [标杆] `server/server.go`: Server 结构体 + 路由注册 (M13 对外网关主入口)
- [参照] `server/context.go`: 请求上下文装配
- [参照] `server/sessions.go`: 会话生命周期管理
- [参照] `server/mcp_servers.go`: MCP 安装/启停 API
- [参照] `server/plugin_catalog.go`: 插件市场安装 API
- [参照] `server/cron.go`: Automation/Cron 任务调度
- [参照] `server/sse.go`: SSE 流式推送
- [参照] `server/openai_compat.go`: OpenAI 兼容接口
- [参照] `channels/manager.go`: 多第三方接入 poller 管理 (Telegram/Slack/Discord 等)
- [参照] `channels/dispatch.go`: 消息分发路由

**跨模块**:
- 调用 kernel.Agent (cognition), inference.ProviderRegistry (substrate), mcp.MCPManager (extensions)
- HITL 审批经 protocol.HITL 接口 → pkg/edge/hitl
- 接口/路由变更视同 B5 破坏性变更
