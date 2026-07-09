# Shell Script Hooks 使用指南

Hooks 目录：`~/.polarisagi/polaris/hooks/`（或 `POLARIS_HOOKS_DIR` 环境变量覆盖）

脚本需要**可执行权限**：`chmod +x ~/.polarisagi/polaris/hooks/<event>`

## 事件点与环境变量

| 脚本文件名 | 触发时机 | 可用环境变量 | 阻塞主流程 |
|---|---|---|---|
| `gateway.startup` | 服务启动后 | `POLARIS_WORKSPACE`, `POLARIS_ADDR` | 否 |
| `session.new` | 新会话创建时 | `POLARIS_SESSION_ID`, `POLARIS_CHANNEL` | 否 |
| `message.before` | 处理用户消息前 | `POLARIS_MESSAGE`, `POLARIS_SESSION_ID`, `POLARIS_CHANNEL`, `POLARIS_USER_ID`, `POLARIS_CHAT_ID` | **是**（非零退出=拦截） |
| `message.after` | AI 回复发出后 | `POLARIS_REPLY`, `POLARIS_SESSION_ID`, `POLARIS_CHANNEL`, `POLARIS_USER_ID`, `POLARIS_CHAT_ID` | 否 |
| `session.compact.before` | 上下文压缩开始前 | `POLARIS_SESSION_ID`, `POLARIS_TOKEN_COUNT` | **是** |
| `session.compact.after` | 上下文压缩完成后 | `POLARIS_SESSION_ID`, `POLARIS_TOKEN_BEFORE`, `POLARIS_TOKEN_AFTER` | 否 |

## 规则

- 脚本不存在 → 静默跳过，不报错
- 非阻塞 hook 超时：5s（失败只记日志）
- 阻塞 hook（before 类）超时：2s（超时视为放行，不拦截）
- `message.before` 非零退出：返回错误给用户，stdout 作为原因
- 所有脚本在当前用户权限下运行

## 示例

见同目录下的示例脚本。
