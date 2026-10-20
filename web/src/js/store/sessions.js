import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: sessions
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('sessions', {
  list: [],
  loading: false,
  projectFilter: null, // 项目 ID；非空时只列该项目的会话（项目页「查看全部」）

  async load() {
    this.loading = true
    try {
      const q = this.projectFilter ? `?project_id=${encodeURIComponent(this.projectFilter)}` : ''
      const r = await fetch('/v1/sessions' + q, { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      this.list = d.sessions || []
    } catch { /* 静默 */ } finally {
      this.loading = false
    }
  },

  clearProjectFilter() {
    this.projectFilter = null
    this.load()
  },

  // 会话是否在后台运行：本地 run 或服务端 running 标记任一为真。
  isRunning(s) {
    return !!(s && (s.running || Alpine.store('chat').isSessionRunning(s.id)))
  },

  async deleteSession(id) {
    if (!(await Alpine.store('dialog').confirm({ message: '确认删除这个会话？', danger: true }))) return
    try {
      const r = await fetch(`/v1/sessions/${id}`, { method: 'DELETE', headers: authHeaders() })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      this.list = this.list.filter(s => s.id !== id)
      Alpine.store('toast').show('ok', '已删除')
    } catch (err) {
      Alpine.store('toast').show('error', `删除失败: ${err.message}`)
    }
  },

  openSession(id) {
    Alpine.store('chat').loadSession(id)
    Alpine.store('nav').navigate('chat')
  },

  sourceIcon(source) {
    return { telegram: '✈', feishu: '🪁', slack: '#', discord: '🎮', line: '💬', qqbot: '🐧', whatsapp: '📱', webhook: '⚡', channel: '📡' }[source] || '📡'
  },
})

