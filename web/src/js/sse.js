/**
 * SSE 客户端：fetch 流式读取 + 断线续传 + Alpine store 驱动
 * 对齐 M13-Interface-WebUI.md §3 协议
 *
 * 回合与连接解耦（ADR-0116）后的两条铁律：
 *  1. POST /v1/agent/stream 只发一次。连接中断后一律改走
 *     GET /v1/sessions/{id}/stream?after=<lastSeq> 续传——重新 POST 会重复提交一轮、烧 token。
 *  2. 服务端每帧带 `id: <seq>`；客户端记住最大 seq，续传时只取其后的事件。
 */

const RETRY_DELAYS = [1000, 2000, 4000, 8000, 16000, 30000]
const MAX_RETRIES = 10

export class SSEClient {
  #url = ''
  #method = 'POST'
  #body = null
  #sid = ''
  #lastSeq = 0
  #started = false
  #terminal = false
  #retryCount = 0
  #abortCtrl = null
  #onEvent = null
  #onError = null
  #onComplete = null
  #onNoContent = null
  #active = false

  /**
   * @param {object} opts
   * @param {string}   opts.url
   * @param {string}   [opts.method]     'POST'（默认，发起回合）| 'GET'（仅订阅，需 sessionID）
   * @param {object}   [opts.body]       POST body
   * @param {string}   [opts.sessionID]  GET 订阅的会话；POST 模式下由 session 事件自动获知
   * @param {Function} opts.onEvent      (type, data) => void
   * @param {Function} opts.onError      (err) => void
   * @param {Function} opts.onComplete   () => void
   * @param {Function} [opts.onNoContent] GET 订阅时服务端无回合（204）
   */
  constructor(opts) {
    this.#url = opts.url
    this.#method = opts.method || 'POST'
    this.#body = opts.body
    this.#sid = opts.sessionID || ''
    this.#onEvent = opts.onEvent
    this.#onError = opts.onError
    this.#onComplete = opts.onComplete
    this.#onNoContent = opts.onNoContent
  }

