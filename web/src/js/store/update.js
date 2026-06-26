import Alpine from 'alpinejs'

const GITHUB_API = 'https://api.github.com/repos/polarisagi/polaris/releases/latest'
// 轮询间隔：页面加载 10s 后首检，此后每 30 分钟一次
const CHECK_INTERVAL_MS = 30 * 60 * 1000
const INITIAL_DELAY_MS  = 10_000

Alpine.store('update', {
  current: '',       // 由后端 /v1/system/version 提供
  latest: '',        // 由 GitHub API 提供
  hasUpdate: false,
  status: 'idle',    // idle | downloading | verifying | installing | restarting | error
  error: '',
  releaseURL: '',
  releaseNotes: '',

  get busy() {
    return this.status !== 'idle' && this.status !== 'error'
  },

  get statusLabel() {
    return {
      idle:        '',
      downloading: '下载中…',
      verifying:   '校验中…',
      installing:  '安装中…',
      restarting:  '重启中…',
      error:       '更新失败',
    }[this.status] ?? this.status
  },

  // ── 1. 从后端获取当前版本 ────────────────────────────────────────────────
  async fetchCurrent() {
    try {
      const resp = await fetch('/v1/system/version')
      if (!resp.ok) return
      const d = await resp.json()
      this.current = d.current || ''
      
      if (d.latest) {
        this.latest = d.latest
        this.hasUpdate = d.has_update || false
        this.releaseURL = d.release_url || ''
      }

      // 同步后端的更新进度（用户刷新页面时恢复状态）
      if (d.update_status && d.update_status !== 'idle') {
        this.status = d.update_status
        this.error  = d.update_error || ''
      }
    } catch { /* 静默失败 */ }
  },

  // ── 2. 直接调 GitHub API 对比版本（不经过后端，前端承担 rate limit）──────
  async checkGitHub() {
    if (!this.current) return
    try {
      const resp = await fetch(GITHUB_API, {
        headers: { Accept: 'application/vnd.github+json' },
        // 缓存 5 分钟，避免同一页面重复请求
        cache: 'default',
      })
      if (!resp.ok) return
      const rel = await resp.json()
      this.latest       = rel.tag_name || ''
      this.releaseURL   = rel.html_url || ''
      this.releaseNotes = rel.body     || ''
      this.hasUpdate    = this._isNewer(this.latest, this.current)
    } catch { /* GitHub 不可达时静默跳过 */ }
  },

  // ── 3. 用户确认后向后端发起热更新 ───────────────────────────────────────
  async triggerUpdate() {
    if (!this.latest || this.busy) return
    this.status = 'downloading'
    this.error  = ''
    try {
      const resp = await fetch('/v1/system/update', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ version: this.latest }),
      })
      if (!resp.ok) {
        this.status = 'error'
        this.error  = await resp.text()
        return
      }
      this._pollStatus()
    } catch (e) {
      this.status = 'error'
      this.error  = e.message
    }
  },

  // 每 2s 轮询后端进度，直到不再 busy
  _pollStatus() {
    const iv = setInterval(async () => {
      try {
        const resp = await fetch('/v1/system/version')
        if (!resp.ok) return
        const d = await resp.json()
        this.status = d.update_status || 'idle'
        this.error  = d.update_error  || ''
        if (!this.busy) clearInterval(iv)
      } catch { clearInterval(iv) }
    }, 2000)
  },

  // semver / tag 比较：latest > current → true
  _isNewer(latest, current) {
    if (!latest || !current) return false
    const strip = s => s.replace(/^v/, '')
    const parse = s => {
      let pre = ""
      const m = s.match(/([-\+].+)/)
      if (m) {
        pre = m[1]
        s = s.replace(/([-\+].+)/, '')
      }
      return { n: s.split('.').map(n => parseInt(n, 10) || 0), pre }
    }
    const a = parse(strip(latest))
    const b = parse(strip(current))
    
    for (let i = 0; i < 3; i++) {
      const la = a.n[i] || 0
      const ca = b.n[i] || 0
      if (la > ca) return true
      if (la < ca) return false
    }
    
    if (a.pre === b.pre) return false
    if (!a.pre) return true
    if (!b.pre) return false
    return a.pre > b.pre
  },

  async init() {
    await this.fetchCurrent()
    setTimeout(async () => {
      await this.checkGitHub()
      setInterval(() => this.checkGitHub(), CHECK_INTERVAL_MS)
    }, INITIAL_DELAY_MS)
  },
})
