// ── 斜杠命令定义（M13-Interface-WebUI.md §15）────────────────────────────
export const SLASH_COMMANDS = [
  { cmd: '/help',     desc: '显示快捷键帮助' },
  { cmd: '/sessions', desc: '跳转会话列表' },
  { cmd: '/skills',   desc: '跳转 Skill 库' },
  { cmd: '/memory',   desc: '查看当前记忆摘要' },
  { cmd: '/status',   desc: '跳转系统状态' },
  { cmd: '/clear',    desc: '清空对话视图（保留记录）' },
  { cmd: '/compact',  desc: '手动触发上下文压缩' },
]