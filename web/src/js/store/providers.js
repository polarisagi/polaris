import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: providers（LLM 厂商凭据管理，两层架构 provider → models）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('providers', {
  list: [],
  loading: false,
  showModal: false,
  showModelModal: false,
  testingID: '',
  editMode: 'create',
  // 厂商凭据表单（无 model_id / role / is_default）
  form: {
    id: '', name: '', type: 'openai_compat',
    base_url: '', api_key: '',
    project_id: '', location: 'us-central1',
    enabled: true,
  },
  // 模型条目表单
  modelForm: {
    id: '', provider_id: '', model_id: '', name: '',
    role: 'general', enabled: true,
  },
  modelEditMode: 'create',

  // 所有启用厂商下的启用模型，供角色分配面板使用
  get allModels() {
    const result = []
    for (const p of this.list) {
      if (!p.enabled) continue
      for (const m of (p.models || [])) {
        if (m.enabled) result.push({ ...m, providerName: p.name })
      }
    }
    return result
  },

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/providers', { headers: authHeaders() })
      const d = await r.json()
      this.list = d.providers || []
    } catch { Alpine.store('toast').show('error', '加载厂商配置失败') }
    finally { this.loading = false }
  },

  openCreate() {
    this.form = { id: '', name: '', type: 'openai_compat', base_url: '', api_key: '', project_id: '', location: 'us-central1', enabled: true }
    this.editMode = 'create'
    this.showModal = true
  },

  openEdit(p) {
    this.form = { id: p.id, name: p.name, type: p.type, base_url: p.base_url, api_key: p.api_key, project_id: p.project_id, location: p.location, sa_key_json: p.sa_key_json, enabled: p.enabled }
    this.editMode = 'update'
    this.showModal = true
  },

  async save() {
    const method = this.editMode === 'create' ? 'POST' : 'PUT'
    const url = this.editMode === 'create' ? '/v1/providers' : `/v1/providers/${this.form.id}`
    const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json', ...authHeaders() }, body: JSON.stringify(this.form) })
    if (r.ok) {
      this.showModal = false
      await this.load()
      Alpine.store('toast').show('ok', '保存成功')
    } else {
      Alpine.store('toast').show('error', '保存失败')
    }
  },

  async remove(id) {
    const r = await fetch(`/v1/providers/${id}`, { method: 'DELETE', headers: authHeaders() })
    if (r.ok) { await this.load(); Alpine.store('toast').show('ok', '已删除') }
  },

  async test(id) {
    this.testingID = id
    try {
      const r = await fetch(`/v1/providers/${id}/test`, { method: 'POST', headers: authHeaders() })
      const d = await r.json()
      Alpine.store('toast').show(d.ok ? 'ok' : 'error', d.message || (d.ok ? '连接正常' : '连接失败'))
    } finally { this.testingID = '' }
  },

  async toggle(p) {
    await fetch(`/v1/providers/${p.id}`, { method: 'PUT', headers: { 'Content-Type': 'application/json', ...authHeaders() }, body: JSON.stringify({ ...p, enabled: !p.enabled }) })
    await this.load()
  },

  // ── 模型 CRUD ──────────────────────────────────────────────────────────

  openAddModel(providerID) {
    this.modelForm = { id: '', provider_id: providerID, model_id: '', name: '', role: 'general', enabled: true }
    this.modelEditMode = 'create'
    this.showModelModal = true
  },

  openEditModel(providerID, m) {
    this.modelForm = { ...m, provider_id: providerID }
    this.modelEditMode = 'update'
    this.showModelModal = true
  },

  async saveModel() {
    const { provider_id, id } = this.modelForm
    const method = this.modelEditMode === 'create' ? 'POST' : 'PUT'
    const url = this.modelEditMode === 'create'
      ? `/v1/providers/${provider_id}/models`
      : `/v1/providers/${provider_id}/models/${id}`
    const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json', ...authHeaders() }, body: JSON.stringify(this.modelForm) })
    if (r.ok) {
      this.showModelModal = false
      await this.load()
      await Alpine.store('modelRoles').load()
      Alpine.store('toast').show('ok', '模型已保存')
    } else {
      Alpine.store('toast').show('error', '保存失败')
    }
  },

  async removeModel(providerID, modelID) {
    if (!confirm('确认删除这个模型配置？')) return
    const r = await fetch(`/v1/providers/${providerID}/models/${modelID}`, { method: 'DELETE', headers: authHeaders() })
    if (r.ok) {
      await this.load()
      await Alpine.store('modelRoles').load()
      Alpine.store('toast').show('ok', '已删除')
    }
  },

  async toggleModel(providerID, m) {
    await fetch(`/v1/providers/${providerID}/models/${m.id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', ...authHeaders() },
      body: JSON.stringify({ ...m, enabled: !m.enabled }),
    })
    await this.load()
  },

  typeLabel(t) {
    return {
      openai_compat:           'OpenAI 兼容',
      anthropic:               'Anthropic',
      google_agent_platform:   'Google',
      ollama:                  'Ollama',
    }[t] || t
  },

  roleLabel(r) {
    return { general: '通用', default: '对话', reasoning: '推理' }[r] || r
  },
})

