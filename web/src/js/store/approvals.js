import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: approvals（HITL 审批，M13-Interface-WebUI.md §4）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('approvals', {
  list: [],
  pollFailures: 0,
  _timer: null,
  _seen: null, // 已提示过的无人值守审批 id（首轮只建立基线，不对存量项重复弹提示）

  // 全局轮询：无人值守审批（cron / workflow / webhook）可能在任何页面产生，侧边栏角标与提示
  // 不能依赖用户恰好停在自动化页。自动化页 5s，其它页 15s。
  startPolling(fast = false) {
    this.pollFailures = 0
    this.poll()
    this._setInterval(fast ? 5000 : 15000)
  },

  setFast(fast) {
    if (this._timer === null) return
    this._setInterval(fast ? 5000 : 15000)
  },

  _setInterval(ms) {
    clearInterval(this._timer)
    this._timer = setInterval(() => this.poll(), ms)
  },

  stopPolling() { clearInterval(this._timer); this._timer = null },

  // 无人值守审批 = 自动化运行中产生、没人在对话界面前的审批。
  unattendedCount() { return this.list.filter(a => a.unattended).length },

  // 新出现的无人值守审批：页面可见时弹 toast；页面在后台且已授权时发系统通知（桌面壳 webview 同样走
  // Web Notification，Tauri 壳不承载业务命令）。
  _notifyNew(items) {
    const fresh = items.filter(a => a.unattended && !(this._seen && this._seen.has(a.id)))
    const first = this._seen === null
    this._seen = new Set(items.filter(a => a.unattended).map(a => a.id))
    if (first || fresh.length === 0) return
    const i18n = Alpine.store('i18n')
    const msg = i18n.t('approvals_unattended_toast').replace('{0}', fresh.length)
    if (document.visibilityState === 'visible') {
      Alpine.store('toast').show('ok', msg, 8000, {
        key: 'approvals-unattended',
        onClick: () => Alpine.store('nav').navigate('automation'),
      })
    } else if (typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      try { new Notification(i18n.t('approvals_unattended_notify_title'), { body: msg }) } catch { /* 通知不可用时静默 */ }
    }
  },

  // 首次出现无人值守审批时请求系统通知权限（需用户手势之外的场景浏览器可能忽略，失败不影响功能）。
  requestNotifyPermission() {
    try {
      if (typeof Notification !== 'undefined' && Notification.permission === 'default') Notification.requestPermission()
    } catch { /* ignore */ }
  },

  async poll() {
    if (this.pollFailures >= 3) return
    try {
      const r = await fetch('/v1/approvals/pending', { headers: authHeaders() })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      const pending = d.pending || []
      this._notifyNew(pending)
      this.list = pending.map(a => ({
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

  async resolve(id, action, comment = '', remember = false) {
    this.requestNotifyPermission() // 用户手势内请求系统通知权限
    try {
      const r = await fetch(`/v1/approvals/${id}/resolve`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ action, comment, remember: action === 'approve' && !!remember }),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      this.list = this.list.filter(a => a.id !== id)
      Alpine.store('toast').show('ok', `审批 ${id.slice(0, 8)} 已${action === 'approve' ? '通过' : '拒绝'}`)
    } catch (err) {
      Alpine.store('toast').show('error', `操作失败: ${err.message}`)
    }
  },
})

