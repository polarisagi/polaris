import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'

// ══════════════════════════════════════════════════════════════════════════
// 会话步骤时间线（ADR-0118）
//
// 数据来自服务端结构化 `step` SSE 事件（实时），或 GET /v1/sessions/{id}/turns/{turn_id}/steps（历史懒加载）。
// 一个回合 = { id, steps: [], summary, loaded, loading, running }：
//   - steps 按 seq 排序，parent_id 指向子智能体步骤（缩进显示）；
//   - 实时回合在 SSE 重放（重连/刷新后重新订阅）时会重复收到同一 id 的事件，所以 applyStep 对 id 幂等。
// 步骤从不进入模型上下文，纯展示。
// ══════════════════════════════════════════════════════════════════════════

export function newTurn() {
  return { id: '', steps: [], summary: null, loaded: true, loading: false, running: true, error: '' }
}

/** 把一条 step 事件载荷并入回合。phase: start | update | end；对相同 id 幂等。 */
export function applyStep(turn, p) {
  if (!turn || !p || !p.id) return
  let st = turn.steps.find(s => s.id === p.id)
  if (!st) {
    st = { ...p, content: p.content || '', _open: false }
    turn.steps.push(st)
    turn.steps.sort((a, b) => a.seq - b.seq)
    // 重新取回响应式代理后的引用，保证后续原地修改触发更新
    st = turn.steps.find(s => s.id === p.id)
  }
  if (p.phase === 'update') {
    // 思考增量：只有 start→update 链路才拼接；重放时 end 事件的整段 content 会覆盖，不会重复。
    if (p.delta) st.content = (st.content || '') + p.delta
    return
  }
  const keep = st.content
  Object.assign(st, p)
  st.content = p.content || keep || ''
  delete st.delta
}

/** 展开为带缩进深度的行（先序遍历：父在前，子紧随）。 */
export function flattenSteps(steps) {
  const byParent = new Map()
  const ids = new Set(steps.map(s => s.id))
  for (const s of steps) {
    // 父步骤缺失（被截断）时当作根
    const key = s.parent_id && ids.has(s.parent_id) ? s.parent_id : ''
    if (!byParent.has(key)) byParent.set(key, [])
    byParent.get(key).push(s)
  }
  const out = []
  const walk = (key, depth) => {
    for (const s of byParent.get(key) || []) {
      out.push({ step: s, depth })
      walk(s.id, depth + 1)
    }
  }
  walk('', 0)
  return out
}

const KIND_ICON = { thinking: '💭', tool: '⚡', subagent: '🤖', approval: '🔒', retry: '↻', notice: 'ℹ', llm: '✦' }

Alpine.data('stepTimeline', (getTurn, getSessionID) => ({
  open: false,

  get turn() { return getTurn() },
  get rows() { return flattenSteps(this.turn?.steps || []) },
  get has() {
    const t = this.turn
    return !!t && (t.steps.length > 0 || (t.summary && t.summary.steps > 0))
  },

  icon(step) { return KIND_ICON[step.kind] || '•' },
  kindLabel(step) { return Alpine.store('i18n').t('step_kind_' + step.kind) },

  statusCls(step) {
    switch (step.status) {
      case 'running': return 'text-info'
      case 'error': return 'text-error'
      case 'denied': return 'text-warning'
      case 'cancelled': return 'text-base-content/40'
      default: return 'text-success'
    }
  },
  statusMark(step) {
    return { running: '', ok: '✓', error: '✕', denied: '⊘', cancelled: '–' }[step.status] ?? ''
  },

  /** 折叠摘要行："思考 · 3 次工具 · 2 次搜索 · 12.3s"。 */
  get summaryLine() {
    const t = this.turn
    if (!t) return ''
    const i18n = Alpine.store('i18n')
    const fmt = Alpine.store('fmt')
    const s = t.summary
    const live = !s
    const tools = s ? s.tools : t.steps.filter(x => x.kind === 'tool').length
    const searches = s ? s.searches : t.steps.filter(x => x.kind === 'tool' && x.tool === 'web_search').length
    const fetches = s ? s.fetches : t.steps.filter(x => x.kind === 'tool' && x.tool === 'web_fetch').length
    const parts = []
    if (tools > 0) parts.push(i18n.t('step_sum_tools').replace('{0}', tools))
    if (searches > 0) parts.push(i18n.t('step_sum_searches').replace('{0}', searches))
    if (fetches > 0) parts.push(i18n.t('step_sum_fetches').replace('{0}', fetches))
    if (parts.length === 0) parts.push(i18n.t(live ? 'step_sum_working' : 'step_sum_thought'))
    if (s && s.duration_ms > 0) parts.push(fmt.secs(s.duration_ms))
    return parts.join(' · ')
  },

  /** 实时：当前最近一个仍在运行的步骤，作为折叠态下的"正在做什么"。 */
  get current() {
    const t = this.turn
    if (!t || !t.running) return null
    for (let i = t.steps.length - 1; i >= 0; i--) {
      if (t.steps[i].status === 'running') return t.steps[i]
    }
    return null
  },

  async toggle() {
    this.open = !this.open
    if (this.open) await this.ensureLoaded()
  },

  /** 历史回合：展开时才拉步骤。 */
  async ensureLoaded() {
    const t = this.turn
    if (!t || t.loaded || t.loading || !t.id) return
    const sid = getSessionID()
    if (!sid) return
    t.loading = true
    t.error = ''
    try {
      const r = await fetch(`/v1/sessions/${encodeURIComponent(sid)}/turns/${encodeURIComponent(t.id)}/steps`, { headers: authHeaders() })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      // 落库行就是 Step 的 JSON；summary 行（seq=0）不是步骤
      t.steps = (d.steps || []).filter(s => s && s.id).map(s => ({ ...s, content: s.content || '', _open: false }))
      t.loaded = true
    } catch (e) {
      t.error = e.message || 'error'
    } finally {
      t.loading = false
    }
  },
}))
