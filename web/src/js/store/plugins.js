import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
import { readHTTPError, readHTTPErrorMessage } from '../http_error.mjs'
import { mcpAppsHost } from '../mcp_apps.js'
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

  // ── 目录覆盖层：中文 + 四维分类（ADR-0123）────────────────────────────
  // taxonomy 来自 /v1/plugins/catalog 响应：{ version, values: { topic|department|industry|life: { id: {en, zh} } } }。
  // 筛选是内存内过滤（与 search 同一模式），计数基于「类型 + 搜索 + 状态」过滤后的集合，所以数字与当前视图一致。
  taxonomy: null,
  facetKeys: ['topic', 'department', 'industry', 'life'],
  facets: { topic: '', department: '', industry: '', life: '' },
  facetExpanded: { topic: false, department: false, industry: false, life: false },
  status: '',                     // '' | 'polaris' | 'verified' | 'community' | 'installed' | 'zh'
  sortMode: 'recommended',        // 'recommended'（后端顺序：已安装→精选→官方→名称）| 'name'

  isZh() {
    return Alpine.store('i18n').lang === 'zh'
  },

  // useZh 中文界面且有中文、且上游原文未在中文生成后变更时才用中文。
  useZh(entry) {
    return this.isZh() && !entry.i18n_stale && !!(entry.name_zh || entry.summary_zh || entry.description_zh)
  },

  displayName(entry) {
    if (!entry) return ''
    if (this.useZh(entry) && entry.name_zh) return entry.name_zh
    return entry.display_name || entry.name || ''
  },

  displaySummary(entry) {
    if (!entry) return ''
    if (this.useZh(entry) && entry.summary_zh) return entry.summary_zh
    return entry.short_description || entry.description || ''
  },

  displayDescription(entry) {
    if (!entry) return ''
    if (this.useZh(entry) && entry.description_zh) return entry.description_zh
    return entry.description || ''
  },

  // marketDescription 内置市场说明按界面语言取 i18n（plugins_market_desc_<市场ID>）；无对应键（用户自建市场）时回落到数据库原文。
  marketDescription(mp) {
    if (!mp) return ''
    const i18n = Alpine.store('i18n')
    const key = 'plugins_market_desc_' + mp.id
    const text = i18n.t(key)
    return text === key ? (mp.description || '') : text
  },

  hasZh(entry) {
    return !!(entry && (entry.name_zh || entry.summary_zh || entry.description_zh))
  },

  entryFacet(entry, key) {
    const m = { topic: entry.topics, department: entry.departments, industry: entry.industries, life: entry.life }
    return m[key] || []
  },

  facetLabel(key, id) {
    const l = this.taxonomy?.values?.[key]?.[id]
    if (!l) return id
    return this.isZh() ? (l.zh || l.en) : (l.en || l.zh)
  },

  facetTitle(key) {
    return Alpine.store('i18n').t('plugins_facet_' + key)
  },

  // entryTags 卡片展示的分类徽标：优先置顶当前命中的已选维度，再按主题→部门→行业→生活依次补充。
  entryTags(entry) {
    const out = []
    const pushUnique = (key, id) => {
      if (id && !out.some(t => t.key === key && t.id === id)) out.push({ key, id })
    }
    for (const k of this.facetKeys) {
      if (this.facets[k] && this.entryFacet(entry, k).includes(this.facets[k])) {
        pushUnique(k, this.facets[k])
      }
    }
    for (const id of this.entryFacet(entry, 'topic')) pushUnique('topic', id)
    for (const k of ['department', 'industry', 'life']) {
      const first = this.entryFacet(entry, k)[0]
      if (first) pushUnique(k, first)
    }
    return out
  },

  matchStatus(entry) {
    switch (this.status) {
      case 'polaris': return entry.publisher === 'polarisagi'
      case 'verified': return (entry.trust_tier || 0) >= 3
      case 'community': return (entry.trust_tier || 0) < 3
      case 'installed': return !!entry.installed
      case 'zh': return this.hasZh(entry)
      default: return true
    }
  },

  // crossFilteredFor 在 baseFiltered 基础上，排除 targetKey 维度，叠加其它维度的已选条件以实现交叉联动。
  crossFilteredFor(targetKey) {
    let list = this.baseFiltered
    for (const k of this.facetKeys) {
      if (k !== targetKey && this.facets[k]) {
        list = list.filter(e => this.entryFacet(e, k).includes(this.facets[k]))
      }
    }
    return list
  },

  // facetCounts 基于交叉过滤集合统计某维度下各取值的数量（只返回 > 0 的，按数量降序、名称升序）。
  facetCounts(key) {
    const counts = {}
    for (const e of this.crossFilteredFor(key)) {
      for (const id of this.entryFacet(e, key)) counts[id] = (counts[id] || 0) + 1
    }
    return Object.entries(counts)
      .map(([id, count]) => ({ id, count, label: this.facetLabel(key, id) }))
      .sort((a, b) => b.count - a.count || a.label.localeCompare(b.label))
  },

  // visibleFacetItems 根据展开态返回前 10 项或完整项；若当前选中项不在列表中则保底可见，避免选中态丢失。
  visibleFacetItems(key) {
    const all = this.facetCounts(key)
    const sel = this.facets[key]
    if (this.facetExpanded[key]) {
      if (sel && !all.some(item => item.id === sel)) {
        return [...all, { id: sel, count: 0, label: this.facetLabel(key, sel) }]
      }
      return all
    }
    const top = all.slice(0, 10)
    if (sel && !top.some(item => item.id === sel)) {
      const found = all.find(item => item.id === sel) || { id: sel, count: 0, label: this.facetLabel(key, sel) }
      if (top.length >= 10) {
        top[top.length - 1] = found
      } else {
        top.push(found)
      }
    }
    return top
  },

  get hasActiveFacet() {
    return this.facetKeys.some(k => !!this.facets[k])
  },

  get activeFacets() {
    return this.facetKeys
      .filter(k => !!this.facets[k])
      .map(k => ({
        key: k,
        id: this.facets[k],
        title: this.facetTitle(k),
        label: this.facetLabel(k, this.facets[k]),
      }))
  },

  setFacet(key, value) {
    if (!key) {
      this.facets = { topic: '', department: '', industry: '', life: '' }
    } else if (!value || this.facets[key] === value) {
      this.facets[key] = ''
    } else {
      this.facets[key] = value
    }
    this.resetVisible()
  },

  setStatus(s) {
    this.status = s
    this.resetVisible()
  },

  setSort(m) {
    this.sortMode = m
    this.resetVisible()
  },

  clearFilters() {
    this.facets = { topic: '', department: '', industry: '', life: '' }
    this.status = ''
    this.search = ''
    this.resetVisible()
  },


  // MCP OAuth（8e-2）：serverID -> {auth_required, auth_scopes, oauth} 来自 /v1/mcp-servers，
  // 与 catalog（/v1/plugins/catalog）按 id 关联叠加渲染，两个接口的语义/字段不同不合并存储。
  mcpAuth: {},
  authorizing: {},   // serverID -> true（发起授权中）
  deauthorizing: {}, // serverID -> true（注销授权中）
  mcpServers: [],    // 全量 /v1/mcp-servers 快照（Apps 绑定下拉框用，需要 name 字段）

  // 插件设置向导（setupTool/statusTool，ADR-0103 私有字段；ADR-0125 在详情弹窗「设置」页呈现，不新建会话）：
  // setupBusy 为设置请求进行中的 pluginRow.id；
  // setupHints[pluginRow.id] 为"依赖插件需要先完成设置"的提示列表，随 load() 刷新。
  setupBusy: {},
  setupHints: {},
  // setupReady[pluginRow.id] = { ready: boolean, message } —— 声明了 statusTool 的插件自身就绪状态；
  // ready 未知（null）的不入表。与依赖提示共用同一轮查询，随 load() 刷新。
  setupReady: {},

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
  // 目录条目详情（未安装 / 非 plugin 类型 / 无 pluginRow 可查的条目）：纯展示目录元数据，
  // 不发请求。与上面 pluginRow 详情（config/apps/... 需已安装）是两个弹窗。
  showEntryDetail: false,
  entryDetail: null,
  detailTab: 'config',
  detailLoading: {},  // tab -> true
  detailData: { setup: null, config: null, apps: null, deps: null, agents: null, hooks: null, channels: null },
  // 「设置」页：setupView 为页内挂载的插件设置视图（POST /setup 返回）；打开期间每 5 秒刷新状态清单。
  setupView: null,
  _setupPoll: null,
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
    args: '',    // JSON array string
    env: '',     // JSON object string
    headers: '', // JSON object string（远程传输请求头，如 Authorization）
    // OAuth 预注册（仅远程传输 MCP 连接器编辑时展示，可折叠区）
    oauthClientId: '',
    oauthClientSecret: '',
    oauthAuthServerMetadataUrl: '',
    oauthScopes: '', // 空格分隔
    oauthClearSecret: false,
    oauthHasClientSecret: false,
  },

  // ── 窗口化渲染（性能）──────────────────────────────────────────────────
  // 目录条目可达数千（如社区插件市场单市场 2000+，见 M13-bis §5.6）；filter/search
  // 只是内存内过滤，真正的卡顿来自一次性把几千张卡片全部挂进 DOM（Alpine 每张卡片
  // 还各自持有若干响应式绑定）。切一次 tab 就是一次同步的几千节点创建，这才是用户
  // 感知到的"切换卡"。数据本身不分页取（一次性拉回全量、内存内过滤/搜索不受影响），
  // 只是渲染层按页窗口化——比后端分页侵入小得多，且不影响搜索能搜到全量数据。
  //
  // 切窗口只在 setFilter()/resetVisible() 里显式重置（不放进 filtered/visibleFiltered
  // 的 getter 里做隐式副作用）：Alpine 的响应式基于 @vue/reactivity，在一个被追踪读取
  // 的 getter 内部写自身依赖的属性容易造成同一次求值里"读后又写"，不同 Alpine/Vue
  // 版本下表现不一致（轻则多算一轮，重则触发递归更新告警）；显式调用没有这层不确定性。
  pageSize: 60,
  _visibleCount: 60,

  get visibleFiltered() {
    return this.filtered.slice(0, this._visibleCount)
  },

  resetVisible() {
    this._visibleCount = this.pageSize
  },

  loadMoreVisible() {
    this._visibleCount += this.pageSize
  },

  setFilter(f) {
    this.filter = f
    this.resetVisible()
  },

  // baseFiltered 类型 + 搜索 + 状态（不含分类），分类计数基于它。
  get baseFiltered() {
    let list = this.catalog
    if (this.filter !== 'all') list = list.filter(e => (e.type || 'mcp') === this.filter)
    if (this.search.trim()) {
      const q = this.search.toLowerCase()
      list = list.filter(e =>
        (e.name || '').toLowerCase().includes(q) ||
        (e.description || '').toLowerCase().includes(q) ||
        (e.publisher || '').toLowerCase().includes(q) ||
        (e.name_zh || '').toLowerCase().includes(q) ||
        (e.summary_zh || '').toLowerCase().includes(q) ||
        (e.description_zh || '').toLowerCase().includes(q) ||
        (e.tags || []).some(t => t.toLowerCase().includes(q))
      )
    }
    if (this.status) list = list.filter(e => this.matchStatus(e))

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

  get filtered() {
    let list = this.baseFiltered
    for (const k of this.facetKeys) {
      if (this.facets[k]) {
        list = list.filter(e => this.entryFacet(e, k).includes(this.facets[k]))
      }
    }
    if (this.sortMode === 'name') {
      list = [...list].sort((a, b) => this.displayName(a).localeCompare(this.displayName(b)))
    }
    return list
  },

  get filteredMarketplaces() {
    let list = this.marketplaces
    if (this.search.trim()) {
      const q = this.search.toLowerCase()
      list = list.filter(e =>
        e.name.toLowerCase().includes(q) ||
        this.marketDescription(e).toLowerCase().includes(q) ||
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
        this.taxonomy = d.taxonomy || null
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
            headers: s.headers || {},
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
    } catch { /* 静默：installedIndex 为空时 onDetail 自然回落到目录条目详情 */ }
    this.loading = false
    this.loadSetupHints() // 不阻塞列表渲染：依赖状态是附加提示
  },

  // ── 插件快捷提示 / 设置向导 ───────────────────────────────────────────

  promptsFor(entry) {
    if (!entry) return []
    return this.installedPluginFor(entry)?.defaultPrompts || []
  },

  // setupFor 返回 { row, setup }；未安装或插件未声明 setupTool 返回 null（不显示设置入口）。
  setupFor(entry) {
    const row = this.installedPluginFor(entry)
    return row?.setup?.tool ? { row, setup: row.setup } : null
  },

  // readyFor 自身卡片的就绪标记；未声明 statusTool 或状态未知返回 null（不显示）。
  readyFor(entry) {
    const row = this.installedPluginFor(entry)
    return row ? (this.setupReady[row.id] || null) : null
  },

  hintsFor(entry) {
    const row = this.installedPluginFor(entry)
    return row ? (this.setupHints[row.id] || []) : []
  },

  // reasonText 后端机器码 → 本地化文案；未知码回落到通用文案，避免界面露出内部码。
  reasonText(reason) {
    const i18n = Alpine.store('i18n')
    const key = 'plugins_setup_reason_' + reason
    const text = i18n.t(key)
    return text === key ? i18n.t('plugins_setup_reason_unknown') : text
  },

  // usePrompt 新建会话并发送该提示（与 projects.startChat 同一条"新会话 → 进入聊天页"路径）。
  usePrompt(prompt) {
    const chat = Alpine.store('chat')
    chat.newSession()
    Alpine.store('nav').navigate('chat')
    chat.submit(prompt)
  },

  // runSetup 插件卡片与依赖提示的「设置」入口：打开该插件详情弹窗的「设置」页（ADR-0125）。
  runSetup(row) {
    if (row) this.openDetail(row, 'setup')
  },

  // openSetupWizard 不经 LLM 直接触发插件的设置工具，把返回的设置视图挂在「设置」页内。
  async openSetupWizard() {
    const row = this.detail
    if (!row || this.setupBusy[row.id]) return
    this.setupBusy[row.id] = true
    try {
      const r = await fetch(`/v1/plugins/${encodeURIComponent(row.id)}/setup`, {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: '{}',
      })
      if (!r.ok) {
        const error = await readHTTPError(r)
        const reason = typeof error.payload?.reason === 'string' ? error.payload.reason : ''
        Alpine.store('toast').show('error', reason ? this.reasonText(reason) : error.message)
        return
      }
      const d = await r.json()
      // x-if 只在 null ↔ 非 null 切换时重建节点：先卸载旧视图并置空，下一帧再挂新视图。
      await this._teardownSetupView()
      await Alpine.nextTick()
      if (this.detail && this.detail.id === row.id) this.setupView = d.view
      this._loadDetailSetup()
    } catch (e) {
      Alpine.store('toast').show('error', `${Alpine.store('i18n').t('plugins_setup_failed')}: ${e.message}`)
    } finally {
      delete this.setupBusy[row.id]
    }
  },

  async _teardownSetupView() {
    const v = this.setupView
    this.setupView = null
    if (v) await mcpAppsHost.teardown(v.view_id, 'plugin_settings_closed')
  },

  // loadSetupHints 对每个已安装插件找出"声明了 setupTool 的依赖插件"。
  // 有 statusTool 的依赖才查就绪状态，ready===false 才提示；没有 statusTool 的依赖无法判断，
  // 只给设置入口、不报未就绪。依赖插件均无 setupTool 时整段跳过，避免白打一轮请求。
  async loadSetupHints() {
    const byName = {}
    for (const p of this.installed) if (p.setup?.tool) byName[p.name] = p
    if (Object.keys(byName).length === 0) { this.setupHints = {}; this.setupReady = {}; return }
    // 自身卡片与依赖提示共用 readyCache：同一插件一轮 load() 只查一次 setup-status。
    const readyCache = {}
    const statusOf = (dep) => {
      if (!dep.setup.statusTool) return Promise.resolve({ ready: null, message: '' })
      if (!(dep.id in readyCache)) {
        readyCache[dep.id] = fetch(`/v1/plugins/${encodeURIComponent(dep.id)}/setup-status`, { headers: authHeaders() })
          .then(r => (r.ok ? r.json() : null))
          .then(d => ({ ready: d && typeof d.ready === 'boolean' ? d.ready : null, message: (d && d.message) || '' }))
          .catch(() => ({ ready: null, message: '' }))
      }
      return readyCache[dep.id]
    }
    const own = {}
    await Promise.all(Object.values(byName).map(async (p) => {
      const st = await statusOf(p)
      if (st.ready !== null) own[p.id] = st
    }))
    this.setupReady = own
    const hints = {}
    for (const p of this.installed) {
      try {
        const r = await fetch(`/v1/plugins/${encodeURIComponent(p.id)}/dependencies`, { headers: authHeaders() })
        if (!r.ok) continue
        const deps = (await r.json()).dependencies || []
        const list = []
        for (const d of deps) {
          const dep = byName[d.name]
          if (!dep || dep.id === p.id) continue
          const { ready, message } = await statusOf(dep)
          if (dep.setup.statusTool && ready !== false) continue
          list.push({ name: dep.display_name || dep.name, row: dep, ready, message, hasStatus: !!dep.setup.statusTool })
        }
        if (list.length) hints[p.id] = list
      } catch { /* 静默：依赖提示是附加信息 */ }
    }
    this.setupHints = hints
  },

  // installedPluginFor 目录条目 → 对应的已安装 pluginRow；找不到（未安装/非 plugin 类型
  // 装成 mcp/skill 单体）返回 null，调用方据此决定是否显示「详情」入口。
  installedPluginFor(entry) {
    if (!entry) return null
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
        const t = await readHTTPErrorMessage(r)
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
        const t = await readHTTPErrorMessage(r)
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
        const t = await readHTTPErrorMessage(r)
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
        const t = await readHTTPErrorMessage(r)
        Alpine.store('toast').show('error', `卸载失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `卸载失败：${e.message}`)
    } finally {
      delete this.uninstalling[entry.id]
    }
  },

  // upgrade 原地升级：保留实例 ID、安装路径、插件数据与用户配置。此前是「卸载再安装」——
  // 卸载会删插件数据与配置，且卸载异步进行，紧随其后的安装会因实例仍在卸载中被拒。
  async upgrade(entry) {
    if (!entry.instance_id) return
    this.installing[entry.id] = true
    try {
      const r = await fetch(`/v1/plugins/${encodeURIComponent(entry.instance_id)}/upgrade`, {
        method: 'POST',
        headers: authHeaders(),
      })
      if (r.ok || r.status === 304) {
        Alpine.store('toast').show('ok', `已升级：${entry.name}`)
        await this.load()
      } else {
        const t = await readHTTPErrorMessage(r)
        Alpine.store('toast').show('error', `升级失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `升级失败：${e.message}`)
    } finally {
      delete this.installing[entry.id]
    }
  },

  resetCreateForm() {
    this.editingId = null
    this.createForm = {
      name: '', description: '', source: '', mp_type: 'plugin', url: '', transport: 'stdio', command: '', args: '', env: '', headers: '',
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
      headers: JSON.stringify(info.headers || {}),
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
          headers: this.createForm.headers ? JSON.parse(this.createForm.headers) : {},
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
        const t = await readHTTPErrorMessage(r)
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
          headers: f.headers ? JSON.parse(f.headers) : {},
          url: f.url,
          enabled: true,
        }),
      })
      if (!r.ok) {
        const t = await readHTTPErrorMessage(r)
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
          const t = await readHTTPErrorMessage(or)
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
        const t = await readHTTPErrorMessage(r)
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
        const t = await readHTTPErrorMessage(r)
        Alpine.store('toast').show('error', `注销失败：${t}`)
      }
    } catch (e) {
      Alpine.store('toast').show('error', `注销失败：${e.message}`)
    } finally {
      delete this.deauthorizing[entry.id]
    }
  },

  // onDetail 目录卡片「详情」统一入口：已安装的 plugin 走 pluginRow 详情（可配置），
  // 其余（未安装、skill、mcp）走目录条目详情。旧实现对有 homepage 的条目渲染
  // <a target=_blank>，桌面外壳里新窗口请求被 WebView 吞掉，表现为点击无反应。
  onDetail(entry) {
    const row = entry.type === 'plugin' ? this.installedPluginFor(entry) : null
    if (row) { this.openDetail(row); return }
    this.entryDetail = entry
    this.showEntryDetail = true
  },

  // ── 插件详情弹窗：config / apps / dependencies / agents / hooks / channels ──

  // openDetail 声明了 setupTool 的插件默认打开「设置」页（ADR-0125），其余打开「配置」页。
  openDetail(pluginRow, tab) {
    this.detail = pluginRow
    this.detailTab = tab || (pluginRow.setup?.tool ? 'setup' : 'config')
    this.detailData = { setup: null, config: null, apps: null, deps: null, agents: null, hooks: null, channels: null }
    this.detailConfigValues = {}
    this.detailConfigClear = {}
    this.showDetail = true
    this.switchDetailTab(this.detailTab)
  },

  // closeDetail 弹窗关闭：停止状态轮询并卸载页内设置视图（iframe 不随弹窗隐藏而释放）。
  closeDetail() {
    clearInterval(this._setupPoll)
    this._setupPoll = null
    this._teardownSetupView()
    this.showDetail = false
    this.detail = null
  },

  switchDetailTab(tab) {
    this.detailTab = tab
    clearInterval(this._setupPoll)
    this._setupPoll = null
    if (tab === 'setup') {
      this._setupPoll = setInterval(() => {
        if (this.showDetail && this.detailTab === 'setup' && !document.hidden) this._loadDetailSetup()
      }, 5000)
    }
    if (!this.detailData[tab]) this.loadDetailTab(tab)
  },

  async loadDetailTab(tab) {
    if (!this.detail) return
    this.detailLoading[tab] = true
    try {
      if (tab === 'setup') await this._loadDetailSetup()
      else if (tab === 'config') await this._loadDetailConfig()
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

  // _loadDetailSetup 读取 statusTool 的就绪状态与逐项清单；无法得出结论时 ready=null。
  // 同步刷新卡片上的就绪徽标，弹窗内完成设置后不必重新加载列表。
  async _loadDetailSetup() {
    const row = this.detail
    if (!row) return
    let d = null
    try {
      const r = await fetch(`/v1/plugins/${encodeURIComponent(row.id)}/setup-status`, { headers: authHeaders() })
      d = r.ok ? await r.json() : null
    } catch { /* 轮询中的瞬时失败按未知处理 */ }
    if (!this.detail || this.detail.id !== row.id) return
    const ready = d && typeof d.ready === 'boolean' ? d.ready : null
    this.detailData.setup = { ready, message: (d && d.message) || '', checks: (d && d.checks) || [] }
    if (ready !== null) this.setupReady = { ...this.setupReady, [row.id]: { ready, message: this.detailData.setup.message } }
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
      Alpine.store('toast').show('ok', `已信任：${src.key}`)
      await this._loadDetailHooks()
    } catch (e) {
      Alpine.store('toast').show('error', `信任失败：${e.message}`)
    }
  },

  async revokeHookTrust(src) {
    try {
      const r = await fetch(`/v1/hooks/trust?key=${encodeURIComponent(src.key)}`, { method: 'DELETE', headers: authHeaders() })
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
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
      if (!r.ok) throw new Error(await readHTTPErrorMessage(r))
      const d = await r.json()
      const n = (d.pruned || []).length
      Alpine.store('toast').show('ok', n > 0 ? `已清理 ${n} 个孤儿依赖插件` : '没有可清理的插件')
      if (n > 0) await this.load()
    } catch (e) {
      Alpine.store('toast').show('error', `清理失败：${e.message}`)
    }
  },

  typeLabel(type) {
    const key = { mcp: 'plugins_tab_mcp', skill: 'plugins_tab_skill', plugin: 'plugins_tab_plugin', marketplace: 'plugins_tab_marketplace' }[type]
    return key ? Alpine.store('i18n').t(key) : (type || Alpine.store('i18n').t('plugins_tab_plugin'))
  },
  typeColor(type) {
    return { mcp: '#3b82f6', skill: '#8b5cf6', plugin: '#f59e0b', marketplace: '#ec4899' }[type] || '#3b82f6'
  },
  typeIcon(type) {
    return { mcp: '🔌', skill: '⚡', plugin: '📦', marketplace: '🛒' }[type] || '📦'
  },

  trustLabel(tier) {
    const key = ({ 0: 'plugins_trust_untrusted', 1: 'plugins_trust_local', 2: 'plugins_trust_community', 3: 'plugins_trust_verified', 4: 'plugins_trust_builtin' })[tier]
    return key ? Alpine.store('i18n').t(key) : Alpine.store('i18n').t('plugins_trust_unknown')
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
