import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: agents（Agent 状态总览）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('agents', {
  // Agent 状态（来自 /v1/status）
  agentID: '',
  agentState: '',
  agentConfig: {},

  // 统计数据
  tools: [],
  skills: [],
  mcpServers: [],
  channels: [],

  loading: true,

  async load() {
    this.loading = true
    try {
      const hdrs = authHeaders()

      const pStatus = fetch('/v1/status', { headers: hdrs })
        .then(r => r.ok ? r.json() : null)
        .then(d => {
          if (d) {
            this.agentID = d.agent_id || ''
            this.agentState = d.agent_state || ''
            this.agentConfig = d.agent_config || {}
            this.tokenUsed = d.token_used || 0
            this.tokenLimit = d.token_limit || 0
            this.memoryMB = d.memory_mb || 0
          }
        }).catch(() => {})

      const pTools = fetch('/v1/tools', { headers: hdrs })
        .then(r => r.ok ? r.json() : null)
        .then(d => { if (d) this.tools = d.tools || [] })
        .catch(() => {})

      const pSkills = fetch('/v1/skills', { headers: hdrs })
        .then(r => r.ok ? r.json() : null)
        .then(d => { if (d) this.skills = d.skills || [] })
        .catch(() => {})

      const pMcp = fetch('/v1/mcp-servers', { headers: hdrs })
        .then(r => r.ok ? r.json() : null)
        .then(d => { if (d) this.mcpServers = d.mcp_servers || [] })
        .catch(() => {})

      const pChannels = fetch('/v1/channels', { headers: hdrs })
        .then(r => r.ok ? r.json() : null)
        .then(d => { if (d) this.channels = d.channels || [] })
        .catch(() => {})

      await Promise.allSettled([pStatus, pTools, pSkills, pMcp, pChannels])
    } finally { 
      this.loading = false 
    }
  },

  stateLabel(state) {
    const labels = {
      idle: '空闲', perceive: '感知', plan: '规划',
      validate: '校验', execute: '执行', reflect: '反思',
      replan: '重规划', rollback: '回滚',
      complete: '完成', failed: '失败', interrupt: '中断',
    }
    return labels[state] || state
  },

  // 技能显示名：去掉 "skill:" 前缀，保留可读部分
  skillDisplayName(name) {
    return (name || '').replace(/^skill:/, '')
  },

  // exec_mode 标签颜色
  skillModeClass(mode) {
    return mode === 'ambient' ? 'badge-info' : 'badge-success'
  },

  // exec_mode 中文标签
  skillModeLabel(mode) {
    return mode === 'ambient' ? '环境注入' : '工具调用'
  },

  // 仅统计启用（non-deprecated）的 skill
  get enabledSkillCount() {
    return this.skills.filter(sk => sk.enabled).length
  },
})

