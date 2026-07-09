import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'

// ══════════════════════════════════════════════════════════════════════════
// store: cron（自动化任务管理）
// ══════════════════════════════════════════════════════════════════════════

// cron 预设选项（存储合法 cron 表达式，显示人类可读文本）
const SCHEDULE_PRESETS = [
  { label: '每小时', value: '0 * * * *' },
  { label: '每天 09:00', value: '0 9 * * *' },
  { label: '工作日 09:00', value: '0 9 * * 1-5' },
  { label: '每周一 09:00', value: '0 9 * * 1' },
  { label: '每天 18:00', value: '0 18 * * *' },
  { label: '每月 1 日', value: '0 9 1 * *' },
]

// 推理档位（不暴露模型名称，系统自动映射）
const REASONING_LEVELS = [
  { value: 'low', label: '低', desc: '快速响应，适合简单汇报' },
  { value: 'medium', label: '中', desc: '均衡，大多数任务默认' },
  { value: 'high', label: '高', desc: '深度分析，适合代码审查' },
  { value: 'ultra', label: '超高', desc: '最强推理，适合复杂规划' },
]

// 结果分发
const RESULT_ACTIONS = [
  { value: 'session', label: '保存到会话', desc: '生成对话记录，可在"会话"页查看' },
  { value: 'silent', label: '静默执行', desc: '只记录日志，不推送' },
]

// 人类可读的 cron 表达式
function cronLabel(expr) {
  const preset = SCHEDULE_PRESETS.find(p => p.value === expr)
  if (preset) return preset.label
  if (!expr) return '未设定'
  return expr
}

// 状态色
function statusColor(s) {
  if (s === 'ok') return 'var(--color-success, #22c55e)'
  if (s === 'error') return 'var(--color-error, #ef4444)'
  if (s === 'running') return 'var(--color-accent)'
  return 'var(--color-text-dim)'
}

function statusIcon(s) {
  if (s === 'ok') return '✓'
  if (s === 'error') return '✗'
  if (s === 'running') return '⟳'
  return '○'
}

Alpine.store('cron', {
  list: [],
  loading: false,

  // 创建/编辑弹窗
  showModal: false,
  editMode: 'create',
  showTemplates: false,

  // 执行历史侧栏
  showRuns: false,
  runsJobID: '',
  runsJobName: '',
  runs: [],
  runsLoading: false,

  // 模板（从 /v1/automation-templates 加载，初始空数组）
  templates: [],
  templatesLoaded: false,

  form: {
    id: '',
    name: '',
    prompt: '',
    trigger_type: 'cron',
    cron_schedule: '0 9 * * 1-5',
    channel_id: '',
    working_dir: '',
    reasoning_effort: 'medium',
    result_action: 'session',
    cedar_rules_json: '[]',
    enabled: true,
  },

  schedulePresets: SCHEDULE_PRESETS,
  reasoningLevels: REASONING_LEVELS,
  resultActions: RESULT_ACTIONS,

  cronLabel,
  statusColor,
  statusIcon,

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/automations', { headers: authHeaders() })
      const d = await r.json()
      this.list = d.automations || []
    } catch { } finally { this.loading = false }
  },

  async loadTemplates() {
    if (this.templatesLoaded) return
    try {
      const r = await fetch('/v1/automation-templates', { headers: authHeaders() })
      const d = await r.json()
      this.templates = d.templates || []
    } catch { } finally { this.templatesLoaded = true }
  },

  openCreate() {
    this.form = {
      id: '', name: '', prompt: '',
      trigger_type: 'cron', cron_schedule: '0 9 * * 1-5', channel_id: '',
      working_dir: '',
      reasoning_effort: 'medium', result_action: 'session',
      cedar_rules_json: '[]', enabled: true,
    }
    this.editMode = 'create'
    this.showTemplates = false
    this.showModal = true
    this.loadTemplates()
  },

  openEdit(job) {
    this.form = { ...job }
    this.editMode = 'edit'
    this.showTemplates = false
    this.showModal = true
    this.loadTemplates()
  },

  applyTemplate(t) {
    this.form.name = t.name
    this.form.prompt = t.prompt
    this.form.trigger_type = t.trigger_type || 'cron'
    this.form.cron_schedule = t.cron_schedule || '0 9 * * 1-5'
    this.form.reasoning_effort = t.reasoning_effort || 'medium'
    this.showTemplates = false
  },

  async save() {
    const body = { ...this.form }
    try {
      let r
      if (this.editMode === 'create') {
        r = await fetch('/v1/automations', {
          method: 'POST', headers: authHeaders(), body: JSON.stringify(body),
        })
      } else {
        r = await fetch(`/v1/automations/${body.id}`, {
          method: 'PUT', headers: authHeaders(), body: JSON.stringify(body),
        })
      }
      if (!r.ok) {
        const msg = await r.text()
        throw new Error(`HTTP ${r.status}: ${msg}`)
      }
      this.showModal = false
      await this.load()
      Alpine.store('toast').show('ok', '保存成功')
    } catch (e) {
      Alpine.store('toast').show('error', `保存失败: ${e.message}`)
    }
  },

  async toggle(job) {
    try {
      await fetch(`/v1/automations/${job.id}`, {
        method: 'PUT',
        headers: authHeaders(),
        body: JSON.stringify({ enabled: !job.enabled }),
      })
      await this.load()
    } catch { }
  },

  async del(id) {
    if (!confirm('确认删除这个自动化任务？运行历史也会一并删除。')) return
    try {
      await fetch(`/v1/automations/${id}`, { method: 'DELETE', headers: authHeaders() })
      this.list = this.list.filter(j => j.id !== id)
      Alpine.store('toast').show('ok', '已删除')
    } catch (e) {
      Alpine.store('toast').show('error', `删除失败: ${e.message}`)
    }
  },

  async trigger(job) {
    try {
      const r = await fetch(`/v1/automations/${job.id}/trigger`, {
        method: 'POST', headers: authHeaders(),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      Alpine.store('toast').show('ok', `已触发：${job.name || job.id.slice(0, 12)}`)
      setTimeout(() => this.load(), 1500)
    } catch (e) {
      Alpine.store('toast').show('error', `触发失败: ${e.message}`)
    }
  },

  async openRuns(job) {
    this.runsJobID = job.id
    this.runsJobName = job.name || job.id.slice(0, 12)
    this.runs = []
    this.runsLoading = true
    this.showRuns = true
    try {
      const r = await fetch(`/v1/automations/${job.id}/runs?limit=20`, { headers: authHeaders() })
      const d = await r.json()
      this.runs = d.runs || []
    } catch { } finally { this.runsLoading = false }
  },
})
