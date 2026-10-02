import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'

// ══════════════════════════════════════════════════════════════════════════
// store: audio（语音资产状态与后台预置门控，ADR-0108）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('audio', {
  stt: null,
  tts: null,
  engine: 'auto',
  autoInstall: true,
  pendingIntent: { stt: false, tts: false },
  _timer: null,
  _lastFailToast: { stt: '', tts: '' },

  // 页内确认 modal 数据
  modal: {
    open: false,
    kind: 'stt',
    sizeMB: 0,
    resolve: null,
  },

  get activeKinds() {
    const res = []
    if (this.stt && (this.stt.state === 'downloading' || this.stt.state === 'loading')) res.push('stt')
    if (this.tts && (this.tts.state === 'downloading' || this.tts.state === 'loading')) res.push('tts')
    return res
  },

  pct(kind) {
    const st = this[kind]
    if (!st || !st.progress) return null
    const { bytes_done, bytes_total } = st.progress
    if (!bytes_total || bytes_total <= 0) return null
    return Math.floor((bytes_done * 100) / bytes_total)
  },

  get combinedPct() {
    let done = 0
    let total = 0
    let hasProgress = false
    for (const k of ['stt', 'tts']) {
      const st = this[k]
      if (st && (st.state === 'downloading' || st.state === 'loading')) {
        if (st.progress && st.progress.bytes_total > 0) {
          done += st.progress.bytes_done
          total += st.progress.bytes_total
          hasProgress = true
        }
      }
    }
    if (!hasProgress || total <= 0) return null
    return Math.floor((done * 100) / total)
  },

  get chipVisible() {
    const hasActive = this.activeKinds.length > 0
    const hasFail = (this.stt && this.stt.state === 'failed') || (this.tts && this.tts.state === 'failed')
    return hasActive || hasFail
  },

  get chipLevel() {
    const hasFail = (this.stt && this.stt.state === 'failed') || (this.tts && this.tts.state === 'failed')
    if (hasFail) return 'warn'
    return 'ok'
  },

  isDownloading(kind) {
    const st = this[kind]
    return !!(st && (st.state === 'downloading' || st.state === 'loading'))
  },

  buttonTitle(kind) {
    const st = this[kind]
    if (!st || (st.state !== 'downloading' && st.state !== 'loading')) return ''
    const t = (k) => Alpine.store('i18n')?.t(k) || k
    const p = this.pct(kind)
    const pStr = p !== null ? `${p}%` : '…'
    if (kind === 'stt') {
      return t('audio_wait_stt').replace('{0}', pStr)
    }
    return t('audio_wait_tts').replace('{0}', pStr)
  },

  sizeMB(kind) {
    const st = this[kind]
    const bytes = st?.install_size_bytes || 0
    if (bytes <= 0) return kind === 'stt' ? 247 : 382
    return Math.max(1, Math.round(bytes / 1048576))
  },

  async refresh() {
    try {
      const r = await fetch('/v1/audio/status', { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      const prevSTT = this.stt
      const prevTTS = this.tts
      this.stt = d.stt_status
      this.tts = d.tts_status
      this.engine = d.tts_engine || 'auto'
      this.autoInstall = d.auto_install ?? true

      // 终态提示检查
      this._checkTerminalState('stt', prevSTT, this.stt)
      this._checkTerminalState('tts', prevTTS, this.tts)

      // 排定下一次轮询
      this._scheduleNextPoll()
    } catch {
      this._scheduleNextPoll(15000)
    }
  },

  _checkTerminalState(kind, prev, cur) {
    if (!cur) return
    const t = (k) => Alpine.store('i18n')?.t(k) || k
    const toast = Alpine.store('toast')

    // ready：仅当用户曾点击过该按钮时提示一次
    if (cur.state === 'ready') {
      if (this.pendingIntent[kind]) {
        this.pendingIntent[kind] = false
        if (toast) {
          const msg = t(kind === 'stt' ? 'audio_ready_stt' : 'audio_ready_tts')
          toast.show('ok', msg, 4000, { key: `audio-${kind}` })
        }
      }
    } else if (cur.state === 'failed') {
      // 自动重试耗尽（无 next_retry_at）
      if (!cur.next_retry_at) {
        const errText = cur.error || cur.detail || '安装失败'
        if (errText !== this._lastFailToast[kind]) {
          this._lastFailToast[kind] = errText
          if (toast) {
            toast.show('error', errText, 6000, { key: `audio-${kind}` })
          }
        }
      }
    }
  },

  _scheduleNextPoll(overrideMs) {
    if (this._timer) {
      clearTimeout(this._timer)
      this._timer = null
    }

    if (overrideMs) {
      this._timer = setTimeout(() => this.refresh(), overrideMs)
      return
    }

    // 任一 kind 处于 downloading|loading → 每 2s
    const active = (this.stt && (this.stt.state === 'downloading' || this.stt.state === 'loading')) ||
                   (this.tts && (this.tts.state === 'downloading' || this.tts.state === 'loading'))
    if (active) {
      this._timer = setTimeout(() => this.refresh(), 2000)
      return
    }

    // 任一处于 not_installed 且 auto_install=true，或 failed 且带 next_retry_at → 每 15s
    const pendingAuto = this.autoInstall && (
      (this.stt && this.stt.state === 'not_installed') ||
      (this.tts && this.tts.state === 'not_installed')
    )
    const retrying = (this.stt && this.stt.state === 'failed' && this.stt.next_retry_at) ||
                     (this.tts && this.tts.state === 'failed' && this.tts.next_retry_at)
    if (pendingAuto || retrying) {
      this._timer = setTimeout(() => this.refresh(), 15000)
      return
    }

    // 其余终态停止轮询
  },

  async install(kind) {
    try {
      await fetch(`/v1/audio/${kind}/install`, { method: 'POST', headers: authHeaders() })
    } catch {}
    await this.refresh()
  },

  async retry(kind) {
    return this.install(kind)
  },

  confirmInstallModal(confirmed) {
    if (this.modal.resolve) {
      this.modal.resolve(confirmed)
      this.modal.resolve = null
    }
    this.modal.open = false
  },

  promptInstall(kind) {
    const size = this.sizeMB(kind)
    this.modal.kind = kind
    this.modal.sizeMB = size
    this.modal.open = true
    return new Promise((resolve) => {
      this.modal.resolve = resolve
    })
  },
})