  get sessionID() { return this.#sid }
  get lastSeq() { return this.#lastSeq }

  start() {
    this.#active = true
    this.#retryCount = 0
    this.#connect()
  }

  stop() {
    this.#active = false
    this.#abortCtrl?.abort()
  }

  #streamURL() {
    return `/v1/sessions/${encodeURIComponent(this.#sid)}/stream?after=${this.#lastSeq}`
  }

  async #connect() {
    if (!this.#active) return

    // 首次连接按构造参数；此后（含 POST 模式的断线重连）只订阅，绝不再 POST。
    let method = this.#method
    let url = this.#url
    if (method === 'GET') {
      url = this.#streamURL()
    } else if (this.#started) {
      if (!this.#sid) {
        // 会话 ID 尚未获知就断了：无从续传，也不能冒险重发。
        this.#onError?.({ code: 'connection_lost', message: '连接中断' })
        return
      }
      method = 'GET'
      url = this.#streamURL()
    }
    this.#started = true
    this.#abortCtrl = new AbortController()

    try {
      const token = sessionStorage.getItem('polaris_token') || ''
      const headers = token ? { 'X-Session-Token': token } : {}
      const init = { method, headers, signal: this.#abortCtrl.signal }
      if (method === 'POST') {
        headers['Content-Type'] = 'application/json'
        init.body = JSON.stringify(this.#body)
      }
      const resp = await fetch(url, init)

      if (resp.status === 204) {
        this.#active = false
        this.#onNoContent?.()
        return
      }
      if (!resp.ok) {
        const err = await this.#httpError(resp)
        // POST 失败（含 409 session_busy）不重试；GET 续传失败才走退避重试。
        if (method === 'POST' || resp.status === 401 || resp.status === 403 || resp.status === 400) {
          this.#active = false
          this.#onError?.(err)
          return
        }
        throw new Error(err.message)
      }

      const reader = resp.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''

      while (this.#active) {
        const { value, done } = await reader.read()
        if (done) break
        this.#retryCount = 0
        buf += decoder.decode(value, { stream: true })
        buf = this.#flushBuffer(buf)
      }
      if (!this.#active) return

      // 流关闭：见到终止事件才算正常结束；否则是连接被掐断，按 seq 续传。
      if (this.#terminal) {
        this.#onComplete?.()
      } else {
        this.#scheduleRetry(new Error('stream closed without terminal event'))
      }
    } catch (err) {
      if (!this.#active || err.name === 'AbortError') return
      this.#scheduleRetry(err)
    }
  }

  async #httpError(resp) {
    let code = `http_${resp.status}`
    let message = `HTTP ${resp.status}`
    try {
      const j = JSON.parse(await resp.text())
      if (j.code) code = j.code
      if (j.message) message = j.message
    } catch { /* 非 JSON 错误体 */ }
    return { code, message }
  }

  /**
   * 解析 text/event-stream 缓冲区，返回剩余未处理部分
   * 格式: [id: <seq>\n]event: <type>\ndata: <json>\n\n
   * 兼容旧格式: data: {"type":"...","data":"..."}\n\n
   */
  #flushBuffer(buf) {
    const chunks = buf.split('\n\n')
    const remaining = chunks.pop() // 最后一段可能不完整

    for (const chunk of chunks) {
      if (!chunk.trim()) continue
      this.#parseChunk(chunk)
    }

    return remaining
  }

  #parseChunk(chunk) {
    const lines = chunk.split('\n')
    let eventType = 'message'
    let dataStr = ''
    let seq = 0

    for (const line of lines) {
      if (line.startsWith('id:')) {
        seq = parseInt(line.slice(3).trim(), 10) || 0
      } else if (line.startsWith('event:')) {
        eventType = line.slice(6).trim()
      } else if (line.startsWith('data:')) {
        dataStr = line.slice(5).trim()
      }
    }

    if (!dataStr) return
    // 重放去重：续传按 after 取 seq 之后的事件，这里再兜底丢弃已处理过的帧。
    if (seq) {
      if (seq <= this.#lastSeq) return
      this.#lastSeq = seq
    }

    let data
    try {
      data = JSON.parse(dataStr)
    } catch {
      return
    }

    // 兼容 MVP 格式: {"type":"agent_event"|"thinking"|"complete", ...}
    if (eventType === 'message' && data.type) {
      eventType = this.#mapLegacyType(data.type)
      data = this.#normalizeLegacyData(data)
    }

    if (eventType === 'session' && data.session_id) this.#sid = data.session_id
    if (eventType === 'complete' || eventType === 'error') this.#terminal = true

    this.#onEvent?.(eventType, data)
  }

  #mapLegacyType(t) {
    const map = {
      agent_event: 'token',
      thinking: 'thinking',
      complete: 'complete',
      error: 'error',
      tool_call: 'tool_call',
      tool_result: 'tool_result',
    }
    return map[t] || t
  }

  #normalizeLegacyData(d) {
    if (d.type === 'agent_event') return { content: d.payload || d.data || '' }
    if (d.type === 'thinking') return { content: d.data || d.content || '' }
    if (d.type === 'complete') return {}
    if (d.type === 'error') return { code: d.code || 'unknown', message: d.message || d.data || '' }
    return d
  }

  #scheduleRetry(err) {
    if (this.#retryCount >= MAX_RETRIES) {
      this.#onError?.({ code: 'max_retries', message: `连接失败，已重试 ${MAX_RETRIES} 次` })
      return
    }
    const delay = RETRY_DELAYS[Math.min(this.#retryCount, RETRY_DELAYS.length - 1)]
    this.#retryCount++
    console.warn(`[SSE] resume ${this.#retryCount}/${MAX_RETRIES} in ${delay}ms:`, err.message)
    setTimeout(() => this.#connect(), delay)
  }
}

/**
 * 幂等提交去重：相同 (sessionID, input) 在 5s 窗口内返回同一 runID
 */
const _dedupCache = new Map()
export function dedupeRunID(sessionID, input) {
  const key = `${sessionID}::${input}`
  const now = Date.now()
  const cached = _dedupCache.get(key)
  if (cached && now - cached.ts < 5000) return cached.id
  const id = crypto.randomUUID()
  _dedupCache.set(key, { id, ts: now })
  // 定期清理
  if (_dedupCache.size > 50) {
    for (const [k, v] of _dedupCache) {
      if (now - v.ts > 10000) _dedupCache.delete(k)
    }
  }
  return id
}
