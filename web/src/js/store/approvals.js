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
        // 2026-07-07 修复：后端 HITLPrompt.deadline_ns 是绝对 Unix 纳秒时间戳
        // （非相对 duration），不存在 created_at/timeout_ms 字段——此前用这两个
        // 不存在的字段算倒计时，永远是 NaN，审批卡片的计时器从未正常显示过。
        // 直接用 deadline_ns 换算成毫秒后与当前时间相减即可，无需额外字段。
        _remainingMs: () => (a.deadline_ns ? a.deadline_ns / 1e6 - Date.now() : Infinity),
      }))
      this.pollFailures = 0
    } catch {
      this.pollFailures++
    }
  },

  // riskBucket 把后端 risk_level（数值，对应 pkg/types.RiskLevel：0=low/1=medium/
  // 2=high/3=privileged）映射为前端展示用的等级桶。
  riskBucket(level) {
    if (level >= 3) return 'critical'
    if (level === 2) return 'high'
    if (level === 1) return 'medium'
    return 'low'
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

