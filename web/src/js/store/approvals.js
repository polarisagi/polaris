import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: approvals（HITL 审批，M13-Interface-WebUI.md §4）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('approvals', {
  list: [],
  pollFailures: 0,
  _timer: null,

  startPolling() {
    this.pollFailures = 0
    this.poll()
    this._timer = setInterval(() => this.poll(), 5000)
  },

  stopPolling() { clearInterval(this._timer); this._timer = null },

  async poll() {
    if (this.pollFailures >= 3) return
    try {
      const r = await fetch('/v1/approvals/pending', { headers: authHeaders() })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      this.list = (d.pending || []).map(a => ({
        ...a,
        _remainingMs: () => {
          const deadline = new Date(a.created_at).getTime() + (a.timeout_ms || 1800000)
          return deadline - Date.now()
        },
      }))
      this.pollFailures = 0
    } catch {
      this.pollFailures++
    }
  },

  async resolve(id, action, comment = '') {
    try {
      const r = await fetch(`/v1/approvals/${id}/resolve`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ action, comment }),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      this.list = this.list.filter(a => a.id !== id)
      Alpine.store('toast').show('ok', `审批 ${id.slice(0, 8)} 已${action === 'approve' ? '通过' : '拒绝'}`)
    } catch (err) {
      Alpine.store('toast').show('error', `操作失败: ${err.message}`)
    }
  },
})

