import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: search（全文搜索，FTS5）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('search', {
  query: '',
  results: [],
  loading: false,
  searched: false,

  async run() {
    const q = this.query.trim()
    if (!q) return
    this.loading = true
    this.searched = false
    try {
      const r = await fetch(`/v1/search?q=${encodeURIComponent(q)}&limit=20`, { headers: authHeaders() })
      const d = await r.json()
      this.results = d.results || []
    } catch { this.results = [] }
    finally { this.loading = false; this.searched = true }
  },

  openSession(id) {
    Alpine.store('chat').loadSession(id)
    Alpine.store('nav').navigate('chat')
  },
})

