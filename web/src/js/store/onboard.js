import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: onboard（首次配置向导）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('onboard', {
  show: false,
  step: 1,  // 1 欢迎 | 2 Provider | 3 模型 | 4 接入（可选）

  // Step 2
  providerForm: { name: '', type: 'openai_compat', base_url: '', api_key: '', project_id: '', location: 'us-central1' },
  providerID: '',
  testingProvider: false,
  testResult: null,   // null | 'ok' | 'error'
  testMsg: '',

  // Step 3（追踪已分配角色的 model UUID，用于合并调用 model-roles）
  modelForm: { model_id: '', name: '', role: 'default' },
  models: [],
  defaultModelID: '',
  reasoningModelID: '',

  // Step 4
  channelForm: { name: '', type: 'telegram', config: {} },

  saving: false,

  async checkFirstRun() {
    if (localStorage.getItem('polaris:onboard:dismissed')) return
    try {
      const r = await fetch('/v1/providers', { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      if (!d.providers || d.providers.length === 0) this._open()
    } catch { /* 静默，不阻断正常使用 */ }
  },

  _open() {
    this.reset()
    this.show = true
  },

  start() {
    // Providers 页"配置向导"按钮手动触发
    this._open()
  },

  dismiss() {
    localStorage.setItem('polaris:onboard:dismissed', '1')
    this.show = false
  },

  reset() {
    this.step = 1
    this.providerForm = { name: '', type: 'openai_compat', base_url: '', api_key: '', project_id: '', location: 'us-central1' }
    this.providerID = ''
    this.testResult = null
    this.testMsg = ''
    this.modelForm = { model_id: '', name: '', role: 'default' }
    this.models = []
    this.defaultModelID = ''
    this.reasoningModelID = ''
    this.channelForm = { name: '', type: 'telegram', config: {} }
  },

  // ── Step 2 ────────────────────────────────────────────────────────────
  async saveProvider() {
    if (!this.providerForm.name.trim()) { Alpine.store('toast').show('error', '请填写厂商名称'); return }
    if (this.providerID) { this.step = 3; return }  // 已保存（测试时隐式创建），直接前进
    this.saving = true
    try {
      const r = await fetch('/v1/providers', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify({ ...this.providerForm, enabled: true }),
      })
      if (!r.ok) { Alpine.store('toast').show('error', '保存失败'); return }
      const d = await r.json()
      this.providerID = d.id
      await Alpine.store('providers').load()
      this.step = 3
    } catch (e) { Alpine.store('toast').show('error', `保存失败: ${e.message}`) }
    finally { this.saving = false }
  },

  async testProvider() {
    if (!this.providerForm.name.trim()) { Alpine.store('toast').show('error', '请先填写厂商名称'); return }
    // 若未保存先隐式创建，不前进步骤
    if (!this.providerID) {
      this.saving = true
      try {
        const r = await fetch('/v1/providers', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeaders() },
          body: JSON.stringify({ ...this.providerForm, enabled: true }),
        })
        if (!r.ok) { Alpine.store('toast').show('error', '保存失败'); return }
        const d = await r.json()
        this.providerID = d.id
        await Alpine.store('providers').load()
      } catch (e) { Alpine.store('toast').show('error', `保存失败: ${e.message}`); return }
      finally { this.saving = false }
    }
    this.testingProvider = true
    this.testResult = null
    this.testMsg = ''
    try {
      const r = await fetch(`/v1/providers/${this.providerID}/test`, { method: 'POST', headers: authHeaders() })
      const d = await r.json()
      this.testResult = d.ok ? 'ok' : 'error'
      this.testMsg = d.message || (d.ok ? '连接正常' : '连接失败')
    } catch (e) { this.testResult = 'error'; this.testMsg = e.message }
    finally { this.testingProvider = false }
  },

  // ── Step 3 ────────────────────────────────────────────────────────────
  async addModel() {
    if (!this.modelForm.model_id.trim()) { Alpine.store('toast').show('error', '请填写模型 ID'); return }
    this.saving = true
    try {
      const r = await fetch(`/v1/providers/${this.providerID}/models`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify({ ...this.modelForm, enabled: true }),
      })
      if (!r.ok) { Alpine.store('toast').show('error', '添加失败'); return }
      const d = await r.json()
      const uuid = d.id

      // 更新本地角色追踪，合并调用 model-roles（避免上一次添加的角色被重置）
      if (this.modelForm.role === 'default')   this.defaultModelID   = uuid
      if (this.modelForm.role === 'reasoning') this.reasoningModelID = uuid
      const rolesPayload = {}
      if (this.defaultModelID)   rolesPayload.default_model_id   = this.defaultModelID
      if (this.reasoningModelID) rolesPayload.reasoning_model_id = this.reasoningModelID
      if (Object.keys(rolesPayload).length > 0) {
        await fetch('/v1/config/model-roles', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json', ...authHeaders() },
          body: JSON.stringify(rolesPayload),
        })
      }

      this.models.push({ model_id: this.modelForm.model_id, role: this.modelForm.role })
      this.modelForm = { model_id: '', name: '', role: 'general' }
      await Alpine.store('providers').load()
      await Alpine.store('modelRoles').load()
      Alpine.store('toast').show('ok', '模型已添加')
    } catch (e) { Alpine.store('toast').show('error', `添加失败: ${e.message}`) }
    finally { this.saving = false }
  },

  // ── Step 4 ────────────────────────────────────────────────────────────
  async saveChannel() {
    if (!this.channelForm.name.trim()) { Alpine.store('toast').show('error', '请填写接入名称'); return false }
    this.saving = true
    try {
      const r = await fetch('/v1/channels', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify({ ...this.channelForm, enabled: true }),
      })
      if (!r.ok) { Alpine.store('toast').show('error', '保存失败'); return false }
      await Alpine.store('channels').load()
      return true
    } catch (e) { Alpine.store('toast').show('error', `保存失败: ${e.message}`); return false }
    finally { this.saving = false }
  },

  async saveChannelAndFinish() {
    const ok = await this.saveChannel()
    if (ok) this.finish()
  },

  finish() {
    localStorage.setItem('polaris:onboard:dismissed', '1')
    this.show = false
    Alpine.store('nav').navigate('chat')
    Alpine.store('toast').show('ok', '配置完成，开始对话！')
  },

  // 渠道类型对应的 config 字段名和显示标签
  channelConfigKey(type)   { return { telegram: 'bot_token', feishu: 'app_id', slack: 'bot_token', discord: 'bot_token', webhook: '' }[type] || '' },
  channelConfigLabel(type) { return { telegram: 'Bot Token', feishu: 'App ID', slack: 'Bot Token', discord: 'Bot Token', webhook: '' }[type] || '' },
})

// EOF
