import Alpine from 'alpinejs'
import { authHeaders } from './utils.js'

// ============================================================================
// MCP Apps 宿主桥（M8f-2，io.modelcontextprotocol/ui）。
//
// 职责：管理每个 MCP Apps 视图对应的"外层沙箱代理 iframe"（web/src/mcp-apps/
// sandbox.html，异源，见 apps_spec.mdx §Sandbox proxy），实现 Host 侧的
// ui/initialize 应答、Host<->View 标准 MCP 消息子集转发、MCP Apps 专属消息
// （ui/open-link、ui/message、ui/request-display-mode、ui/update-model-context、
// ui/notifications/size-changed 等）。
//
// 安全边界（本文件内）：
//   - 收到的每条 postMessage 都先核对 event.source === 对应 view.iframe.contentWindow
//     且 event.origin === 该 view 的沙箱源，不满足直接丢弃。
//   - 发往沙箱代理 iframe 的消息一律带精确 targetOrigin（沙箱源），不使用 '*'。
//   - 来自 MCP 服务器的字符串（服务器名、工具名、URL、消息文本）在确认对话框里
//     只经浏览器原生 confirm()/window.open() 展示——原生对话框只渲染纯文本，
//     不解释 HTML，天然满足"一律 textContent"的约束，不走 innerHTML 拼接。
// ============================================================================

const PROTOCOL_VERSION = '2026-01-26'
const HOST_DISPLAY_MODES = ['inline', 'fullscreen'] // 本宿主不支持 pip
const TEARDOWN_TIMEOUT_MS = 2000
const DEFAULT_IFRAME_HEIGHT = 200

// daisyui 运行时注入的主题 CSS 变量 → apps_spec.mdx §Theming 标准化变量名，
// 仅映射存在合理对应关系的子集（规范允许 Host 只提供部分变量，View 侧应有回退）。
const STYLE_VAR_MAP = {
  '--color-background-primary': '--color-base-100',
  '--color-background-secondary': '--color-base-200',
  '--color-background-tertiary': '--color-base-300',
  '--color-background-inverse': '--color-neutral',
  '--color-background-info': '--color-info',
  '--color-background-danger': '--color-error',
  '--color-background-success': '--color-success',
  '--color-background-warning': '--color-warning',
  '--color-text-primary': '--color-base-content',
  '--color-text-inverse': '--color-neutral-content',
  '--color-text-info': '--color-info-content',
  '--color-text-danger': '--color-error-content',
  '--color-text-success': '--color-success-content',
  '--color-text-warning': '--color-warning-content',
  '--color-border-primary': '--color-base-300',
  '--color-ring-primary': '--color-primary',
  '--border-radius-sm': '--radius-selector',
  '--border-radius-md': '--radius-field',
  '--border-radius-lg': '--radius-box',
  '--border-width-regular': '--border',
}

function hostVersion() {
  const store = Alpine.store('update')
  return (store && store.current) || 'dev'
}

function t(key, fallback) {
  const store = Alpine.store('i18n')
  const v = store ? store.t(key) : key
  return v === key && fallback ? fallback : v
}

