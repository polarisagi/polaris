import Alpine from 'alpinejs'
// ══════════════════════════════════════════════════════════════════════════
// store: dialog（应用内确认/提示对话框）
//
// 为什么存在：桌面版是 Tauri v2 的 WKWebView，未装 dialog 插件时 window.confirm/alert
// 既不弹框又直接返回 false/undefined，导致「删除」之类操作静默无反应。所有原生
// confirm/alert/prompt 一律改走本 store（门控：make native-dialog-check）。
//
// 安全：title/message 只经 x-text 渲染为纯文本，与原生对话框同等的「不解释 HTML」
// 保证——mcp_apps.js 依赖这一点展示服务器/View 提供的字符串。
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('dialog', {
  open: false,
  kind: 'confirm', // 'confirm' | 'alert'
  title: '',
  message: '',
  confirmText: '',
  cancelText: '',
  danger: false,
  _resolve: null,
  _queue: [],

  _t(key) {
    return Alpine.store('i18n')?.t(key) ?? key
  },

  /** @returns {Promise<boolean>} 点确认 true；取消 / Esc / 点遮罩 false */
  confirm(opts = {}) {
    return this._enqueue('confirm', opts)
  },

  /** @returns {Promise<void>} */
  alert(opts = {}) {
    return this._enqueue('alert', opts)
  },

  _enqueue(kind, opts) {
    return new Promise((resolve) => {
      this._queue.push({ kind, opts: typeof opts === 'string' ? { message: opts } : opts, resolve })
      if (!this.open) this._next()
    })
  },

  _next() {
    const item = this._queue.shift()
    if (!item) {
      this.open = false
      return
    }
    const { kind, opts } = item
    this.kind = kind
    this.title = opts.title || this._t(kind === 'confirm' ? 'dialog_confirm_title' : 'dialog_alert_title')
    this.message = opts.message || ''
    this.confirmText = opts.confirmText || this._t(kind === 'confirm' ? 'dialog_confirm_btn' : 'dialog_ok_btn')
    this.cancelText = opts.cancelText || this._t('cancel')
    this.danger = !!opts.danger
    this._resolve = item.resolve
    this.open = true
  },

  /** 关闭当前对话框；ok=true 为确认 */
  close(ok) {
    const resolve = this._resolve
    this._resolve = null
    this.open = false
    if (resolve) resolve(this.kind === 'confirm' ? !!ok : undefined)
    // 让出一拍再展示队列里的下一个，避免同一次点击连带关掉它
    setTimeout(() => this._next(), 0)
  },
})
