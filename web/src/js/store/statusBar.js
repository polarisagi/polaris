import Alpine from 'alpinejs'
import { readHTTPErrorMessage } from '../http_error.mjs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: statusBar（持久状态栏，M13-Interface-WebUI.md §13）
// ══════════════════════════════════════════════════════════════════════════

// ══════════════════════════════════════════════════════════════════════════
// store: fmt（用量/费用展示格式化，A2）
// 后端账本一律 USD；展示币种默认跟随界面语言（中文 ¥ / 英文 $），可在设置页改。
// 汇率是配置常量（后端 config.UsageDisplayFXCNYPerUSD，经 /v1/status 下发），只是估算，界面处处标注。
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('fmt', {
  pref: (() => { try { return localStorage.getItem('polaris_currency') || 'auto' } catch { return 'auto' } })(), // auto | cny | usd
  fx: 7.1,

  get currency() {
    if (this.pref === 'cny' || this.pref === 'usd') return this.pref
    return Alpine.store('i18n').lang === 'en' ? 'usd' : 'cny'
  },
  get symbol() { return this.currency === 'cny' ? '¥' : '$' },

  setPref(v) {
    this.pref = v
    try { localStorage.setItem('polaris_currency', v) } catch { /* 隐私模式 */ }
  },

  /** USD → 展示币种金额文本，如 ¥0.0042。小额保留 4 位有效小数，避免显示成 0。 */
  money(usd) {
    const v = (Number(usd) || 0) * (this.currency === 'cny' ? this.fx : 1)
    if (v === 0) return this.symbol + '0'
    const digits = v >= 1 ? 2 : v >= 0.01 ? 3 : 4
    return this.symbol + v.toFixed(digits)
  },

  /** 1234 → 1.2k；1_500_000 → 1.5M */
  tok(n) {
    n = Number(n) || 0
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M'
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k'
    return String(n)
  },

  pct(ratio) { return Math.round((Number(ratio) || 0) * 100) + '%' },

  secs(ms) {
    const s = (Number(ms) || 0) / 1000
    if (s < 1) return Math.round(ms || 0) + 'ms'
    if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's'
    return Math.floor(s / 60) + 'm' + String(Math.round(s % 60)).padStart(2, '0') + 's'
  },

  /** 估算说明（tooltip）。 */
  get hint() {
    return Alpine.store('i18n').t('cost_estimate_hint').replace('{0}', String(this.fx))
  },
})

Alpine.store('statusBar', {
  connected: false,
  sealed: false,
  modelID: '',
  sessionID: '',
  // 用量（A4）：scope=session 时为当前会话累计与上下文占用；无会话时为今日全局累计。
  usageScope: 'today',
  usage: null,
  memoryMB: 0,
  memoryLimitMB: 8192,
  embedding: null,
  accelerator: null,
  rebenchRunning: false,
  _timer: null,

  /** 上下文占用比例 0..1（窗口未知为 0）。 */
  get tokenPct() { return this.usage?.context_pct || 0 },
  get tokenUsed() { return this.usage?.last_input || 0 },
  get tokenLimit() { return this.usage?.context_window || 0 },
  get costUSD() { return this.usage?.cost_usd || 0 },
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
      const sid = Alpine.store('chat').sessionID || ''
      const r = await fetch('/v1/status' + (sid ? `?session_id=${encodeURIComponent(sid)}` : ''), { headers: authHeaders() })
      if (!r.ok) { this.connected = false; return }
      const d = await r.json()
      this.connected = true
      this.sealed = d.sealed || false
      this.modelID = d.model_id || ''
      this.sessionID = sid
      if (d.fx_cny_per_usd > 0) Alpine.store('fmt').fx = d.fx_cny_per_usd
      this.usageScope = d.usage?.scope || 'today'
      this.usage = d.usage?.data || null
      this.memoryMB = d.memory_mb || 0
      this.memoryLimitMB = d.memory_limit_mb || 8192
      this.embedding = d.embedding || null
      this.accelerator = d.accelerator || null

      if (this.sealed) {
        Alpine.store('toast').show('warn', '⚠ 服务器已进入 Sealed 状态，所有操作已暂停')
      }
    } catch {
      this.connected = false
    }
  },

  async rebenchEmbedding() {
    this.rebenchRunning = true
    try {
      const r = await fetch('/v1/embedding/rebench', { method: 'POST', headers: authHeaders() })
      if (!r.ok) console.warn('rebench rejected', await readHTTPErrorMessage(r))
      await this.poll()
    } catch (e) {
      console.warn('rebench failed', e)
    } finally {
      this.rebenchRunning = false
    }
  },
})

