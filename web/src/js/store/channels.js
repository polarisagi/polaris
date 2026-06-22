import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: channels（聊天平台集成，M13-Interface-WebUI.md §18）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('channels', {
  list: [],
  loading: false,
  showModal: false,
  editMode: 'create',
  form: {
    id: '', name: '', type: 'telegram', enabled: true, config: {},
  },

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/channels', { headers: authHeaders() })
      const d = await r.json()
      this.list = d.channels || []
    } catch { Alpine.store('toast').show('error', '加载集成配置失败') }
    finally { this.loading = false }
  },

  openCreate() {
    this.form = { id: '', name: '', type: 'telegram', enabled: true, config: {} }
    this.editMode = 'create'
    this.showModal = true
  },

  openEdit(c) {
    this.form = { ...c, config: { ...c.config } }
    this.editMode = 'update'
    this.showModal = true
  },

  async save() {
    const method = this.editMode === 'create' ? 'POST' : 'PUT'
    const url = this.editMode === 'create' ? '/v1/channels' : `/v1/channels/${this.form.id}`
    const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json', ...authHeaders() }, body: JSON.stringify(this.form) })
    if (r.ok) {
      this.showModal = false
      await this.load()
      Alpine.store('toast').show('success', '保存成功')
    } else {
      Alpine.store('toast').show('error', '保存失败')
    }
  },

  async remove(id) {
    const r = await fetch(`/v1/channels/${id}`, { method: 'DELETE', headers: authHeaders() })
    if (r.ok) { await this.load(); Alpine.store('toast').show('success', '已删除') }
  },

  async toggle(c) {
    await fetch(`/v1/channels/${c.id}`, { method: 'PUT', headers: { 'Content-Type': 'application/json', ...authHeaders() }, body: JSON.stringify({ ...c, enabled: !c.enabled }) })
    await this.load()
  },

  copyWebhook(url) {
    navigator.clipboard.writeText(location.origin + url)
    Alpine.store('toast').show('success', 'Webhook URL 已复制')
  },

  typeLabel(t) {
    return { telegram: 'Telegram', feishu: '飞书', slack: 'Slack', discord: 'Discord', webhook: '通用 Webhook' }[t] || t
  },

  typeIcon(t) {
    return { telegram: '✈', feishu: '🪁', slack: '#', discord: '🎮', webhook: '⚡' }[t] || '?'
  },
})