// getCurrentTheme 用 color-scheme 计算属性推断实际生效的明暗主题，不依赖
// document.documentElement.dataset.theme 的具体取值——daisyui 多主题（light/
// dark/nord/corporate）与 data-theme 的映射关系是 app.js 的内部实现细节，
// color-scheme 是浏览器据当前生效样式计算出的语义化结果，天然兼容任何主题。
function getCurrentTheme() {
  try {
    const cs = getComputedStyle(document.documentElement).colorScheme
    if (cs && cs.indexOf('dark') !== -1) return 'dark'
    if (cs && cs.indexOf('light') !== -1) return 'light'
  } catch { /* ignore */ }
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function hostStyleVariables() {
  const cs = getComputedStyle(document.documentElement)
  const vars = {}
  for (const [specKey, localVar] of Object.entries(STYLE_VAR_MAP)) {
    const v = cs.getPropertyValue(localVar).trim()
    if (v) vars[specKey] = v
  }
  const fontSans = getComputedStyle(document.body).fontFamily
  if (fontSans) vars['--font-sans'] = fontSans.trim()
  vars['--font-mono'] = "'JetBrains Mono', 'Fira Code', ui-monospace, monospace"
  return vars
}

// toWireCSP / toWirePermissions 把 GET /v1/mcp-apps/resource 返回的 Go 结构体
// 默认序列化（无 json tag，字段名原样大写：ConnectDomains/Camera/...）转换成
// apps_spec.mdx 线上协议要求的驼峰字段名 + presence-based 权限对象。
function toWireCSP(csp) {
  const out = {}
  if (csp && Array.isArray(csp.ConnectDomains) && csp.ConnectDomains.length) out.connectDomains = csp.ConnectDomains
  if (csp && Array.isArray(csp.ResourceDomains) && csp.ResourceDomains.length) out.resourceDomains = csp.ResourceDomains
  if (csp && Array.isArray(csp.FrameDomains) && csp.FrameDomains.length) out.frameDomains = csp.FrameDomains
  if (csp && Array.isArray(csp.BaseURIDomains) && csp.BaseURIDomains.length) out.baseUriDomains = csp.BaseURIDomains
  return out
}

function toWirePermissions(perm) {
  const out = {}
  if (perm && perm.Camera) out.camera = {}
  if (perm && perm.Microphone) out.microphone = {}
  if (perm && perm.Geolocation) out.geolocation = {}
  if (perm && perm.ClipboardWrite) out.clipboardWrite = {}
  return out
}

function safeParseJSON(text, fallback) {
  if (text === undefined || text === null || text === '') return fallback
  if (typeof text !== 'string') return text
  try {
    return JSON.parse(text)
  } catch {
    return fallback
  }
}

// McpAppView 单个 MCP Apps 视图实例：一个外层沙箱代理 iframe + 其生命周期状态。
// widgetStateMeta 已保存的 widgetState（库列缺省 '{}' 视为从未保存）→ ui/initialize 结果 _meta。
function widgetStateMeta(state) {
  if (state === undefined || state === null) return undefined
  if (typeof state === 'object' && !Array.isArray(state) && Object.keys(state).length === 0) return undefined
  return { 'polaris/widgetState': state }
}

class McpAppView {
  constructor(host, container, ref, sandboxOrigin, sessionID) {
    this.host = host
    this.container = container
    this.ref = ref // {viewId, serverId, resourceUri, toolName, toolInput, toolResult, cancelled}
    this.sandboxOrigin = sandboxOrigin
    this.sessionID = sessionID
    this.iframe = null
    this.resource = null
    this.initialized = false
    this.torndown = false
    this.displayMode = 'inline'
    this.appCapabilities = {}
    this.pending = new Map() // id -> {resolve, reject}
    this._reqSeq = 0
    this._fullscreenOverlay = null
    this._onEsc = this._onEsc.bind(this)
  }

  async mount() {
    this.iframe = document.createElement('iframe')
    this.iframe.setAttribute('sandbox', 'allow-scripts allow-same-origin allow-forms')
    this.iframe.setAttribute('title', 'MCP App: ' + this.ref.toolName)
    this.iframe.style.cssText = 'display:block;width:100%;border:0;height:' + DEFAULT_IFRAME_HEIGHT + 'px;'
    const src = this.sandboxOrigin + '/sandbox.html?host=' + encodeURIComponent(location.origin)
    this.iframe.src = src

    this.container.classList.add('overflow-hidden', 'rounded-2xl', 'bg-base-100')
    this.container.appendChild(this.iframe)

    // 与沙箱代理并行发起资源读取，代理就绪后直接可用，不必等待往返串行。
    this._resourcePromise = this._fetchResource().catch((err) => {
      console.warn('[mcp-apps] resource fetch failed', err)
      return null
    })

    this._proxyReadyTimer = setTimeout(() => {
      if (!this.resource) this._renderError(t('mcp_apps_load_failed', '视图加载失败'))
    }, 8000)
  }

  async _fetchResource() {
    const q = new URLSearchParams({ server_id: this.ref.serverId, uri: this.ref.resourceUri })
    const r = await fetch('/v1/mcp-apps/resource?' + q.toString(), { headers: authHeaders() })
    if (!r.ok) throw new Error('HTTP ' + r.status)
    const d = await r.json()
    this.resource = d
    this.ref.prefersBorder = d.prefers_border
    // prefersBorder: nil/undefined => 宿主自行决定（默认加边框）；显式 false 才不加。
    if (d.prefers_border !== false) {
      this.container.classList.add('border', 'border-base-300')
    }
    return d
  }

  _renderError(msg) {
    this.container.textContent = ''
    const p = document.createElement('div')
    p.className = 'p-3 text-xs text-base-content/60'
    p.textContent = msg
    this.container.appendChild(p)
    if (this.iframe && this.iframe.parentNode === this.container) {
      this.container.removeChild(this.iframe)
    }
  }

  // _handleMessage 分派来自"本视图对应沙箱代理 iframe"的消息（调用方已校验
  // event.source / event.origin，见 McpAppsHost._onMessage）。
  async _handleMessage(data) {
    if (!data || data.jsonrpc !== '2.0') return

    if (data.method === 'ui/notifications/sandbox-proxy-ready') {
      const resource = await this._resourcePromise
      clearTimeout(this._proxyReadyTimer)
      if (!resource) {
        this._renderError(t('mcp_apps_load_failed', '视图加载失败'))
        return
      }
      this._postToProxy({
        jsonrpc: '2.0',
        method: 'ui/notifications/sandbox-resource-ready',
        params: {
          html: resource.html,
          csp: toWireCSP(resource.csp),
          permissions: toWirePermissions(resource.permissions),
        },
      })
      return
    }

    // Host 自己发起的请求（如 ui/resource-teardown）的响应。
    if (data.id !== undefined && this.pending.has(data.id)) {
      const p = this.pending.get(data.id)
      this.pending.delete(data.id)
      if (data.error) p.reject(data.error)
      else p.resolve(data.result)
      return
    }

    if (!data.method) return
    if (data.id !== undefined) {
      this._handleRequest(data.id, data.method, data.params || {})
    } else {
      this._handleNotification(data.method, data.params || {})
    }
  }

  _handleRequest(id, method, params) {
    switch (method) {
      case 'ui/initialize':
        this.appCapabilities = params.appCapabilities || {}
        this._respond(id, this._buildInitializeResult())
        break
      case 'tools/call':
      case 'resources/read':
      case 'ping':
        this._proxyRPC(id, method, params)
        break
      case 'ui/open-link':
        this._handleOpenLink(id, params)
        break
      case 'ui/message':
        this._handleUIMessage(id, params)
        break
      case 'ui/request-display-mode':
        this._handleRequestDisplayMode(id, params)
        break
      case 'ui/update-model-context':
        this._handleUpdateModelContext(id, params)
        break
      default:
        this._respondError(id, -32601, 'method not found: ' + method)
    }
  }

  _handleNotification(method, params) {
    switch (method) {
      case 'ui/notifications/initialized':
        this._onInitialized()
        break
      case 'ui/notifications/size-changed':
        this._handleSizeChanged(params)
        break
      case 'notifications/message':
        // 服务器日志消息：仅 console 记录，带服务器名前缀，不进入页面 DOM。
        console.info('[mcp-apps:' + this.ref.serverId + ']', params && params.level, params && params.data)
        break
      case 'polaris/widget-state':
        this._handleWidgetState(params)
        break
      default:
        // 未知通知：静默忽略（apps_spec.mdx 标准消息子集之外的通知本宿主不处理）。
        break
    }
  }

  _buildInitializeResult() {
    const rect = this.container.getBoundingClientRect()
    const width = Math.max(Math.round(rect.width), 1)
    const maxHeight = Math.round(window.innerHeight * 0.7)
    this._lastMaxHeight = maxHeight
    return {
      protocolVersion: PROTOCOL_VERSION,
      // ChatGPT widgetState 回放：规范无对应字段，放在 MCP 保留的 _meta 扩展槽（带命名空间），
      // 标准 View 忽略即可；window.openai 垫片据此恢复刷新前 setWidgetState 保存的值。
      _meta: widgetStateMeta(this.ref.widgetState),
      hostInfo: { name: 'polaris', version: hostVersion() },
      hostCapabilities: {
        openLinks: {},
        serverTools: {},
        serverResources: {},
        logging: {},
        sandbox: {
          permissions: toWirePermissions(this.resource && this.resource.permissions),
          csp: toWireCSP(this.resource && this.resource.csp),
        },
      },
      hostContext: {
        toolInfo: { tool: { name: this.ref.toolName } },
        theme: getCurrentTheme(),
        styles: { variables: hostStyleVariables() },
        displayMode: this.displayMode,
        availableDisplayModes: HOST_DISPLAY_MODES,
        containerDimensions: { width, maxHeight },
        locale: navigator.language || 'en-US',
        timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        platform: 'web',
        deviceCapabilities: {
          touch: 'ontouchstart' in window || navigator.maxTouchPoints > 0,
          hover: window.matchMedia('(hover: hover)').matches,
        },
        userAgent: 'polaris/' + hostVersion(),
      },
    }
  }

  // _onInitialized 规范强制：宿主在收到 initialized 之前不得向 View 发任何消息。
  // 只在首次 initialized 时发送 tool-input/tool-result（或 tool-cancelled），
  // 重复到达（如 View 自身也实现了标准桥、与 window.openai 垫片各自握手一次）
  // 时幂等跳过——两次握手对宿主是无害的，但工具数据只应回放一次。
  _onInitialized() {
    if (this.initialized) return
    this.initialized = true

    const args = safeParseJSON(this.ref.toolInput, {})
    this._notify('ui/notifications/tool-input', { arguments: args })

    if (this.ref.cancelled) {
      this._notify('ui/notifications/tool-cancelled', { reason: 'cancelled' })
      return
    }
    const result = safeParseJSON(this.ref.toolResult, null)
    if (result) {
      this._notify('ui/notifications/tool-result', result)
    }
  }

  async _proxyRPC(id, method, params) {
    try {
      const r = await fetch('/v1/mcp-apps/views/' + encodeURIComponent(this.ref.viewId) + '/rpc', {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ jsonrpc: '2.0', id, method, params, session_id: this.sessionID }),
      })
      if (!r.ok) {
        this._respondError(id, -32000, 'HTTP ' + r.status)
        return
      }
      const resp = await r.json()
      if (resp.error) this._respond(id, undefined, resp.error)
      else this._respond(id, resp.result)
    } catch (err) {
      this._respondError(id, -32000, String((err && err.message) || err))
    }
  }

  _handleOpenLink(id, params) {
    const url = params && params.url
    let parsed
    try {
      parsed = new URL(String(url))
    } catch {
      this._respondError(id, -32000, 'Invalid URL')
      return
    }
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
      this._respondError(id, -32000, 'Policy violation')
      return
    }
    // 原生 confirm()：浏览器只渲染纯文本，服务器/View 提供的 URL 字符串不会被
    // 当作 HTML 解释，天然满足"一律 textContent"的约束。
    const ok = window.confirm(t('mcp_apps_open_link_confirm', '允许该应用打开外部链接？') + '\n\n' + parsed.href)
    if (!ok) {
      this._respondError(id, -32000, 'Link opening denied by user')
      return
    }
    window.open(parsed.href, '_blank', 'noopener,noreferrer')
    this._respond(id, {})
  }

  _handleUIMessage(id, params) {
    const text = params && params.content && params.content.text
    if (typeof text !== 'string' || !text) {
      this._respondError(id, -32000, 'Invalid message format')
      return
    }
    const chat = Alpine.store('chat')
    if (!chat) {
      this._respondError(id, -32000, 'chat store unavailable')
      return
    }
    const label = t('mcp_apps_message_from_app', '来自应用') + ' (' + this.ref.serverId + ')'
    const ok = window.confirm(label + '\n\n' + text)
    if (!ok) {
      this._respondError(id, -32000, 'Message sending denied')
      return
    }
    if (chat.isActive) {
      this._respondError(id, -32000, 'host is busy, try again later')
      return
    }
    this._respond(id, {})
    chat.submit(text)
  }

  _handleRequestDisplayMode(id, params) {
    const requested = params && params.mode
    const hostAllows = HOST_DISPLAY_MODES.indexOf(requested) !== -1
    const declared = this.appCapabilities.availableDisplayModes
    const viewDeclares = !Array.isArray(declared) || declared.indexOf(requested) !== -1
    if (hostAllows && viewDeclares && requested !== this.displayMode) {
      this._setDisplayMode(requested)
    }
    this._respond(id, { mode: this.displayMode })
    this._notify('ui/notifications/host-context-changed', { displayMode: this.displayMode })
  }

  async _handleUpdateModelContext(id, params) {
    try {
      const r = await fetch('/v1/mcp-apps/views/' + encodeURIComponent(this.ref.viewId) + '/model-context', {
        method: 'PUT',
        headers: authHeaders(),
        body: JSON.stringify({
          session_id: this.sessionID,
          content: params.content,
          structuredContent: params.structuredContent,
        }),
      })
      if (!r.ok) {
        this._respondError(id, -32000, 'Context update denied')
        return
      }
      this._respond(id, {})
    } catch (err) {
      this._respondError(id, -32000, String((err && err.message) || err))
    }
  }

  _handleSizeChanged(params) {
    if (this.displayMode === 'fullscreen') return // fullscreen 忽略 View 自报尺寸
    const maxHeight = this._lastMaxHeight || Math.round(window.innerHeight * 0.7)
    const h = Math.max(1, Math.min(Number(params && params.height) || DEFAULT_IFRAME_HEIGHT, maxHeight))
    if (this.iframe) this.iframe.style.height = h + 'px'
  }

  _handleWidgetState(params) {
    const state = params && params.state
    this.ref.widgetState = state // 同页内重新初始化（如 View 自行重载）时回放最新值
    const body = JSON.stringify({ session_id: this.sessionID, widget_state: state === undefined ? {} : state })
    fetch('/v1/mcp-apps/views/' + encodeURIComponent(this.ref.viewId) + '/state', {
      method: 'PUT',
      headers: authHeaders(),
      body,
    }).catch((err) => console.warn('[mcp-apps] widget state persist failed', err))
  }

  _setDisplayMode(mode) {
    if (mode === 'fullscreen') this._enterFullscreen()
    else this._exitFullscreen()
    this.displayMode = mode
  }

  _enterFullscreen() {
    if (this._fullscreenOverlay || !this.iframe) return
    const overlay = document.createElement('div')
    overlay.className = 'fixed inset-0 bg-base-100 flex flex-col'
    overlay.style.zIndex = '9999'
    const bar = document.createElement('div')
    bar.className = 'flex items-center justify-end p-2 border-b border-base-300'
    const btn = document.createElement('button')
    btn.type = 'button'
    btn.className = 'btn btn-sm btn-ghost'
    btn.textContent = t('mcp_apps_exit_fullscreen', '退出全屏 (Esc)')
    btn.addEventListener('click', () => this._exitFullscreenByUser())
    bar.appendChild(btn)
    overlay.appendChild(bar)
    this.iframe.style.height = '100%'
    overlay.appendChild(this.iframe)
    document.body.appendChild(overlay)
    this._fullscreenOverlay = overlay
    document.addEventListener('keydown', this._onEsc)
  }

  _exitFullscreen() {
    if (!this._fullscreenOverlay) return
    document.removeEventListener('keydown', this._onEsc)
    this.container.appendChild(this.iframe)
    this.iframe.style.height = DEFAULT_IFRAME_HEIGHT + 'px'
    this._fullscreenOverlay.remove()
    this._fullscreenOverlay = null
  }

  _onEsc(e) {
    if (e.key === 'Escape') this._exitFullscreenByUser()
  }

  _exitFullscreenByUser() {
    if (this.displayMode !== 'fullscreen') return
    this._setDisplayMode('inline')
    this._notify('ui/notifications/host-context-changed', { displayMode: 'inline' })
  }

  hostContextChanged(partial) {
    if (!this.initialized || this.torndown) return
    this._notify('ui/notifications/host-context-changed', partial)
  }

  _postToProxy(msg) {
    if (!this.iframe || !this.iframe.contentWindow) return
    this.iframe.contentWindow.postMessage(msg, this.sandboxOrigin)
  }

  _respond(id, result, error) {
    if (id === undefined) return
    const msg = { jsonrpc: '2.0', id }
    if (error) msg.error = error
    else msg.result = result === undefined ? {} : result
    this._postToProxy(msg)
  }

  _respondError(id, code, message) {
    this._respond(id, undefined, { code, message })
  }

  _notify(method, params) {
    this._postToProxy({ jsonrpc: '2.0', method, params: params === undefined ? {} : params })
  }

  // _request Host 主动向 View 发起请求（目前只有 ui/resource-teardown 用到）。
  _request(method, params, timeoutMs) {
    const id = 'host-' + (++this._reqSeq)
    return new Promise((resolve) => {
      let settled = false
      const finish = (v) => {
        if (settled) return
        settled = true
        this.pending.delete(id)
        resolve(v)
      }
      this.pending.set(id, { resolve: finish, reject: finish })
      this._postToProxy({ jsonrpc: '2.0', id, method, params: params || {} })
      setTimeout(() => finish(undefined), timeoutMs)
    })
  }

  async teardown(reason) {
    if (this.torndown) return
    this.torndown = true
    if (this.initialized) {
      await this._request('ui/resource-teardown', { reason: reason || 'unmount' }, TEARDOWN_TIMEOUT_MS)
    }
    clearTimeout(this._proxyReadyTimer)
    if (this._fullscreenOverlay) this._fullscreenOverlay.remove()
    document.removeEventListener('keydown', this._onEsc)
    if (this.iframe && this.iframe.parentNode) this.iframe.parentNode.removeChild(this.iframe)
  }
}

