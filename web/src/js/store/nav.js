import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: nav（页面路由状态）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('nav', {
  page: 'chat',   // chat | projects | sessions | search | eval | monitor | settings | tasks | skills | plugins | automation
  revision: 0,
  navigate(page) {
    this.revision++
    this.page = page
    history.pushState({}, '', page === 'chat' ? '/' : `/${page}`)
    if (page === 'settings' || page === 'chat') { Alpine.store('providers').load(); Alpine.store('modelRoles').load() }
    if (page === 'projects' || page === 'chat') Alpine.store('projects').load()
    if (page === 'skills')     Alpine.store('skills').load()
    if (page === 'plugins')    Alpine.store('plugins').load()
    if (page === 'automation') Alpine.store('cron').load()
    Alpine.store('approvals').setFast(page === 'automation')
    if (page === 'chat') {
      Alpine.store('elicitations').startPolling()
      const chat = Alpine.store('chat')
      if (!chat.sessionID && chat.messages.length === 0) Alpine.store('experts').load(true)
    } else {
      Alpine.store('elicitations').stopPolling()
    }
    // logs 由浮动抽屉控制，不受 nav 干预
  },
})

