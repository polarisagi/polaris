import Alpine from 'alpinejs'
import { authHeaders, sanitizeContent } from '../utils.js'

// ══════════════════════════════════════════════════════════════════════════
// store: eval - M12 eval suite page
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('eval', {
  suite: 'training',
  running: false,
  report: null,
  history: [],

  async run() {
    if (this.running) return
    this.running = true
    this.report = null
    try {
      const r = await fetch('/v1/eval/run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify({ suite: this.suite }),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      this.report = d
      this.history.unshift({ ...d, _runAt: new Date().toISOString() })
      if (this.history.length > 50) this.history.pop()
      Alpine.store('toast').show('ok', 'eval ' + this.suite + ' done: ' + d.pass_count + '/' + d.total_cases + ' passed')
    } catch (err) {
      Alpine.store('toast').show('error', 'eval failed: ' + err.message)
    } finally {
      this.running = false
    }
  },

  passRate(report) {
    if (!report || report.total_cases === 0) return 0
    return Math.round((report.pass_count / report.total_cases) * 100)
  },

  statusLabel(status) {
    const i18n = Alpine.store('i18n')
    const map = {
      completed: i18n.t('eval_status_completed'),
      failed:    i18n.t('eval_status_failed'),
      cancelled: i18n.t('eval_status_cancelled'),
      running:   i18n.t('eval_status_running'),
    }
    return map[status] || status
  },

  statusClass(status) {
    return {
      completed: 'color:var(--color-success)',
      failed:    'color:var(--color-error)',
      cancelled: 'color:var(--color-text-dim)',
      running:   'color:var(--color-accent)',
    }[status] || ''
  },
})