// McpAppsHost 单例：持有全部存活视图，路由全局 message 事件与主题/尺寸变化通知。
class McpAppsHost {
  constructor() {
    this.config = null
    this.views = new Map()
    window.addEventListener('message', (e) => this._onMessage(e))
  }

  async ensureConfig() {
    if (this.config) return this.config
    try {
      const r = await fetch('/v1/mcp-apps/config', { headers: authHeaders() })
      if (!r.ok) throw new Error('HTTP ' + r.status)
      const d = await r.json()
      let sandboxOrigin = d.sandbox_origin
      if (!sandboxOrigin) {
        sandboxOrigin = location.protocol + '//' + location.hostname + ':' + d.sandbox_port
      }
      this.config = { enabled: !!d.enabled, sandboxOrigin }
    } catch (err) {
      console.warn('[mcp-apps] config load failed', err)
      this.config = { enabled: false, sandboxOrigin: '' }
    }
    return this.config
  }

  // mount container: DOM 容器节点（由 chat.html x-init 传入 $el）；ref: 会话
  // store 里的视图快照（既可能来自实时 SSE tool_ui 事件，也可能来自历史回放）。
  async mount(container, ref) {
    if (!container || !ref || !ref.view_id) return null
    if (this.views.has(ref.view_id)) return this.views.get(ref.view_id)

    const cfg = await this.ensureConfig()
    if (!cfg.enabled) return null // 总开关关闭：工具文本输出照常显示，不渲染视图（降级行为）

    const normalized = {
      viewId: ref.view_id,
      serverId: ref.server_id,
      resourceUri: ref.resource_uri,
      toolName: ref.tool_name,
      toolInput: ref.tool_input,
      toolResult: ref.tool_result,
      widgetState: ref.widget_state,
      cancelled: !!ref.cancelled,
    }
    const sessionID = (Alpine.store('chat') && Alpine.store('chat').sessionID) || ''
    const view = new McpAppView(this, container, normalized, cfg.sandboxOrigin, sessionID)
    this.views.set(ref.view_id, view)
    await view.mount()
    return view
  }

