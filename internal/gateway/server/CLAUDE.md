# internal/gateway/server — AI 导航索引

> **背景**：此目录含 150+ 文件。Go struct-method 绑定约束（`func (s *Server) ...`）导致顶层处理函数无法拆分为独立子包，因此将**路由处理**按功能域放入 4 个子包，**核心基础设施**留在顶层。
>
> **请通过本文件定位目标，避免执行高代价的 `ls` / `grep` 扫描。**

## 顶层文件（核心基础设施）

| 文件 | 职责 |
|------|------|
| `server.go` | HTTP Server 生命周期、路由注册入口、依赖注入 |
| `middleware.go` | 认证 / 限流 / 日志等全局中间件链 |
| `logstream.go` | SSE 日志流推送（Agent 日志实时输出到前端） |
| `context.go` | 请求上下文封装（从 HTTP Request 提取 Agent/Session ID） |
| `contextref.go` | ContextRef 跨请求引用（长连接场景） |

## 子包索引

### `chat/` — 对话与会话

| 文件 | 职责 |
|------|------|
| `handler.go` | chat 路由注册，挂载到主 Server |
| `sessions.go` | Session CRUD、列表、删除 |
| `sse.go` | SSE 推流主逻辑（流式 token 输出） |
| `sse_media.go` | 多媒体消息（图片/音频）SSE 扩展 |
| `audio.go` | 语音输入处理（STT 接入） |
| `compressor.go` | 长上下文压缩（超 Token 上限时触发） |
| `recap.go` | 会话摘要生成 |
| `slash_commands.go` | `/command` 解析与派发 |
| `transcript.go` | 历史消息导出 |

### `plugin/` — 插件管理

| 文件 | 职责 |
|------|------|
| `handler.go` | plugin 路由注册 |
| `catalog.go` | 插件市场目录查询 |
| `custom.go` | 用户自定义插件管理 |
| `manage.go` | 插件安装 / 卸载 / 升级 |
| `sync.go` | 插件同步（远端市场拉取） |

### `provider/` — LLM Provider 管理

| 文件 | 职责 |
|------|------|
| `handler.go` | provider 路由注册 |
| `providers.go` | Provider CRUD（增删查改） |
| `catalog.go` | 内置 Provider 目录（模型列表） |
| `loader.go` | 运行时 Provider 动态加载 |
| `seed.go` | 默认 Provider 初始化（首次启动写库） |

### `sysadmin/` — 系统管理

| 文件 | 职责 |
|------|------|
| `handler.go` | sysadmin 路由注册 |
| `channels.go` | 聊天平台适配器管理（TG/Discord） |
| `mcp_servers.go` | MCP Server 配置 CRUD |
| `cron.go` | 定时任务管理（增删查改） |
| `workflow.go` | 自动化工作流 CRUD |
| `hooks.go` | Webhook 管理 |
| `tools.go` | 工具白名单 / 策略配置 |
| `preferences.go` | 系统偏好设置 |
| `prompts.go` | 系统提示词管理 |
| `openai_compat.go` | OpenAI 兼容接口（`/v1/chat/completions`） |
| `budget.go` | Token 预算管理 |
| `insights.go` | 使用统计与洞察 |
| `vfs.go` | 虚拟工作区文件系统接口 |
| `export.go` | 数据导出 |
| `doctor.go` | 系统健康诊断（`/health` + 组件自检） |
| `system_update.go` | 系统自动更新触发 |

## 常用查找场景

| 场景 | 目标文件 |
|------|------|
| 修改 SSE 流式输出 | `chat/sse.go` |
| 新增路由 | 对应子包 `handler.go` + `server.go` 注册 |
| 修改认证逻辑 | `middleware.go` |
| 插件安装流程 | `plugin/manage.go` |
| OpenAI 兼容接口 | `sysadmin/openai_compat.go` |
| MCP Server 配置 | `sysadmin/mcp_servers.go` |
| Agent 日志推送 | `logstream.go` |
| 定时任务 API | `sysadmin/cron.go` |
