import Alpine from 'alpinejs'
import './net.js' // 必须最先：它替换 window.fetch，之后所有 store 的请求才经过 401 自愈
import { marked } from 'marked'
import { sanitizeContent, authHeaders } from './utils.js'

// Import all stores
import './store/chat.js'
import './store/statusBar.js'
import './store/nav.js'
import './store/i18n.js'
import './store/modelRoles.js'
import './store/approvals.js'
import './store/elicitations.js'
import './store/sessions.js'
import './store/projects.js'
import './store/skills.js'
import './store/toast.js'
import './store/dialog.js'
import './store/providers.js'
import './store/channels.js'
import './store/onboard.js'
import './store/search.js'
import './store/cron.js'
import './store/workflow.js'
import './store/agents.js'
import './store/experts.js'
import './store/insights.js'
import './store/logs.js'
import './store/config.js'
import './store/mesh.js'
import './store/eval.js'
import './store/components.js'
import './store/computer.js'
import './store/plugins.js'
import './store/update.js'
import './store/audio.js'
import './mcp_apps.js'

import { sanitizeHtml } from './sanitize.mjs'

// ── Markdown 渲染配置 ──────────────────────────────────────────────────────
// marked v5+ 移除了 setOptions()，改用 marked.use()
marked.use({ gfm: true, breaks: true })

function renderMarkdown(text, options = {}) {
  if (!text) return ''
  text = sanitizeContent(text)
  if (!text) return ''
  try {
    const html = marked.parse(text)
    return sanitizeHtml(html, options)
  } catch {
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\n/g, '<br>')
  }
}

function renderDocMarkdown(text, baseUrl) {
  if (!text) return ''
  try {
    const html = marked.parse(text)
    return sanitizeHtml(html, { baseUrl, allowImages: false })
  } catch {
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\n/g, '<br>')
  }
}

window.renderMarkdown = renderMarkdown
window.renderDocMarkdown = renderDocMarkdown
window.Alpine = Alpine

// ── 主题切换（dark | light | terminal | system）────────────────────────────
const _mq = window.matchMedia('(prefers-color-scheme: dark)')
_mq.addEventListener('change', () => {
  if (localStorage.getItem('polaris_theme') === 'system') applyTheme('system')
})

function applyTheme(theme) {
  const root = document.documentElement
  if (theme === 'system') {
    root.dataset.theme = _mq.matches ? '' : 'light'
  } else if (theme === 'light') {
    root.dataset.theme = 'light'
  } else if (theme === 'terminal') {
    root.dataset.theme = 'terminal'
  } else {
    root.dataset.theme = '' // dark
  }
  localStorage.setItem('polaris_theme', theme)
}
applyTheme(localStorage.getItem('polaris_theme') || 'system')
window.applyTheme = applyTheme

// ── 视口高度修正（macOS Chrome 窗口模式下 100vh 可能超出可视区域）──────────
function fixViewportHeight() {
  document.documentElement.style.setProperty('--app-height', window.innerHeight + 'px')
}
window.addEventListener('resize', fixViewportHeight)
fixViewportHeight()

Alpine.start()

// 状态栏开始轮询
document.addEventListener('DOMContentLoaded', () => {
  Alpine.store('statusBar').startPolling()
  Alpine.store('audio').refresh()
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      Alpine.store('audio').refresh()
    }
  })

  const rawPath = location.pathname.replace(/^\//, '') || 'chat'
  // 兼容旧 URL → 新页面
  const legacyPageMap = {
    status: 'monitor', insights: 'monitor', logs: 'monitor',
    providers: 'settings', channels: 'settings', config: 'settings', computer: 'settings',
    approvals: 'automation', cron: 'automation',
    agents: 'monitor', capabilities: 'skills',
  }
  const page = legacyPageMap[rawPath] || rawPath
  Alpine.store('nav').page = page
  history.replaceState({}, '', page === 'chat' ? '/' : `/${page}`)
  
  if (page === 'settings' || page === 'chat') { Alpine.store('providers').load(); Alpine.store('modelRoles').load() }
  if (page === 'projects' || page === 'chat' || page === 'sessions') Alpine.store('projects').load()
  if (page === 'sessions')   Alpine.store('sessions').load()
  if (page === 'skills')     Alpine.store('skills').load()
  if (page === 'plugins')    Alpine.store('plugins').load()
  if (page === 'automation') Alpine.store('cron').load()
  Alpine.store('approvals').startPolling(page === 'automation')
  if (page === 'chat') { Alpine.store('elicitations').startPolling(); Alpine.store('chat').fetchCapabilities?.(); }
  if (page === 'eval')       { void 0 }

  // 首次配置引导（延迟 400ms 等 Alpine reactive 系统就绪）
  setTimeout(() => Alpine.store('onboard').checkFirstRun(), 400)

  // 版本检查与热更新（30s 后首检，6h 定时）
  Alpine.store('update').init()

  // 刷新时恢复上次会话历史
  if (page === 'chat') {
    const savedID = localStorage.getItem('polaris_session_id')
    if (savedID) {
      Alpine.store('chat').loadSession(savedID)
    }
  }

  // 监听浏览器前进/后退
  window.addEventListener('popstate', () => {
    const p = location.pathname.replace(/^\//, '') || 'chat'
    const mapped = legacyPageMap[p] || p
    const nav = Alpine.store('nav')
    nav.revision++
    nav.page = mapped
    if (mapped === 'chat') {
      const chat = Alpine.store('chat')
      if (!chat.sessionID && chat.messages.length === 0) Alpine.store('experts').load(true)
    }
  })
})
