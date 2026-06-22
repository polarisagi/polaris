import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: modelRoles（对话模型 / 推理模型角色分配，操作 provider_models.role）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('modelRoles', {
  defaultModelID:      '',   // provider_models.id
  defaultModelName:    '',
  defaultProviderName: '',
  reasoningModelID:    '',
  reasoningModelName:  '',
  reasoningProviderName: '',
  saving: false,

  async load() {
    try {
      const r = await fetch('/v1/config/model-roles', { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      this.defaultModelID       = d.default?.model_id       || ''
      this.defaultModelName     = d.default?.model_name     || ''
      this.defaultProviderName  = d.default?.provider_name  || ''
      this.reasoningModelID     = d.reasoning?.model_id     || ''
      this.reasoningModelName   = d.reasoning?.model_name   || ''
      this.reasoningProviderName = d.reasoning?.provider_name || ''
    } catch { /* 静默 */ }
  },

  async save() {
    this.saving = true
    try {
      const r = await fetch('/v1/config/model-roles', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify({
          default_model_id:   this.defaultModelID   || null,
          reasoning_model_id: this.reasoningModelID || null,
        }),
      })
      if (r.ok) {
        await this.load()
        await Alpine.store('providers').load()
        Alpine.store('toast').show('ok', Alpine.store('i18n').t('roles_saved'))
      } else {
        Alpine.store('toast').show('error', Alpine.store('i18n').t('save_failed'))
      }
    } finally { this.saving = false }
  },
})

