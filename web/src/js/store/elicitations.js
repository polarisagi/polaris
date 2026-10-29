import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: elicitations（MCP elicitation 待办，ADR-0103 决策八 / M13-bis §8.5）
// 与 approvals.js 同一轮询范式：MCP 服务器发起的 elicitation/create 请求经
// gateway/elicitation.Broker 登记为 pending，前端没有专属推送通道（不复用 SSE
// tool_ui/approval_required 那类回合内事件——elicitation 可能发生在任意一次
// MCP 工具调用期间，不限于当前回合），只能轮询 GET /v1/elicitations。
// ══════════════════════════════════════════════════════════════════════════

// buildFields 把 requestedSchema（spec client/elicitation.md §Requested Schema：
// 平铺 object + primitive 属性）解析为渲染用字段列表。解析失败的字段整体跳过
// 而不是让整个表单报错——服务器给出的 schema 不可信，单个字段形状异常不应
// 阻塞其余合法字段的展示。
function buildFields(schema) {
  if (!schema || schema.type !== 'object' || !schema.properties) return []
  const required = new Set(schema.required || [])
  const fields = []
  for (const [name, prop] of Object.entries(schema.properties)) {
    if (!prop || typeof prop !== 'object') continue
    const base = { name, required: required.has(name), title: prop.title || name, description: prop.description || '' }
    if (prop.type === 'array') {
      fields.push({ ...base, kind: 'multiselect', options: enumOptions(prop.items), minItems: prop.minItems, maxItems: prop.maxItems, default: Array.isArray(prop.default) ? prop.default : [] })
    } else if (prop.type === 'string' && (prop.enum?.length || prop.oneOf?.length)) {
      fields.push({ ...base, kind: 'select', options: enumOptions(prop), default: prop.default ?? '' })
    } else if (prop.type === 'string') {
      fields.push({ ...base, kind: 'string', format: prop.format || '', minLength: prop.minLength, maxLength: prop.maxLength, default: prop.default ?? '' })
    } else if (prop.type === 'number' || prop.type === 'integer') {
      fields.push({ ...base, kind: 'number', integer: prop.type === 'integer', minimum: prop.minimum, maximum: prop.maximum, default: prop.default ?? '' })
    } else if (prop.type === 'boolean') {
      fields.push({ ...base, kind: 'boolean', default: !!prop.default })
    }
    // 未识别类型（服务器违反 schema 限制）：静默跳过该字段，不渲染也不提交。
  }
  return fields
}

// enumOptions 单选（enum/oneOf）与多选 items（enum/anyOf/oneOf）统一取值-标题对；
// 无标题变体用取值本身当标题。
function enumOptions(node) {
  if (!node) return []
  if (Array.isArray(node.oneOf) || Array.isArray(node.anyOf)) {
    return (node.oneOf || node.anyOf).map(o => ({ value: o.const, title: o.title ?? String(o.const) }))
  }
  if (Array.isArray(node.enum)) return node.enum.map(v => ({ value: v, title: String(v) }))
  return []
}

Alpine.store('elicitations', {
  list: [],
  pollFailures: 0,
  _timer: null,

  startPolling() {
    this.pollFailures = 0
    this.poll()
    this._timer = setInterval(() => this.poll(), 3000)
  },

  stopPolling() { clearInterval(this._timer); this._timer = null; this.list = [] },

  async poll() {
    if (this.pollFailures >= 3) return
    try {
      const sid = Alpine.store('chat')?.sessionID || ''
      const r = await fetch(`/v1/elicitations?session_id=${encodeURIComponent(sid)}`, { headers: authHeaders() })
      if (r.status === 501) { this.stopPolling(); return } // broker 未启用：不再轮询
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const d = await r.json()
      const known = new Map(this.list.map(e => [e.id, e]))
      this.list = (d.elicitations || []).map(e => this._toView(e, known.get(e.id)))
      this.pollFailures = 0
    } catch {
      this.pollFailures++
    }
  },

  // _toView 服务器快照 → 渲染态；已存在的条目保留用户尚未提交的表单输入
  // （prev），避免每次轮询刷新把用户正在填写的值冲掉。
  _toView(e, prev) {
    if (prev) return prev
    let schema = null
    try { schema = e.requested_schema ? JSON.parse(e.requested_schema) : null } catch { /* 非法 schema：不渲染表单字段，仍可 decline/cancel */ }
    const fields = buildFields(schema)
    const values = {}
    for (const f of fields) values[f.name] = f.default
    return { ...e, fields, values, busy: false, error: '' }
  },

  urlHost(u) {
    try { return new URL(u).host } catch { return u }
  },

  toggleMulti(item, name, value) {
    const cur = item.values[name] || []
    const i = cur.indexOf(value)
    item.values[name] = i >= 0 ? cur.filter(v => v !== value) : [...cur, value]
  },

  // validate 提交前的客户端复核（真正的权威校验在网关 elicitation.validateFormContent）：
  // 只检查必填是否存在，减少明显会被拒绝的往返；不做类型/格式细校验。
  validate(item) {
    for (const f of item.fields) {
      if (!f.required) continue
      const v = item.values[f.name]
      if (v === '' || v === undefined || v === null || (Array.isArray(v) && v.length === 0)) {
        return Alpine.store('i18n').t('elicit_err_required').replace('{0}', f.title)
      }
    }
    return ''
  },

  async respond(item, action) {
    if (item.busy) return
    let content
    if (action === 'accept') {
      if (item.mode === 'url') {
        content = undefined // 规范：url 模式 accept 时不回传内容
      } else {
        const err = this.validate(item)
        if (err) { item.error = err; return }
        content = this._coerce(item)
      }
    }
    item.busy = true
    item.error = ''
    try {
      const r = await fetch(`/v1/elicitations/${encodeURIComponent(item.id)}`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ action, content }),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      this.list = this.list.filter(e => e.id !== item.id)
    } catch (err) {
      item.busy = false
      item.error = err.message
    }
  },

  // _coerce 表单值按字段类型转换（number input 原生返回字符串；multiselect 已是数组）。
  _coerce(item) {
    const out = {}
    for (const f of item.fields) {
      const v = item.values[f.name]
      if (v === '' || v === undefined) continue // 未填的非必填字段不提交（避免与 required 校验之外误传空串）
      if (f.kind === 'number') out[f.name] = f.integer ? parseInt(v, 10) : Number(v)
      else out[f.name] = v
    }
    return out
  },
})
