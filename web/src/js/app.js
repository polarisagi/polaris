import Alpine from 'alpinejs'
import { marked } from 'marked'
import { sanitizeContent, authHeaders } from './utils.js'

// Import all stores
import './store/chat.js'
import './store/statusBar.js'
import './store/nav.js'
import './store/i18n.js'
import './store/modelRoles.js'
import './store/approvals.js'
import './store/sessions.js'
import './store/skills.js'
import './store/toast.js'
import './store/providers.js'
import './store/channels.js'
import './store/onboard.js'
import './store/search.js'
import './store/cron.js'
import './store/agents.js'
import './store/insights.js'
import './store/logs.js'
import './store/config.js'
import './store/eval.js'
import './store/components.js'
import './store/computer.js'
import './store/plugins.js'

// ── Markdown 渲染配置 ──────────────────────────────────────────────────────
// marked v5+ 移除了 setOptions()，改用 marked.use()
marked.use({ gfm: true, breaks: true })

// 允许的 HTML 标签白名单（marked 输出的安全子集）
const ALLOWED_TAGS = /^(p|br|hr|b|i|em|strong|code|pre|ul|ol|li|blockquote|h[1-6]|table|thead|tbody|tr|th|td|a|span|div|del|ins)$/i
const ALLOWED_ATTRS = { a: ['href', 'title', 'target', 'rel'] }

function stripDangerousHtml(html) {
  return html
    .replace(/<script[\s\S]*?<\/script>/gi, '')
    .replace(/\son\w+\s*=\s*["'][^"']*["']/gi, '')  // onerror=, onclick= 等
    .replace(/javascript\s*:/gi, 'javascript-blocked:')
    .replace(/<a /g, '<a target="_blank" rel="noopener noreferrer" ')
}

function renderMarkdown(text) {
  if (!text) return ''
  text = sanitizeContent(text)
  if (!text) return ''
  try {
    const html = marked.parse(text)
    return stripDangerousHtml(html)
  } catch {
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\n/g, '<br>')
  }
}
window.renderMarkdown = renderMarkdown
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
  
  if (page === 'settings')   { Alpine.store('providers').load(); Alpine.store('modelRoles').load() }
  if (page === 'sessions')   Alpine.store('sessions').load()
  if (page === 'skills')     Alpine.store('skills').load()
  if (page === 'plugins')    Alpine.store('plugins').load()
  if (page === 'automation') { Alpine.store('cron').load(); Alpine.store('approvals').startPolling() }
  if (page === 'eval')       { void 0 }

  // 首次配置引导（延迟 400ms 等 Alpine reactive 系统就绪）
  setTimeout(() => Alpine.store('onboard').checkFirstRun(), 400)

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
    Alpine.store('nav').page = mapped
  })
})
