import Alpine from 'alpinejs'

// ══════════════════════════════════════════════════════════════════════════
// 统一 fetch 包装：本地令牌失效（401）时自愈
//
// 现象：守护进程重启（或令牌轮换）后，浏览器里的 polaris_local Cookie 与旧令牌不再匹配，
// 所有 /v1 请求（含状态栏、审批轮询、SSE）返回 401，用户只能手动刷新页面。
// 做法：对同源 /v1 请求，遇 401 时重新请求一次静态外壳 "/"（服务端对回环请求在该响应里
// 重新签发 Cookie），然后重试原请求一次；仍 401 才亮顶部横幅让用户手动重连。
// 刷新是单飞的：并发的多个 401 共用同一次刷新，不会把外壳请求放大。
// ══════════════════════════════════════════════════════════════════════════

const nativeFetch = window.fetch.bind(window)

let refreshing = null

/** 重新请求外壳以换取新 Cookie；返回是否成功（HTTP 2xx）。并发调用共用同一次请求。 */
function refreshCookie() {
  if (!refreshing) {
    refreshing = nativeFetch('/', { cache: 'no-store', credentials: 'same-origin', headers: { Accept: 'text/html' } })
      .then((r) => r.ok)
      .catch(() => false)
      .finally(() => { refreshing = null })
  }
  return refreshing
}

function isLocalAPI(input) {
  try {
    const raw = typeof input === 'string' || input instanceof URL ? String(input) : input.url
    const u = new URL(raw, location.href)
    return u.origin === location.origin && u.pathname.startsWith('/v1/')
  } catch {
    return false
  }
}

Alpine.store('net', {
  /** 重试一次后仍 401：连接已失效，需要用户点击横幅重连。 */
  broken: false,
  refreshing: false,

  async reconnect() {
    this.refreshing = true
    try {
      await refreshCookie()
    } finally {
      this.refreshing = false
    }
    // 状态已无法就地恢复（流/轮询均已放弃），整页重载最稳妥。
    location.reload()
  },

  /** 给 EventSource 这类无法走 fetch 包装的通道用：先换 Cookie，再重连。 */
  refresh: refreshCookie,
})

window.fetch = async function polarisFetch(input, init) {
  if (!isLocalAPI(input)) return nativeFetch(input, init)

  // Request 对象的 body 只能消费一次：重试需要的副本在发送前克隆。
  const retryInput = input instanceof Request ? input.clone() : input
  const resp = await nativeFetch(input, init)
  const net = Alpine.store('net')
  if (resp.status !== 401) {
    if (net?.broken && resp.ok) net.broken = false
    return resp
  }
  if (init?.signal?.aborted) return resp

  const ok = await refreshCookie()
  if (!ok) {
    if (net) net.broken = true
    return resp
  }
  const retried = await nativeFetch(retryInput, init)
  if (net) net.broken = retried.status === 401
  return retried
}
