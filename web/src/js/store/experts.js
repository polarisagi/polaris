import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
import { readHTTPErrorMessage } from '../http_error.mjs'

const isMatch = (expert, query, text) => {
  if (!query) return true
  const fields = [
    text(expert.displayName), text(expert.description),
    expert.plugin_name, expert.id, expert.entryAgent,
    ...(expert.members || []),
  ]
  return fields.some(value => String(value || '').toLowerCase().includes(query))
}

Alpine.store('experts', {
  list: [],
  diagnostics: [],
  loading: false,
  loaded: false,
  error: '',
  query: '',
  filter: 'all',
  starting: false,

  get filtered() {
    const query = this.query.trim().toLowerCase()
    return this.list.filter(expert => {
      if (this.filter === 'general') return false
      if (this.filter === 'experts' && expert.type !== 'agent') return false
      if (this.filter === 'teams' && expert.type !== 'team') return false
      return isMatch(expert, query, value => this.text(value))
    })
  },

  async load(force = false) {
    if (this.loading || (this.loaded && !force)) return
    this.loading = true
    this.error = ''
    try {
      const response = await fetch('/v1/experts', { headers: authHeaders() })
      if (!response.ok) throw new Error(await readHTTPErrorMessage(response))
      const data = await response.json()
      this.list = Array.isArray(data.experts) ? data.experts : []
      this.diagnostics = Array.isArray(data.diagnostics) ? data.diagnostics : []
      this.loaded = true
    } catch (error) {
      this.error = error.message || 'Unable to load experts'
    } finally {
      this.loading = false
    }
  },

  text(value) {
    if (typeof value === 'string') return value
    if (!value || typeof value !== 'object') return ''
    const lang = Alpine.store('i18n').lang
    return value[lang] || value.zh || value.en || ''
  },

  find(pluginID, expertID) {
    return this.list.find(item => item.plugin_id === pluginID && item.id === expertID) || null
  },

  greeting(binding) {
    if (!binding) return ''
    return this.text(this.find(binding.pluginID, binding.expertID)?.greeting)
  },

  async start(expert, prompt = '') {
    if (this.starting) return
    this.starting = true
    let originIsCurrent = () => false
    try {
      const chat = Alpine.store('chat')
      const nav = Alpine.store('nav')
      if (chat.sessionID || chat.messages.length || chat.isActive || nav.page !== 'chat') return
      const origin = {
        sequence: chat._loadSeq,
        sessionID: chat.sessionID,
        messages: chat.messages,
        viewKey: chat.viewKey,
        navigationRevision: nav.revision,
      }
      originIsCurrent = () => chat._loadSeq === origin.sequence
        && chat.sessionID === origin.sessionID
        && chat.messages === origin.messages
        && chat.messages.length === 0
        && chat.viewKey === origin.viewKey
        && !chat.isActive
        && nav.page === 'chat'
        && nav.revision === origin.navigationRevision
      const agentProfile = expert.agent?.name || ''
      if (!agentProfile) throw new Error('Expert entry agent is unavailable')
      const displayName = this.text(expert.displayName)
      const projectID = Alpine.store('projects').current || 'default'
      const response = await fetch('/v1/sessions', {
        method: 'POST',
        headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: JSON.stringify({
          title: displayName,
          project_id: projectID,
          agent_profile: agentProfile,
        }),
      })
      if (!response.ok) throw new Error(await readHTTPErrorMessage(response))
      const data = await response.json()
      if (!data.session_id) throw new Error('Session creation returned no session ID')
      if (!originIsCurrent()) return // 请求期间视图已切换，留存新会话但不覆盖当前视图
      chat.bindExpertSession(data, expert)
      nav.navigate('chat')
      if (prompt && chat.sessionID === data.session_id && chat.expertBinding?.expertID === expert.id) {
        await chat.submit(prompt)
      }
    } catch (error) {
      if (originIsCurrent()) Alpine.store('toast').show('error', error.message || 'Failed to start expert session')
    } finally {
      this.starting = false
    }
  },

  focusGeneralChat() {
    document.getElementById('input')?.focus()
  },

  openPluginManager() {
    Alpine.store('nav').navigate('plugins')
  },
})
