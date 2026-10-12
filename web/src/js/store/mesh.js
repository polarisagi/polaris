import Alpine from 'alpinejs'
import { authHeaders } from '../utils.js'
// ══════════════════════════════════════════════════════════════════════════
// store: mesh（设置页「多节点」：本机身份、监听与策略设置、对端配对、令牌，ADR-0111）
// 后端：GET/PUT /v1/mesh/settings、/v1/mesh/{self,invites,peers,tokens}；配对的 MCP 登记
// 与 CLI（cmd/polaris/cli_peer.go）同一顺序：先 /v1/mesh/peers，再 /v1/mcp-servers。
// ══════════════════════════════════════════════════════════════════════════
const jsonHeaders = () => ({ 'Content-Type': 'application/json', ...authHeaders() })

async function errText(r) {
  const t = await r.text()
  try { return JSON.parse(t).error || t } catch { return t || `HTTP ${r.status}` }
}

Alpine.store('mesh', {
  loading: false,
  saving: false,
  info: {},          // GET /v1/mesh/settings 除 settings 外的字段
  form: { enabled: true, node_name: '', extra_listen: [], peer_read_only: true, allow_relay: false, peer_file_root: '' },
  listenInput: '',
  restartRequired: false,

  peers: [],
  peersLoading: false,
  tokens: [],
  showTokens: false,

  // 邀请 / 接受弹窗
  showInvite: false,
  invite: { peer_name: '', result: null, busy: false },
  showAccept: false,
  accept: { name: '', invite: '', result: null, busy: false },

  t(k) { return Alpine.store('i18n').t(k) },
  toast(type, msg) { Alpine.store('toast').show(type, msg) },

  async load() {
    this.loading = true
    try {
      const r = await fetch('/v1/mesh/settings', { headers: authHeaders() })
      if (!r.ok) throw new Error(await errText(r))
      const d = await r.json()
      this.form = { ...d.settings, extra_listen: [...(d.settings.extra_listen || [])] }
      delete d.settings
      this.info = d
      if (d.running) { this.loadPeers(); this.loadTokens() }
    } catch (e) {
      this.toast('error', this.t('mesh_load_failed') + '：' + e.message)
    } finally {
      this.loading = false
    }
  },

  addListen(addr) {
    const a = (addr ?? this.listenInput).trim()
    if (!a) return
    if (!this.form.extra_listen.includes(a)) this.form.extra_listen.push(a)
    if (addr === undefined) this.listenInput = ''
  },

  removeListen(a) {
    this.form.extra_listen = this.form.extra_listen.filter((x) => x !== a)
  },

  unusedSuggestions() {
    return (this.info.suggestions || []).filter((s) => !this.form.extra_listen.includes(s.addr))
  },

  async save() {
    if (this.listenInput.trim()) this.addListen()
    this.saving = true
    try {
      const r = await fetch('/v1/mesh/settings', { method: 'PUT', headers: jsonHeaders(), body: JSON.stringify(this.form) })
      if (!r.ok) throw new Error(await errText(r))
      const d = await r.json()
      this.restartRequired = !!d.restart_required
      this.form = { ...d.settings, extra_listen: [...(d.settings.extra_listen || [])] }
      delete d.settings
      this.info = d
      this.toast('success', this.restartRequired ? this.t('mesh_saved_restart') : this.t('mesh_saved'))
    } catch (e) {
      this.toast('error', e.message)
    } finally {
      this.saving = false
    }
  },

  async loadPeers(probe = true) {
    this.peersLoading = true
    try {
      const r = await fetch('/v1/mesh/peers' + (probe ? '?probe=1' : ''), { headers: authHeaders() })
      if (!r.ok) throw new Error(await errText(r))
      this.peers = (await r.json()).peers || []
    } catch (e) {
      this.toast('error', e.message)
    } finally {
      this.peersLoading = false
    }
  },

  peerState(p) {
    if (p.last_error) return { cls: 'badge-ghost', text: this.t('mesh_peer_offline') }
    if (!p.mcp_connected) return { cls: 'badge-warning', text: this.t('mesh_peer_reconnecting') }
    return { cls: 'badge-success', text: this.t('mesh_peer_online') }
  },

  async removePeer(name) {
    if (!confirm(this.t('mesh_remove_confirm').replace('{0}', name))) return
    const r = await fetch(`/v1/mesh/peers/${encodeURIComponent(name)}`, { method: 'DELETE', headers: authHeaders() })
    if (!r.ok) { this.toast('error', await errText(r)); return }
    const d = await r.json()
    if (d.mcp_server_id) {
      await fetch(`/v1/mcp-servers/${encodeURIComponent(d.mcp_server_id)}`, { method: 'DELETE', headers: authHeaders() })
    }
    this.toast('success', this.t('mesh_removed').replace('{0}', name))
    this.loadPeers(false); this.loadTokens()
  },

  openInvite() {
    this.invite = { peer_name: '', result: null, busy: false }
    this.showInvite = true
  },

  async createInvite() {
    this.invite.busy = true
    try {
      const r = await fetch('/v1/mesh/invites', { method: 'POST', headers: jsonHeaders(), body: JSON.stringify({ peer_name: this.invite.peer_name.trim() }) })
      if (!r.ok) throw new Error(await errText(r))
      this.invite.result = await r.json()
      this.loadTokens()
    } catch (e) {
      this.toast('error', e.message)
    } finally {
      this.invite.busy = false
    }
  },

  openAccept() {
    this.accept = { name: '', invite: '', result: null, busy: false }
    this.showAccept = true
  },

  // 与 `polaris peer accept` 相同：登记对端并验卡签 → 经 /v1/mcp-servers 走安装授权登记委派通道（先删后建，可重复执行）。
  async acceptInvite() {
    this.accept.busy = true
    try {
      const r = await fetch('/v1/mesh/peers', { method: 'POST', headers: jsonHeaders(),
        body: JSON.stringify({ name: this.accept.name.trim(), invite: this.accept.invite.trim() }) })
      if (!r.ok) throw new Error(await errText(r))
      const d = await r.json()
      if (d.mcp_server?.id) {
        await fetch(`/v1/mcp-servers/${encodeURIComponent(d.mcp_server.id)}`, { method: 'DELETE', headers: authHeaders() })
        const m = await fetch('/v1/mcp-servers', { method: 'POST', headers: jsonHeaders(), body: JSON.stringify(d.mcp_server) })
        if (!m.ok) throw new Error(this.t('mesh_mcp_register_failed') + '：' + await errText(m))
      }
      this.accept.result = d
      this.loadPeers(false); this.loadTokens()
    } catch (e) {
      this.toast('error', e.message)
    } finally {
      this.accept.busy = false
    }
  },

  async loadTokens() {
    try {
      const r = await fetch('/v1/mesh/tokens', { headers: authHeaders() })
      if (r.ok) this.tokens = (await r.json()).tokens || []
    } catch { /* 令牌列表非关键，静默 */ }
  },

  async revokeToken(id) {
    if (!confirm(this.t('mesh_revoke_confirm'))) return
    const r = await fetch(`/v1/mesh/tokens/${encodeURIComponent(id)}`, { method: 'DELETE', headers: authHeaders() })
    if (!r.ok) { this.toast('error', await errText(r)); return }
    this.loadTokens()
  },

  async copy(text) {
    try { await navigator.clipboard.writeText(text); this.toast('success', this.t('mesh_copied')) } catch { /* 静默 */ }
  },

  fmtTime(s) {
    if (!s || s.startsWith('0001')) return '—'
    try { return new Date(s).toLocaleString() } catch { return s }
  },
})
