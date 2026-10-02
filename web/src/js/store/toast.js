import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: toast（全局通知）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('toast', {
  items: [],
  _nextId: 0,

  show(type, message, durationMs = 4000, opts = {}) {
    if (opts && opts.key) {
      const existing = this.items.find((t) => t.key === opts.key)
      if (existing) {
        if (existing._timer) clearTimeout(existing._timer)
        existing.type = type
        existing.message = message
        existing._timer = setTimeout(() => {
          this.items = this.items.filter((t) => t.id !== existing.id)
        }, durationMs)
        return
      }
    }
    const id = this._nextId++
    const item = { id, type, message, key: opts?.key, _timer: null }
    item._timer = setTimeout(() => {
      this.items = this.items.filter((t) => t.id !== id)
    }, durationMs)
    this.items.push(item)
  },
})

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

