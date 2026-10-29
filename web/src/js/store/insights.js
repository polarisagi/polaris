import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: insights（用量洞察）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('insights', {
  data: null,
  loading: false,
  days: 30,

  async load() {
    this.loading = true
    try {
      const r = await fetch(`/v1/insights?days=${this.days}`, { headers: authHeaders() })
      this.data = await r.json()
    } catch { } finally { this.loading = false }
  },

  maxDailyCount() {
    if (!this.data?.daily_trend?.length) return 1
    return Math.max(...this.data.daily_trend.map(d => d.count), 1)
  },
})

