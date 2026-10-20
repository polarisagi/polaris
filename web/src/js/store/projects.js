import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: projects（项目 = 会话的运行上下文容器，ADR-0097）
// 说明：项目 API 仅本地可信客户端/admin 可用。记忆口径见 ADR-0097 决策三修订：情景记忆按项目
// 隔离，用户画像/语义知识/反思经验跨项目共享——UI 文案须与此一致，不得笼统说"记忆隔离"。
// ══════════════════════════════════════════════════════════════════════════
const DEFAULT_ID = 'default'
const LS_KEY = 'polaris_project_id'
const RECENT_LIMIT = 2

function readCurrent() {
  try { return localStorage.getItem(LS_KEY) || DEFAULT_ID } catch { return DEFAULT_ID }
}

function emptyForm() {
  return { open: false, mode: 'create', id: '', name: '', root_path: '', instructions: '', trusted: false, saving: false, error: '' }
}

const jsonHeaders = () => ({ ...authHeaders(), 'Content-Type': 'application/json' })

async function errText(r) {
  try {
    const t = (await r.text()).trim()
    return t || `HTTP ${r.status}`
  } catch { return `HTTP ${r.status}` }
}

Alpine.store('projects', {
  list: [],
  loading: false,
  showArchived: false,
  current: readCurrent(),    // 新会话归属的项目
  selectedID: null,          // 项目页当前展开（手风琴）的项目；默认不展开任何项目
  sessions: [],              // 展开项目的最近会话（至多 RECENT_LIMIT 条）
  sessionsLoading: false,
  form: emptyForm(),

  // ── 查询 ──────────────────────────────────────────────
  byID(id) { return this.list.find(p => p.id === id) || null },
  name(id) { const p = this.byID(id); return p ? p.name : (id || '') },
  get selected() { return this.byID(this.selectedID) },
  get active() { return this.list.filter(p => !p.archived) },
  // 项目页可见项目：默认项目置顶；归档项目仅在「显示已归档」开启时出现。
  get visible() {
    return this.list
      .filter(p => this.showArchived || !p.archived)
      .sort((a, b) => (b.is_default ? 1 : 0) - (a.is_default ? 1 : 0))
  },

  async load() {
    this.loading = true
    try {
      const r = await fetch(`/v1/projects?include_archived=true`, { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      this.list = d.projects || []
      // 当前项目被删除/归档后回落默认项目，避免新会话绑定到不存在的项目而失败。
      const cur = this.byID(this.current)
      if (!cur || cur.archived) this.setCurrent(DEFAULT_ID)
      if (this.selectedID && !this.byID(this.selectedID)) this.selectedID = null
    } catch { /* 静默：项目 API 不可用时退化为无项目 UI */ } finally {
      this.loading = false
    }
  },

  setCurrent(id) {
    this.current = id || DEFAULT_ID
    try { localStorage.setItem(LS_KEY, this.current) } catch { /* 私有窗口等 */ }
  },

  // ── 项目页 ────────────────────────────────────────────
  // 展开/收起项目（同时只展开一个）；展开时只拉最近 RECENT_LIMIT 个会话。
  async toggleProject(id) {
    if (this.selectedID === id) { this.selectedID = null; this.sessions = []; return }
    await this.openProject(id)
  },

  async openProject(id) {
    this.selectedID = id
    this.sessions = []
    await this.loadSessions()
  },

  // 「查看全部」：带项目过滤跳转会话页。
  viewAllSessions(p) {
    const s = Alpine.store('sessions')
    s.projectFilter = p.id
    Alpine.store('nav').navigate('sessions')
    s.load()
  },

  async loadSessions() {
    if (!this.selectedID) { this.sessions = []; return }
    this.sessionsLoading = true
    const id = this.selectedID
    try {
      const r = await fetch(`/v1/sessions?project_id=${encodeURIComponent(id)}&limit=${RECENT_LIMIT}`, { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
      if (this.selectedID !== id) return // 加载期间用户已展开别的项目
      this.sessions = d.sessions || []
    } catch { /* 静默 */ } finally {
      this.sessionsLoading = false
    }
  },

  startChat(id) {
    this.setCurrent(id)
    Alpine.store('chat').newSession()
    Alpine.store('nav').navigate('chat')
  },

  openSession(sess) {
    this.setCurrent(sess.project_id || DEFAULT_ID)
    Alpine.store('sessions').openSession(sess.id)
  },

  // ── 表单 ──────────────────────────────────────────────
  openCreate() {
    this.form = { ...emptyForm(), open: true, mode: 'create' }
  },
  openEdit(p) {
    this.form = {
      ...emptyForm(), open: true, mode: 'edit', id: p.id,
      name: p.name, root_path: p.root_path, instructions: p.instructions, trusted: p.trusted,
    }
  },
  closeForm() { this.form.open = false },

  async save() {
    const f = this.form
    if (!f.name.trim()) { f.error = Alpine.store('i18n').t('projects_err_name'); return }
    f.saving = true
    f.error = ''
    try {
      const body = { name: f.name.trim(), root_path: f.root_path.trim(), instructions: f.instructions, trusted: f.trusted && !!f.root_path.trim() }
      const r = await fetch(f.mode === 'create' ? '/v1/projects' : `/v1/projects/${f.id}`, {
        method: f.mode === 'create' ? 'POST' : 'PUT', headers: jsonHeaders(), body: JSON.stringify(body),
      })
      if (!r.ok) { f.error = await errText(r); return }
      const saved = await r.json()
      await this.load()
      if (f.mode === 'create') { this.setCurrent(saved.id); await this.openProject(saved.id) }
      this.closeForm()
    } catch (err) {
      f.error = err.message
    } finally {
      f.saving = false
    }
  },

  async setArchived(p, archived) {
    const r = await fetch(`/v1/projects/${p.id}`, { method: 'PUT', headers: jsonHeaders(), body: JSON.stringify({ archived }) })
    if (!r.ok) { Alpine.store('toast').show('error', await errText(r)); return }
    await this.load()
  },

  async remove(p) {
    const msg = Alpine.store('i18n').t('projects_delete_confirm').replace('{0}', p.name)
    if (!(await Alpine.store('dialog').confirm({ message: msg, danger: true }))) return
    try {
      const r = await fetch(`/v1/projects/${p.id}`, { method: 'DELETE', headers: authHeaders() })
      if (!r.ok) { Alpine.store('toast').show('error', await errText(r)); return }
    } catch (err) {
      Alpine.store('toast').show('error', `${Alpine.store('i18n').t('delete_failed')}: ${err.message}`)
      return
    }
    if (this.selectedID === p.id) { this.selectedID = null; this.sessions = [] }
    Alpine.store('toast').show('ok', Alpine.store('i18n').t('deleted'))
    await this.load()
  },

  async moveSession(sessionID, projectID) {
    const r = await fetch(`/v1/sessions/${sessionID}/project`, {
      method: 'PUT', headers: jsonHeaders(), body: JSON.stringify({ project_id: projectID }),
    })
    if (!r.ok) { Alpine.store('toast').show('error', await errText(r)); return }
    await Promise.all([this.load(), this.loadSessions(), Alpine.store('sessions').load()])
  },
})
