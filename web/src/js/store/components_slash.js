// ── 斜杠命令定义（M13-Interface-WebUI.md §15）────────────────────────────
export const SLASH_COMMANDS = [
  { cmd: '/context',  desc: '查看当前上下文 Token 使用量与压缩统计' },
  { cmd: '/compact',  desc: '立即压缩上下文（跳过阈值，调用 LLM 摘要）' },
  { cmd: '/clear',    desc: '清空会话历史（同时清理后端数据库）' },
  { cmd: '/help',     desc: '列出所有可用斜杠命令' },
  { cmd: '/sessions', desc: '跳转会话列表' },
  { cmd: '/skills',   desc: '跳转 Skill 库' },
  { cmd: '/memory',   desc: '查看当前记忆摘要' },
  { cmd: '/status',   desc: '跳转系统状态' },
]