  async teardown(viewID, reason) {
    const view = this.views.get(viewID)
    if (!view) return
    this.views.delete(viewID)
    await view.teardown(reason)
  }

  teardownAll(reason) {
    const views = Array.from(this.views.values())
    this.views.clear()
    return Promise.all(views.map((view) => view.teardown(reason)))
  }

  notifyThemeChange() {
    const theme = getCurrentTheme()
    for (const view of this.views.values()) view.hostContextChanged({ theme })
  }

  notifyContainerChange() {
    for (const view of this.views.values()) {
      const rect = view.container.getBoundingClientRect()
      const maxHeight = Math.round(window.innerHeight * 0.7)
      view._lastMaxHeight = maxHeight
      view.hostContextChanged({ containerDimensions: { width: Math.max(Math.round(rect.width), 1), maxHeight } })
    }
  }

  _onMessage(event) {
    for (const view of this.views.values()) {
      if (event.source === view.iframe?.contentWindow && event.origin === view.sandboxOrigin) {
        view._handleMessage(event.data)
        return
      }
    }
  }
}

export const mcpAppsHost = new McpAppsHost()
window.mcpAppsHost = mcpAppsHost

// ── 全局钩子：主题切换 / 视口尺寸变化 → 广播给全部存活视图；页面卸载前尽力 teardown ──
const _themeObserver = new MutationObserver(() => mcpAppsHost.notifyThemeChange())
_themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => mcpAppsHost.notifyThemeChange())
window.addEventListener('resize', () => mcpAppsHost.notifyContainerChange())
window.addEventListener('pagehide', () => {
  for (const view of mcpAppsHost.views.values()) {
    // pagehide 阶段无法可靠等待网络往返：尽力发送 teardown 通知（不等待响应）。
    if (view.initialized && !view.torndown) {
      view.torndown = true
      view._notify('ui/resource-teardown', { reason: 'page_unload' })
    }
  }
})
