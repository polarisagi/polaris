import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: statusBar（持久状态栏，M13-Interface-WebUI.md §13）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('statusBar', {
  connected: false,
  sealed: false,
  modelID: '',
  sessionID: '',
  tokenUsed: 0,
  tokenLimit: 0,
  costCNY: 0,
  memoryMB: 0,
  memoryLimitMB: 8192,
  _timer: null,

  get tokenPct() { return this.tokenLimit > 0 ? this.tokenUsed / this.tokenLimit : 0 },
  get tokenBadge() {
    if (this.tokenPct > 0.95) return { label: '🔥 95%', cls: 'error' }
    if (this.tokenPct > 0.80) return { label: '⚡ 80%', cls: 'warn' }
    return null
  },
  get memPct() { return this.memoryLimitMB > 0 ? this.memoryMB / this.memoryLimitMB : 0 },
  get connCls() {
    if (this.sealed) return 'error'
    return this.connected ? 'ok' : 'dim'
  },

  startPolling() {
    this.poll()
    this._timer = setInterval(() => this.poll(), 10000)
  },

  stopPolling() { clearInterval(this._timer) },

  async poll() {
    try {
      const r = await fetch('/v1/status', { headers: authHeaders() })
      if (!r.ok) { this.connected = false; return }
      const d = await r.json()
      this.connected = true
      this.sealed = d.sealed || false
      this.modelID = d.model_id || ''
      this.sessionID = Alpine.store('chat').sessionID || ''
      this.tokenUsed = d.token_used || 0
      this.tokenLimit = d.token_limit || 0
      this.costCNY = d.cost_cny || 0
      this.memoryMB = d.memory_mb || 0
      this.memoryLimitMB = d.memory_limit_mb || 8192

      if (this.sealed) {
        Alpine.store('toast').show('warn', '⚠ 服务器已进入 Sealed 状态，所有操作已暂停')
      }
    } catch {
      this.connected = false
    }
  },
})

