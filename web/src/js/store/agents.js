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
      const [statusR, toolsR, skillsR, mcpR, channelsR] = await Promise.all([
        fetch('/v1/status', { headers: authHeaders() }),
        fetch('/v1/tools', { headers: authHeaders() }),
        fetch('/v1/skills', { headers: authHeaders() }),
        fetch('/v1/mcp-servers', { headers: authHeaders() }),
        fetch('/v1/channels', { headers: authHeaders() }),
      ])
      if (statusR.ok) {
        const d = await statusR.json()
        this.agentID = d.agent_id || ''
        this.agentState = d.agent_state || ''
        this.agentConfig = d.agent_config || {}
        this.tokenUsed = d.token_used || 0
        this.tokenLimit = d.token_limit || 0
        this.memoryMB = d.memory_mb || 0
      }
      if (toolsR.ok) { const d = await toolsR.json(); this.tools = d.tools || [] }
      if (skillsR.ok) { const d = await skillsR.json(); this.skills = d.skills || [] }
      if (mcpR.ok) { const d = await mcpR.json(); this.mcpServers = d.mcp_servers || [] }
      if (channelsR.ok) { const d = await channelsR.json(); this.channels = d.channels || [] }
    } catch { } finally { this.loading = false }
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

