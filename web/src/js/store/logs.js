import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: logs（实时日志 SSE 流）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('logs', {
  entries: [],
  levelFilter: '',     // '' | 'debug' | 'info' | 'warn' | 'error'
  autoFollow: true,
  connected: false,
  paused: false,
  _evtSrc: null,
  _reconnTimer: null,

  get filtered() {
    if (!this.levelFilter) return this.entries
    return this.entries.filter(e => levelGe(e.level, this.levelFilter))
  },

  connect() {
    this.disconnect()
    this.connected = false
    this.entries = [] // 重新连接时清空，防止收到 server 的 recent 批次导致重复
    const url = this.levelFilter
      ? `/v1/logs/stream?level=${this.levelFilter}`
      : '/v1/logs/stream'
    this._evtSrc = new EventSource(url)
    this._evtSrc.addEventListener('log', (ev) => {
      try {
        const entry = JSON.parse(ev.data)
        this.entries.push(entry)
        if (this.entries.length > 1000) {
          this.entries = this.entries.slice(-500)
        }
      } catch { /* 忽略解析失败 */ }
    })
    this._evtSrc.onopen = () => {
      this.connected = true
      this.paused = false
    }
    this._evtSrc.onerror = () => {
      this.connected = false
      this._scheduleReconnect()
    }
  },

  disconnect() {
    if (this._evtSrc) {
      this._evtSrc.close()
      this._evtSrc = null
    }
    this.connected = false
    this._clearReconnect()
  },

  _scheduleReconnect() {
    this._clearReconnect()
    this._reconnTimer = setTimeout(() => {
      if (!this.connected) this.connect()
    }, 3000)
  },

  _clearReconnect() {
    if (this._reconnTimer) {
      clearTimeout(this._reconnTimer)
      this._reconnTimer = null
    }
  },

  clear() {
    this.entries = []
  },

  togglePause() {
    this.paused = !this.paused
    if (!this.paused) this.connect()
    else this.disconnect()
  },

  setLevel(level) {
    this.levelFilter = level
    this.connect()
  },

  drawerOpen: false,

  openDrawer() {
    this.drawerOpen = true
    this.connect()
  },

  closeDrawer() {
    this.drawerOpen = false
    this.disconnect()
  },

  destroy() {
    this.drawerOpen = false
    this.disconnect()
    this.entries = []
  },
})

