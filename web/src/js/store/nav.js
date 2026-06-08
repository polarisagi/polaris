import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: nav（页面路由状态）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('nav', {
  page: 'chat',   // chat | sessions | search | eval | monitor | settings | tasks | skills | plugins | automation
  navigate(page) {
    this.page = page
    history.pushState({}, '', page === 'chat' ? '/' : `/${page}`)
    if (page === 'settings')   { Alpine.store('providers').load(); Alpine.store('modelRoles').load() }
    if (page === 'skills')     Alpine.store('skills').load()
    if (page === 'plugins')    Alpine.store('plugins').load()
    if (page === 'automation') { Alpine.store('cron').load(); Alpine.store('approvals').startPolling() }
    else                       { Alpine.store('approvals').stopPolling() }
    // logs 由浮动抽屉控制，不受 nav 干预
  },
})

