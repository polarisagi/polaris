import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: plugins（插件目录 Catalog）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('plugins', {
  catalog: [],
  loading: false,
  syncing: false,
  filter: 'plugin',   // 'plugin' | 'app' | 'mcp' | 'skill' | 'marketplace'
  search: '',
  installing: {},  // catalogID → true
  uninstalling: {},
  showEnvModal: false,
  envPending: null, // { entry, envVars: {} }
  marketplaces: [],

  // Creation Modal State
  showCreateModal: false,
  createForm: {
    name: '',
    description: '',
    repo_url: '',
    entrypoint: '',
    manifest_url: '',
    url: '',
    transport: 'stdio',
    command: '',
    args: '', // JSON array string
    env: '',  // JSON object string
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
      const [catRes, mpRes] = await Promise.all([
        fetch('/v1/plugins/catalog', { headers: authHeaders() }),
        fetch('/v1/plugins/marketplaces', { headers: authHeaders() })
      ])
      if (catRes.ok) {
        const d = await catRes.json()
        this.catalog = d.catalog || []
      }
      if (mpRes.ok) {
        const d = await mpRes.json()
        this.marketplaces = d.marketplaces || []
      }
    } catch { /* 静默 */ } finally {
      this.loading = false
    }
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
    this.createForm = {
      name: '', description: '', repo_url: '', entrypoint: '',
      manifest_url: '', url: '', transport: 'stdio', command: '', args: '', env: ''
    }
  },

  openCreateModal() {
    this.resetCreateForm()
    this.showCreateModal = true
  },

  async submitCreation() {
    let endpoint = ''
    let body = {}
    const filter = this.filter

    try {
      if (filter === 'skill') {
        endpoint = '/v1/skills/create'
        body = {
          name: this.createForm.name,
          description: this.createForm.description,
          repo_url: this.createForm.repo_url,
          entrypoint: this.createForm.entrypoint
        }
      } else if (filter === 'plugin') {
        endpoint = '/v1/plugins/create'
        body = {
          name: this.createForm.name,
          description: this.createForm.description,
          manifest_url: this.createForm.manifest_url
        }
      } else if (filter === 'app') {
        endpoint = '/v1/apps/create'
        body = {
          name: this.createForm.name,
          description: this.createForm.description,
          url: this.createForm.url
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
          type: 'plugin', // Default to plugin for custom marketplaces
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

  typeLabel(type) {
    return { mcp: 'MCP 服务', skill: '技能', plugin: '插件', app: '应用', marketplace: '市场' }[type] || type || '插件'
  },
  typeColor(type) {
    return { mcp: '#3b82f6', skill: '#8b5cf6', plugin: '#f59e0b', app: '#10b981', marketplace: '#ec4899' }[type] || '#3b82f6'
  },
  typeIcon(type) {
    return { mcp: '⚙', skill: '⚡', plugin: '📦', app: '📱', marketplace: '🛒' }[type] || '📦'
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
