import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: config（运行时配置只读视图）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('config', {
  raw: '',
  path: '',
  format: 'yaml',
  loading: false,
  copied: false,

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/config', { headers: authHeaders() })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      this.raw = d.raw || ''
      this.path = d.path || ''
      this.format = d.format || 'yaml'
    } catch {
      this.raw = '/* 加载配置失败 */'
    } finally {
      this.loading = false
    }
  },

  async copy() {
    try {
      await navigator.clipboard.writeText(this.raw)
      this.copied = true
      setTimeout(() => { this.copied = false }, 2000)
    } catch { /* 静默 */ }
  },
})

