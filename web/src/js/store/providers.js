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
  showCatalogModal: false,
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

  // ── 目录快速添加 + 手动创建（统一 AddModal）──────────────────────────
  catalog: [],
  catalogLoading: false,

  // 统一 Modal：内置厂商选择 or 自定义（id='__custom__'）
  showAddModal: false,
  addSelected: null,         // 选中的 CatalogProvider 或 {id:'__custom__',...}
  addForm: { api_key: '', name: '', base_url: '', type: 'openai_compat', project_id: '', location: 'us-central1' },
  addSaving: false,

  // 兼容旧代码引用（Onboard 等仍使用 catalogSelected/catalogForm）
  get catalogSelected() { return this.addSelected?.id !== '__custom__' ? this.addSelected : null },
  get catalogSaving()   { return this.addSaving },
  get showCatalogModal(){ return this.showAddModal && this.addSelected?.id !== '__custom__' },
  set showCatalogModal(v){ if (!v) this.showAddModal = false },
  get catalogForm()     { return this.addForm },

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

  async loadCatalog() {
    if (this.catalog.length > 0) return
    this.catalogLoading = true
    try {
      const r = await fetch('/v1/catalog/providers', { headers: authHeaders() })
      const d = await r.json()
      this.catalog = d.providers || []
    } catch { Alpine.store('toast').show('error', '加载厂商目录失败') }
    finally { this.catalogLoading = false }
  },

  // 打开统一的 "添加 AI 厂商" Modal
  openAdd() {
    this.addSelected = null
    this.addForm = { api_key: '', name: '', base_url: '', type: 'openai_compat', project_id: '', location: 'us-central1' }
    this.showAddModal = true
    this.loadCatalog()
  },

  // 选择目录中的一个厂商（或自定义占位符）
  selectAddEntry(entry) {
    this.addSelected = entry
    this.addForm.api_key = ''
    this.addForm.name = ''
    this.addForm.base_url = ''
  },

  // 统一保存：内置厂商走 from-catalog，自定义走手动 POST /v1/providers
  async saveAdd() {
    if (!this.addSelected) return
    this.addSaving = true
    try {
      if (this.addSelected.id === '__custom__') {
        // 手动创建
        const payload = {
          name:       this.addForm.name,
          type:       this.addForm.type,
          base_url:   this.addForm.base_url  || '',
          api_key:    this.addForm.api_key   || '',
          project_id: this.addForm.project_id || '',
          location:   this.addForm.location  || 'us-central1',
          enabled:    true,
        }
        const r = await fetch('/v1/providers', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeaders() },
          body: JSON.stringify(payload),
        })
        if (r.ok) {
          this.showAddModal = false
          await this.load()
          Alpine.store('toast').show('ok', '厂商已创建，请手动添加模型')
        } else {
          const msg = await r.text()
          Alpine.store('toast').show('error', '创建失败: ' + msg)
        }
      } else {
        // 内置目录：from-catalog 自动配置模型和角色
        const body = {
          catalog_id: this.addSelected.id,
          api_key:    this.addForm.api_key   || '',
          name:       this.addForm.name      || '',
          base_url:   this.addForm.base_url  || '',
        }
        const r = await fetch('/v1/providers/from-catalog', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeaders() },
          body: JSON.stringify(body),
        })
        if (r.ok) {
          const d = await r.json()
          this.showAddModal = false
          await this.load()
          await Alpine.store('modelRoles').load()
          // 构建成功消息：列出自动分配的角色
          const defaultM   = (d.models || []).find(m => m.role === 'default')
          const reasoningM = (d.models || []).find(m => m.role === 'reasoning')
          let msg = `已添加 ${this.addSelected.display_name}`
          if (defaultM)   msg += `，对话: ${defaultM.name || defaultM.model_id}`
          if (reasoningM) msg += `，推理: ${reasoningM.name || reasoningM.model_id}`
          Alpine.store('toast').show('ok', msg)
        } else {
          const msg = await r.text()
          Alpine.store('toast').show('error', '添加失败: ' + msg)
        }
      }
    } finally { this.addSaving = false }
  },

  // 兼容旧调用：openCreate → 打开自定义路径
  openCreate() {
    this.openAdd()
    // 延迟一帧等 Modal 打开后选中自定义
    setTimeout(() => {
      this.selectAddEntry({id:'__custom__', display_name:'自定义', models:[], is_local:false})
    }, 50)
  },

  // 兼容旧调用：openFromCatalog → 统一 openAdd
  openFromCatalog() { this.openAdd() },

  // 兼容旧调用：selectCatalogEntry → selectAddEntry
  selectCatalogEntry(entry) { this.selectAddEntry(entry) },

  // 兼容旧调用：saveFromCatalog → saveAdd
  async saveFromCatalog() { await this.saveAdd() },

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

