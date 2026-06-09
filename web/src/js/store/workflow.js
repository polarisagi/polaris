import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'

// ══════════════════════════════════════════════════════════════════════════
// store: workflow（工作流管理）
// ══════════════════════════════════════════════════════════════════════════

const REASONING_LEVELS = [
  { value: 'low',    label: '低',   desc: '快速响应' },
  { value: 'medium', label: '中',   desc: '均衡，大多数任务默认' },
  { value: 'high',   label: '高',   desc: '深度分析' },
  { value: 'ultra',  label: '超高', desc: '最强推理' },
]

const SCHEDULE_PRESETS = [
  { label: '每小时',         value: '0 * * * *'   },
  { label: '每天 09:00',    value: '0 9 * * *'   },
  { label: '工作日 09:00',  value: '0 9 * * 1-5' },
  { label: '每周一 09:00',  value: '0 9 * * 1'   },
  { label: '每天 18:00',    value: '0 18 * * *'  },
]

function emptyStep() {
  return {
    id: '',
    name: '',
    automation_id: '',
    prompt: '',
    reasoning_effort: 'medium',
    working_dir: '',
    input_from_prev: true,
  }
}

function emptyForm() {
  return {
    id: '',
    name: '',
    description: '',
    trigger_type: 'manual',
    cron_schedule: '0 9 * * 1-5',
    enabled: true,
    steps: [emptyStep()],
  }
}

Alpine.store('workflow', {
  list: [],
  loading: false,

  showModal: false,
  editMode: 'create',

  showRuns: false,
  runsWfID: '',
  runsWfName: '',
  runs: [],
  runsLoading: false,

  form: emptyForm(),

  schedulePresets: SCHEDULE_PRESETS,
  reasoningLevels: REASONING_LEVELS,

  cronLabel(expr) {
    const p = SCHEDULE_PRESETS.find(p => p.value === expr)
    return p ? p.label : (expr || '手动触发')
  },

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/workflows', { headers: authHeaders() })
      const d = await r.json()
      this.list = d.workflows || []
    } catch { } finally { this.loading = false }
  },

  openCreate() {
    this.form = emptyForm()
    this.editMode = 'create'
    this.showModal = true
  },

  async openEdit(wf) {
    this.editMode = 'edit'
    this.showModal = true
    try {
      const r = await fetch(`/v1/workflows/${wf.id}`, { headers: authHeaders() })
      const d = await r.json()
      this.form = {
        id:           d.workflow.id,
        name:         d.workflow.name,
        description:  d.workflow.description,
        trigger_type: d.workflow.trigger_type,
        cron_schedule:d.workflow.cron_schedule,
        enabled:      d.workflow.enabled,
        steps: (d.steps || []).map(st => ({
          id:               st.id,
          name:             st.name,
          automation_id:    st.automation_id,
          prompt:           st.prompt,
          reasoning_effort: st.reasoning_effort || 'medium',
          working_dir:      st.working_dir,
          input_from_prev:  st.input_from_prev,
        })),
      }
      if (this.form.steps.length === 0) this.form.steps.push(emptyStep())
    } catch (e) {
      Alpine.store('toast').show('error', '加载工作流失败: ' + e.message)
    }
  },

  addStep() {
    this.form.steps.push(emptyStep())
  },

  removeStep(idx) {
    if (this.form.steps.length <= 1) return
    this.form.steps.splice(idx, 1)
  },

  moveStep(idx, dir) {
    const steps = this.form.steps
    const target = idx + dir
    if (target < 0 || target >= steps.length) return
    ;[steps[idx], steps[target]] = [steps[target], steps[idx]]
  },

  async save() {
    if (!this.form.name.trim()) {
      Alpine.store('toast').show('error', '请填写工作流名称')
      return
    }
    const hasContent = this.form.steps.every(st => st.prompt.trim() || st.automation_id.trim())
    if (!hasContent) {
      Alpine.store('toast').show('error', '每个步骤需填写执行内容或绑定自动化任务')
      return
    }
    const body = {
      name:          this.form.name,
      description:   this.form.description,
      trigger_type:  this.form.trigger_type,
      cron_schedule: this.form.cron_schedule,
      enabled:       this.form.enabled,
      steps:         this.form.steps.map((st, i) => ({ ...st, seq: i })),
    }
    try {
      let r
      if (this.editMode === 'create') {
        r = await fetch('/v1/workflows', {
          method: 'POST', headers: authHeaders(), body: JSON.stringify(body),
        })
      } else {
        r = await fetch(`/v1/workflows/${this.form.id}`, {
          method: 'PUT', headers: authHeaders(), body: JSON.stringify(body),
        })
      }
      if (!r.ok) throw new Error(`HTTP ${r.status}: ${await r.text()}`)
      this.showModal = false
      await this.load()
      Alpine.store('toast').show('ok', '保存成功')
    } catch (e) {
      Alpine.store('toast').show('error', '保存失败: ' + e.message)
    }
  },

  async toggle(wf) {
    try {
      await fetch(`/v1/workflows/${wf.id}`, {
        method: 'PUT',
        headers: authHeaders(),
        body: JSON.stringify({ enabled: !wf.enabled }),
      })
      await this.load()
    } catch { }
  },

  async del(id) {
    if (!confirm('确认删除这个工作流？执行历史也会一并删除。')) return
    try {
      await fetch(`/v1/workflows/${id}`, { method: 'DELETE', headers: authHeaders() })
      this.list = this.list.filter(w => w.id !== id)
      Alpine.store('toast').show('ok', '已删除')
    } catch (e) {
      Alpine.store('toast').show('error', '删除失败: ' + e.message)
    }
  },

  async trigger(wf) {
    try {
      const r = await fetch(`/v1/workflows/${wf.id}/trigger`, {
        method: 'POST', headers: authHeaders(),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      Alpine.store('toast').show('ok', `已触发：${wf.name}`)
      setTimeout(() => this.load(), 2000)
    } catch (e) {
      Alpine.store('toast').show('error', '触发失败: ' + e.message)
    }
  },

  async openRuns(wf) {
    this.runsWfID = wf.id
    this.runsWfName = wf.name
    this.runs = []
    this.runsLoading = true
    this.showRuns = true
    try {
      const r = await fetch(`/v1/workflows/${wf.id}/runs?limit=20`, { headers: authHeaders() })
      const d = await r.json()
      this.runs = (d.runs || []).map(run => ({
        ...run,
        stepOutputs: (() => {
          try { return JSON.parse(run.step_outputs || '[]') } catch { return [] }
        })(),
      }))
    } catch { } finally { this.runsLoading = false }
  },

  stepStatusIcon(status) {
    if (status === 'ok') return '✓'
    if (status === 'error') return '✗'
    return '○'
  },

  stepStatusClass(status) {
    if (status === 'ok') return 'text-success'
    if (status === 'error') return 'text-error'
    return 'text-base-content/50'
  },
})
