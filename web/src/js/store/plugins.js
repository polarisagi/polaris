import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: plugins（插件目录 Catalog）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('plugins', {
  catalog: [],
  loading: false,
  syncing: false,
  filter: 'plugin',   // 'plugin' | 'mcp'（UI 名「连接器」）| 'skill' | 'marketplace'
  search: '',
  installing: {},  // catalogID → true
  uninstalling: {},
  showEnvModal: false,
  envPending: null, // { entry, envVars: {} }
  showApprovalModal: false,
  approvalPending: null, // { id: 'ext_xxx', entry: {...} }
  marketplaces: [],

  // MCP OAuth（8e-2）：serverID -> {auth_required, auth_scopes, oauth} 来自 /v1/mcp-servers，
  // 与 catalog（/v1/plugins/catalog）按 id 关联叠加渲染，两个接口的语义/字段不同不合并存储。
  mcpAuth: {},
  authorizing: {},   // serverID -> true（发起授权中）
  deauthorizing: {}, // serverID -> true（注销授权中）
  mcpServers: [],    // 全量 /v1/mcp-servers 快照（Apps 绑定下拉框用，需要 name 字段）

  // 已安装插件详情（config/apps/dependencies/agents/hooks/channels，见 §插件详情弹窗）。
  // installedIndex: entry.id（目录条目 ID，两种命名空间之一）→ pluginRow（plugins 表行，
  // 詳情类接口唯一认的 ID）。目录条目分两类来源，ID 命名空间不同：
  //   1) 市场安装：entry.id = extension_catalog.id，等于 pluginRow.catalog_id（安装时原样写入，
  //      见 internal/extension/lifecycle/plugin_installer.go savePlugin CatalogID: req.CatalogID）。
  //   2) 来源直装（/v1/plugins/create）：entry.id = extension_instances.id = "ext_xxx"（见
  //      internal/gateway/server/plugin/catalog.go AppendCustomCatalogs），而 pluginRow.ID =
  //      "pl_xxx"（PluginRuntimeID 把 "ext_" 前缀换成 "pl_"，两者后缀相同）；catalog_id 为空。
  // 两个键都建进同一张表，entry.id 命中哪个都能解析到同一行，不需要调用方区分来源。
  installed: [],
  installedIndex: {},
  showDetail: false,
  detail: null,       // 当前打开详情的 pluginRow
  detailTab: 'config',
  detailLoading: {},  // tab -> true
  detailData: { config: null, apps: null, deps: null, agents: null, hooks: null, channels: null },
  detailConfigValues: {}, // scope\x00key -> 待保存的表单值（敏感项默认留空=不变更）
  detailConfigClear: {},  // scope\x00key -> true（用户勾选「清除」）
  bindingApp: {},      // alias -> true（绑定中）

  // Creation Modal State
  showCreateModal: false,
  // editingId 非空时 submitCreation() 走编辑分支（PUT 而非 POST），仅用于独立
  // （非插件内嵌）MCP 连接器——插件 MCP 的基础字段本就不允许脱离插件单独编辑。
  editingId: null,
  createForm: {
    name: '',
    description: '',
    source: '',
    mp_type: 'plugin',
    url: '',
    transport: 'stdio',
    command: '',
    args: '', // JSON array string
    env: '',  // JSON object string
    // OAuth 预注册（仅远程传输 MCP 连接器编辑时展示，可折叠区）
    oauthClientId: '',
    oauthClientSecret: '',
    oauthAuthServerMetadataUrl: '',
    oauthScopes: '', // 空格分隔
    oauthClearSecret: false,
    oauthHasClientSecret: false,
  },

  get filtered() {
    let list = this.catalog
    if (this.filter !== 'all') list = list.filter(e => (e.type || 'mcp') === this.filter)
    if (this.search.trim()) {
      const q = this.search.toLowerCase()
      list = list.filter(e =>
        e.name.toLowerCase().includes(q) ||
        e.description.toLowerCase().includes(q) ||
        (e.publisher || '').toLowerCase().includes(q) ||
        (e.tags || []).some(t => t.toLowerCase().includes(q))
      )
    }
    
    // Deduplicate by ID
    const seen = new Set()
    const uniqueList = []
    for (const item of list) {
      if (!seen.has(item.id)) {
        seen.add(item.id)
        uniqueList.push(item)
      }
    }

    return uniqueList
  },

  get filteredMarketplaces() {
    let list = this.marketplaces
    if (this.search.trim()) {
      const q = this.search.toLowerCase()
      list = list.filter(e =>
        e.name.toLowerCase().includes(q) ||
        (e.description || '').toLowerCase().includes(q) ||
        (e.publisher || '').toLowerCase().includes(q)
      )
    }
    return list
  },

  async load() {
    this.loading = true
    try {
      const [catRes, mpRes, msRes] = await Promise.all([
        fetch('/v1/plugins/catalog', { headers: authHeaders() }),
        fetch('/v1/plugins/marketplaces', { headers: authHeaders() }),
        fetch('/v1/mcp-servers', { headers: authHeaders() }),
      ])
      if (catRes.ok) {
        const d = await catRes.json()
        this.catalog = d.catalog || []
      }
      if (mpRes.ok) {
        const d = await mpRes.json()
        this.marketplaces = d.marketplaces || []
      }
      if (msRes.ok) {
        const d = await msRes.json()
        this.mcpServers = d.mcp_servers || []
        const auth = {}
        for (const s of (d.mcp_servers || [])) {
          auth[s.id] = {
            auth_required: !!s.auth_required,
            auth_scopes: s.auth_scopes || [],
            authorized: !!s.oauth_authorized,
            oauth: s.oauth || null,
            transport: s.transport,
          }
        }
        this.mcpAuth = auth
      }
    } catch { /* 静默 */ }
    try {
      const plRes = await fetch('/v1/plugins', { headers: authHeaders() })
      if (plRes.ok) {
        const d = await plRes.json()
        this.installed = d.plugins || []
        const idx = {}
        for (const p of this.installed) {
          if (p.catalog_id) idx[p.catalog_id] = p
          idx['ext_' + p.id.replace(/^pl_/, '')] = p
        }
        this.installedIndex = idx
      }
    } catch { /* 静默：详情按钮按 installedPluginFor() 返回值自然隐藏 */ }
    this.loading = false
  },

  // installedPluginFor 目录条目 → 对应的已安装 pluginRow；找不到（未安装/非 plugin 类型
  // 装成 mcp/skill 单体）返回 null，调用方据此决定是否显示「详情」入口。
  installedPluginFor(entry) {
    return this.installedIndex[entry.id] || null
  },

  async syncMarketplaces(localOnly = false) {
    this.syncing = true
    try {
      const url = localOnly ? '/v1/plugins/sync?local_only=true' : '/v1/plugins/sync'
      const r = await fetch(url, { 
        method: 'POST', 
        headers: authHeaders() 
      })
      if (r.ok) {
        const d = await r.json()
        Alpine.store('toast').show('ok', `同步完成，成功拉取 ${d.synced_count} 个项目`)
        await this.load()
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `同步失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `同步失败：${e.message}`)
    } finally {
      this.syncing = false
    }
  },

  // tryInstall: MCP 带空 env var 时弹框收集，否则直接安装
  tryInstall(entry) {
    const type = entry.type || 'mcp'
    if (type === 'mcp') {
      const required = Object.entries(entry.env || {}).filter(([, v]) => v === '')
      if (required.length > 0) {
        this.envPending = {
          entry,
          envVars: Object.fromEntries(required.map(([k]) => [k, ''])),
        }
        this.showEnvModal = true
        return
      }
    }
    this.doInstall(entry, {})
  },

  async confirmInstall() {
    const pending = this.envPending
    this.showEnvModal = false
    this.envPending = null
    if (!pending) return
    await this.doInstall(pending.entry, pending.envVars)
  },

  async doInstall(entry, env) {
    this.installing[entry.id] = true
    try {
      const body = { catalog_id: entry.id }
      if (env && Object.keys(env).length > 0) body.env = env
      const r = await fetch('/v1/plugins/install', {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (r.ok) {
        if (r.status === 202) {
          const d = await r.json()
          this.approvalPending = { id: d.id, entry }
          this.showApprovalModal = true
          return
        }
        Alpine.store('toast').show('ok', `已安装：${entry.name}`)
        await this.load()
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `安装失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `安装失败：${e.message}`)
    } finally {
      delete this.installing[entry.id]
    }
  },

  async resolveApproval(action) {
    const pending = this.approvalPending
    if (!pending) return
    this.showApprovalModal = false
    this.approvalPending = null
    try {
      const r = await fetch(`/v1/approvals/${pending.id}/resolve`, {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ action, comment: `User ${action} via UI` })
      })
      if (r.ok) {
        Alpine.store('toast').show(action === 'approve' ? 'ok' : 'info', `已${action==='approve'?'批准':'拒绝'}：${pending.entry.name}`)
        if (action === 'approve') {
          setTimeout(() => this.load(), 1500)
        }
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `操作失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `操作失败：${e.message}`)
    }
  },

  async uninstall(entry) {
    this.uninstalling[entry.id] = true
    try {
      // catalog_id 含 '/' 需 encode
      const r = await fetch(`/v1/plugins/${encodeURIComponent(entry.id)}`, {
        method: 'DELETE',
        headers: authHeaders(),
      })
      if (r.ok) {
        Alpine.store('toast').show('ok', `已卸载：${entry.name}`)
        await this.load()
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `卸载失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `卸载失败：${e.message}`)
    } finally {
      delete this.uninstalling[entry.id]
    }
  },

  async upgrade(entry) {
    await this.uninstall(entry)
    this.tryInstall(entry)
  },

  resetCreateForm() {
    this.editingId = null
    this.createForm = {
      name: '', description: '', source: '', mp_type: 'plugin', url: '', transport: 'stdio', command: '', args: '', env: '',
      oauthClientId: '', oauthClientSecret: '', oauthAuthServerMetadataUrl: '', oauthScopes: '',
      oauthClearSecret: false, oauthHasClientSecret: false,
    }
  },

  openCreateModal() {
    this.resetCreateForm()
    this.showCreateModal = true
  },

  // openEditMCPServer 打开「编辑连接器」表单（仅用于独立安装的远程传输 MCP，
  // 复用创建弹窗）。表单内含可折叠的「OAuth 预注册」区，预填自 /v1/mcp-servers
  // 已配置的 client_id/auth_server_metadata_url/scopes（不含明文/密文 secret，
  // 服务端只回 has_client_secret，见 8e-2 PUT .../oauth）。
  openEditMCPServer(entry) {
    const info = this.mcpAuth[entry.id] || {}
    const oauth = info.oauth || {}
    this.editingId = entry.id
    this.filter = 'mcp'
    this.createForm = {
      name: entry.name || '',
      description: '',
      source: '',
      mp_type: 'plugin',
      url: entry.url || '',
      transport: info.transport || entry.transport || 'stdio',
      command: entry.command || '',
      args: JSON.stringify(entry.args || []),
      env: '{}',
      oauthClientId: oauth.client_id || '',
      oauthClientSecret: '',
      oauthAuthServerMetadataUrl: oauth.auth_server_metadata_url || '',
      oauthScopes: (oauth.scopes || []).join(' '),
      oauthClearSecret: false,
      oauthHasClientSecret: !!oauth.has_client_secret,
    }
    this.showCreateModal = true
  },

  async submitCreation() {
    if (this.editingId) {
      await this.submitEditMCPServer()
      return
    }
    let endpoint = ''
    let body = {}
    const filter = this.filter

    try {
      if (filter === 'skill') {
        endpoint = '/v1/skills/create'
        body = {
          name: this.createForm.name,
          source: this.createForm.source
        }
      } else if (filter === 'plugin') {
        endpoint = '/v1/plugins/create'
        body = {
          name: this.createForm.name,
          source: this.createForm.source
        }
      } else if (filter === 'mcp') {
        endpoint = '/v1/mcp/create'
        body = {
          name: this.createForm.name,
          transport: this.createForm.transport,
          command: this.createForm.command,
          args: this.createForm.args ? JSON.parse(this.createForm.args) : [],
          env: this.createForm.env ? JSON.parse(this.createForm.env) : {},
          url: this.createForm.url
        }
      } else if (filter === 'marketplace') {
        endpoint = '/v1/plugins/marketplaces'
        body = {
          name: this.createForm.name,
          description: this.createForm.description,
          type: this.createForm.mp_type,
          publisher: 'user',
          repo_url: this.createForm.url
        }
      }

      const r = await fetch(endpoint, {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      })

      if (r.ok) {
        Alpine.store('toast').show('ok', `已创建：${this.createForm.name}`)
        this.showCreateModal = false
        await this.load()
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `创建失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `创建失败：${e.message}`)
    }
  },

  // submitEditMCPServer 编辑已安装的独立 MCP 连接器：先 PUT 基础字段，
  // 远程传输时再 PUT OAuth 预注册字段（stdio 无 OAuth 语义，跳过第二步）。
  async submitEditMCPServer() {
    const id = this.editingId
    const f = this.createForm
    try {
      const r = await fetch(`/v1/mcp-servers/${encodeURIComponent(id)}`, {
        method: 'PUT',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: f.name,
          transport: f.transport,
          command: f.command,
          args: f.args ? JSON.parse(f.args) : [],
          env: f.env ? JSON.parse(f.env) : {},
          url: f.url,
          enabled: true,
        }),
      })
      if (!r.ok) {
        const t = await r.text()
        Alpine.store('toast').show('error', `保存失败：${t}`)
        return
      }
      if (f.transport !== 'stdio') {
        if (f.oauthAuthServerMetadataUrl && !f.oauthAuthServerMetadataUrl.startsWith('https://')) {
          Alpine.store('toast').show('error', '授权服务器元数据 URL 必须使用 https')
          return
        }
        const or = await fetch(`/v1/mcp-servers/${encodeURIComponent(id)}/oauth`, {
          method: 'PUT',
          headers: { ...authHeaders(), 'Content-Type': 'application/json' },
          body: JSON.stringify({
            client_id: f.oauthClientId,
            auth_server_metadata_url: f.oauthAuthServerMetadataUrl,
            scopes: (f.oauthScopes || '').split(/\s+/).filter(Boolean),
            client_secret: f.oauthClearSecret ? null : f.oauthClientSecret,
          }),
        })
        if (!or.ok) {
          const t = await or.text()
          Alpine.store('toast').show('error', `OAuth 配置保存失败：${t}`)
          return
        }
      }
      Alpine.store('toast').show('ok', `已保存：${f.name}`)
      this.showCreateModal = false
      this.resetCreateForm()
      await this.load()
    } catch (e) {
      Alpine.store('toast').show('error', `保存失败：${e.message}`)
    }
  },

  // authorizeConnector 发起一次 MCP OAuth 授权：弹出授权服务器页面，通过
  // postMessage（回调页 window.opener.postMessage）或弹窗关闭两种信号感知完成，
  // 随后刷新列表。origin 校验防止其它窗口伪造消息触发误刷新。
  async authorizeConnector(entry) {
    this.authorizing[entry.id] = true
    try {
      const r = await fetch(`/v1/mcp-servers/${encodeURIComponent(entry.id)}/oauth/authorize`, {
        method: 'POST',
        headers: authHeaders(),
      })
      if (!r.ok) {
        const t = await r.text()
        Alpine.store('toast').show('error', `发起授权失败：${t}`)
        return
      }
      const d = await r.json()
      const popup = window.open(d.authorization_url, 'polaris-oauth', 'popup,width=600,height=720')
      let settled = false
      const finish = () => {
        if (settled) return
        settled = true
        window.removeEventListener('message', onMessage)
        clearInterval(poll)
        this.load()
      }
      const onMessage = (event) => {
        if (event.origin !== location.origin) return
        if (!event.data || event.data.type !== 'polaris-mcp-oauth') return
        finish()
      }
      window.addEventListener('message', onMessage)
      const poll = setInterval(() => {
        if (!popup || popup.closed) finish()
      }, 1000)
    } catch (e) {
      Alpine.store('toast').show('error', `发起授权失败：${e.message}`)
    } finally {
      delete this.authorizing[entry.id]
    }
  },

  // deauthorizeConnector 注销：删除已获取的令牌并触发重连，重连会因无令牌
  // 重新置为「需要授权」（见网关 DELETE .../oauth/token 注释）。
  async deauthorizeConnector(entry) {
    this.deauthorizing[entry.id] = true
    try {
      const r = await fetch(`/v1/mcp-servers/${encodeURIComponent(entry.id)}/oauth/token`, {
        method: 'DELETE',
        headers: authHeaders(),
      })
      if (r.ok) {
        Alpine.store('toast').show('ok', `已注销授权：${entry.name}`)
        await this.load()
      } else {
        const t = await r.text()
        Alpine.store('toast').show('error', `注销失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `注销失败：${e.message}`)
    } finally {
      delete this.deauthorizing[entry.id]
    }
  },

  // ── 插件详情弹窗：config / apps / dependencies / agents / hooks / channels ──

  openDetail(pluginRow) {
    this.detail = pluginRow
    this.detailTab = 'config'
    this.detailData = { config: null, apps: null, deps: null, agents: null, hooks: null, channels: null }
    this.detailConfigValues = {}
    this.detailConfigClear = {}
    this.showDetail = true
    this.loadDetailTab('config')
  },

  switchDetailTab(tab) {
    this.detailTab = tab
    if (!this.detailData[tab]) this.loadDetailTab(tab)
  },

  async loadDetailTab(tab) {
    if (!this.detail) return
    this.detailLoading[tab] = true
    try {
      if (tab === 'config') await this._loadDetailConfig()
      else if (tab === 'apps') await this._loadDetailApps()
      else if (tab === 'deps') await this._loadDetailDeps()
      else if (tab === 'agents') await this._loadDetailAgents()
      else if (tab === 'hooks') await this._loadDetailHooks()
      else if (tab === 'channels') await this._loadDetailChannels()
    } catch (e) {
      Alpine.store('toast').show('error', `加载失败：${e.message}`)
    } finally {
      delete this.detailLoading[tab]
    }
  },

  async _loadDetailConfig() {
    const r = await fetch(`/v1/plugins/${encodeURIComponent(this.detail.id)}/config`, { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    this.detailData.config = await r.json()
  },

  async _loadDetailApps() {
    const r = await fetch(`/v1/plugins/${encodeURIComponent(this.detail.id)}/apps`, { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    this.detailData.apps = d.apps || []
  },

  async _loadDetailDeps() {
    const r = await fetch(`/v1/plugins/${encodeURIComponent(this.detail.id)}/dependencies`, { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    this.detailData.deps = d.dependencies || []
  },

  // _loadDetailAgents GET /v1/agents 是全局列表（子 Agent 未按插件建索引），按
  // source === "plugin:<id>" 客户端过滤。
  async _loadDetailAgents() {
    const r = await fetch('/v1/agents', { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    const want = `plugin:${this.detail.id}`
    this.detailData.agents = (d.agents || []).filter(a => a.source === want)
  },

  // _loadDetailHooks GET /v1/hooks 同样是全局列表，按 plugin_id 客户端过滤。
  async _loadDetailHooks() {
    const r = await fetch('/v1/hooks', { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    this.detailData.hooks = (d.sources || []).filter(s => s.plugin_id === this.detail.id)
  },

  // _loadDetailChannels GET /v1/plugins/channels 同样是全局列表，按 plugin_id 客户端过滤。
  async _loadDetailChannels() {
    const r = await fetch('/v1/plugins/channels', { headers: authHeaders() })
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    this.detailData.channels = (d.channels || []).filter(c => c.plugin_id === this.detail.id)
  },

  // configValueKey scope 可能为空串（插件级选项），与 key 之间需要分隔符防止
  // "a"+"bc" 与 "ab"+"c" 撞键；\x00 在合法 scope/key 取值中不可能出现。
  configValueKey(scope, key) { return `${scope}\x00${key}` },

  // configFieldModel 表单双向绑定的取值：用户本次编辑过的值优先；否则用已保存的值
  // （非敏感项）；敏感项已设置时不回显明文，留空表示"不变更"。saved.value / opt.default
  // 都是 Go json.RawMessage 字段，经 fetch().json() 后已是原生 JS 值（数字/字符串/布尔/
  // 数组），不需要（也不能）再 JSON.parse 一次。
  configFieldModel(scope, opt) {
    const k = this.configValueKey(scope, opt.key)
    if (k in this.detailConfigValues) return this.detailConfigValues[k]
    if (opt.sensitive) return ''
    const saved = (this.detailData.config?.values || []).find(v => v.scope === scope && v.key === opt.key)
    if (saved && saved.value !== undefined) return saved.value
    if (opt.default !== undefined) return opt.default
    return opt.type === 'boolean' ? false : (opt.multiple ? [] : '')
  },
  setConfigField(scope, key, value) {
    this.detailConfigValues[this.configValueKey(scope, key)] = value
  },
  isConfigSet(scope, key) {
    return !!(this.detailData.config?.values || []).find(v => v.scope === scope && v.key === key && v.is_set)
  },

  // saveDetailConfig 只提交本次编辑过的字段（detailConfigValues）与显式清除的字段
  // （detailConfigClear）；未触碰的敏感项留在服务端原值不变（PUT 语义：未出现在
  // 更新集合中的敏感项保留原值，见 lifecycle.ConfigUpdate 注释）。
  async saveDetailConfig() {
    const updates = []
    for (const [k, cleared] of Object.entries(this.detailConfigClear)) {
      if (!cleared) continue
      const [scope, key] = k.split('\x00')
      updates.push({ scope, key, value: null })
    }
    for (const [k, value] of Object.entries(this.detailConfigValues)) {
      if (this.detailConfigClear[k]) continue
      const [scope, key] = k.split('\x00')
      // value 直接嵌入外层 JSON.stringify（不再手动 JSON.stringify 一次）：
      // 后端 ConfigUpdate.Value 是 json.RawMessage，按 option.Type 原生反序列化校验，
      // 若这里先转成字符串会把布尔/数字统统包成 JSON 字符串，类型校验必然失败。
      updates.push({ scope, key, value })
    }
    if (updates.length === 0) {
      Alpine.store('toast').show('ok', '没有改动')
      return
    }
    this.detailLoading.config = true
    try {
      const r = await fetch(`/v1/plugins/${encodeURIComponent(this.detail.id)}/config`, {
        method: 'PUT',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ values: updates }),
      })
      if (!r.ok) throw new Error(await r.text())
      Alpine.store('toast').show('ok', '配置已保存')
      this.detailConfigValues = {}
      this.detailConfigClear = {}
      await this._loadDetailConfig()
    } catch (e) {
      Alpine.store('toast').show('error', `保存失败：${e.message}`)
    } finally {
      delete this.detailLoading.config
    }
  },

  async bindApp(alias, serverID) {
    this.bindingApp[alias] = true
    try {
      const r = await fetch(`/v1/plugins/${encodeURIComponent(this.detail.id)}/apps/${encodeURIComponent(alias)}`, {
        method: 'PUT',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_id: serverID }),
      })
      if (!r.ok) throw new Error(await r.text())
      Alpine.store('toast').show('ok', serverID ? `已绑定 ${alias}` : `已解绑 ${alias}`)
      await this._loadDetailApps()
    } catch (e) {
      Alpine.store('toast').show('error', `绑定失败：${e.message}`)
    } finally {
      delete this.bindingApp[alias]
    }
  },

  async trustHook(src) {
    try {
      const r = await fetch('/v1/hooks/trust', {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: src.key, digest: src.digest }),
      })
      if (!r.ok) throw new Error(await r.text())
      Alpine.store('toast').show('ok', `已信任：${src.key}`)
      await this._loadDetailHooks()
    } catch (e) {
      Alpine.store('toast').show('error', `信任失败：${e.message}`)
    }
  },

  async revokeHookTrust(src) {
    try {
      const r = await fetch(`/v1/hooks/trust?key=${encodeURIComponent(src.key)}`, { method: 'DELETE', headers: authHeaders() })
      if (!r.ok) throw new Error(await r.text())
      Alpine.store('toast').show('ok', `已撤销信任：${src.key}`)
      await this._loadDetailHooks()
    } catch (e) {
      Alpine.store('toast').show('error', `撤销失败：${e.message}`)
    }
  },

  async toggleChannel(ch) {
    try {
      const r = await fetch('/v1/plugins/channels', {
        method: 'PUT',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ plugin_id: ch.plugin_id, server: ch.server, enabled: !ch.enabled, permission_relay: ch.permission_relay }),
      })
      if (!r.ok) throw new Error(await r.text())
      await this._loadDetailChannels()
    } catch (e) {
      Alpine.store('toast').show('error', `切换失败：${e.message}`)
    }
  },

  async togglePermissionRelay(ch) {
    try {
      const r = await fetch('/v1/plugins/channels', {
        method: 'PUT',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ plugin_id: ch.plugin_id, server: ch.server, enabled: ch.enabled, permission_relay: !ch.permission_relay }),
      })
      if (!r.ok) throw new Error(await r.text())
      await this._loadDetailChannels()
    } catch (e) {
      Alpine.store('toast').show('error', `切换失败：${e.message}`)
    }
  },

  depStateBadge(state) {
    return { ok: 'badge-success', missing: 'badge-error', disabled: 'badge-warning', version_mismatch: 'badge-warning' }[state] || 'badge-ghost'
  },

  async prunePlugins() {
    try {
      const r = await fetch('/v1/plugins/prune', { method: 'POST', headers: authHeaders() })
      if (!r.ok) throw new Error(await r.text())
      const d = await r.json()
      const n = (d.pruned || []).length
      Alpine.store('toast').show('ok', n > 0 ? `已清理 ${n} 个孤儿依赖插件` : '没有可清理的插件')
      if (n > 0) await this.load()
    } catch (e) {
      Alpine.store('toast').show('error', `清理失败：${e.message}`)
    }
  },

  typeLabel(type) {
    return { mcp: '连接器', skill: '技能', plugin: '插件', marketplace: '市场' }[type] || type || '插件'
  },
  typeColor(type) {
    return { mcp: '#3b82f6', skill: '#8b5cf6', plugin: '#f59e0b', marketplace: '#ec4899' }[type] || '#3b82f6'
  },
  typeIcon(type) {
    return { mcp: '🔌', skill: '⚡', plugin: '📦', marketplace: '🛒' }[type] || '📦'
  },

  trustLabel(tier) {
    return (['不可信', '本地', '社区', '官方认证', '系统内置'])[tier] ?? '未知'
  },
  trustColor(tier) {
    return [
      'var(--color-error)',
      'var(--color-text-dim)',
      '#f59e0b',
      'var(--color-ok)',
      'var(--color-accent)',
    ][tier] ?? 'var(--color-text-dim)'
  },

  publisherIcon(publisher) {
    return ({
      openai: '🔷',
      anthropic: '🟣',
      google: '🔴',
      modelcontextprotocol: '🔌',
      github: '🐙',
      microsoft: '🪟',
      figma: '🎨',
    })[publisher] || '🌐'
  },
})
