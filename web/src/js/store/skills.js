import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: skills
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('skills', {
  list: [],
  loading: false,

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/skills', { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      this.list = d.skills || []
    } catch { /* 静默 */ } finally {
      this.loading = false
    }
  },
})

