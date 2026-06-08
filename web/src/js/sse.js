/**
 * SSE 客户端：EventSource 封装 + 指数退避重连 + Alpine store 驱动
 * 对齐 M13-Interface-WebUI.md §3 协议
 */

const RETRY_DELAYS = [1000, 2000, 4000, 8000, 16000, 30000]
const MAX_RETRIES = 10

export class SSEClient {
  #url = ''
  #body = null
  #retryCount = 0
  #abortCtrl = null
  #onEvent = null
  #onError = null
  #onComplete = null
  #active = false

  /**
   * @param {object} opts
   * @param {string}   opts.url
   * @param {object}   opts.body        POST body
   * @param {Function} opts.onEvent     (type, data) => void
   * @param {Function} opts.onError     (err) => void
   * @param {Function} opts.onComplete  () => void
   */
  constructor(opts) {
    this.#url = opts.url
    this.#body = opts.body
    this.#onEvent = opts.onEvent
    this.#onError = opts.onError
    this.#onComplete = opts.onComplete
  }

  start() {
    this.#active = true
    this.#retryCount = 0
    this.#connect()
  }

  stop() {
    this.#active = false
    this.#abortCtrl?.abort()
  }

  async #connect() {
    if (!this.#active) return

    this.#abortCtrl = new AbortController()

    try {
      const token = sessionStorage.getItem('polaris_token') || ''
      const resp = await fetch(this.#url, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(token ? { 'X-Session-Token': token } : {}),
        },
        body: JSON.stringify(this.#body),
        signal: this.#abortCtrl.signal,
      })

      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}`)
      }

      const reader = resp.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''

      while (this.#active) {
        const { value, done } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        buf = this.#flushBuffer(buf)
      }

      // 正常结束（服务器关闭流）
      if (this.#active) {
        this.#onComplete?.()
      }
    } catch (err) {
      if (!this.#active || err.name === 'AbortError') return
      this.#scheduleRetry(err)
    }
  }

  /**
   * 解析 text/event-stream 缓冲区，返回剩余未处理部分
   * 格式: event: <type>\ndata: <json>\n\n
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

    for (const line of lines) {
      if (line.startsWith('event:')) {
        eventType = line.slice(6).trim()
      } else if (line.startsWith('data:')) {
        dataStr = line.slice(5).trim()
      }
    }

    if (!dataStr) return

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
    console.warn(`[SSE] retry ${this.#retryCount}/${MAX_RETRIES} in ${delay}ms:`, err.message)
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
