import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: sessions
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('sessions', {
  list: [],
  loading: false,

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/sessions', { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      this.list = d.sessions || []
    } catch { /* 静默 */ } finally {
      this.loading = false
    }
  },

  async deleteSession(id) {
    if (!confirm('确认删除这个会话？')) return
    try {
      await fetch(`/v1/sessions/${id}`, { method: 'DELETE', headers: authHeaders() })
      this.list = this.list.filter(s => s.id !== id)
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

