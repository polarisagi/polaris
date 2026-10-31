function isRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function nonEmptyString(value) {
  return typeof value === 'string' ? value.trim() : ''
}

function defaultMessage(status) {
  return Number.isFinite(status) ? `HTTP ${status}` : 'HTTP request failed'
}

function withStatus(status, message) {
  if (!Number.isFinite(status) || message === defaultMessage(status)) return message
  return `HTTP ${status}: ${message}`
}

function decodePayload(payload, fallback) {
  if (!isRecord(payload)) return { code: '', message: fallback }

  const nestedError = isRecord(payload.error) ? payload.error : null
  const message = nonEmptyString(payload.message)
    || nonEmptyString(payload.error)
    || nonEmptyString(nestedError?.message)
    || fallback
  const code = nonEmptyString(payload.code) || nonEmptyString(nestedError?.code)
  return { code, message }
}

// HTTP 错误体只能消费一次；坏 JSON 不应把内部结构原样显示到界面。
export async function readHTTPError(response) {
  const status = response?.status
  const fallback = defaultMessage(status)
  let raw
  try {
    raw = (await response.text()).trim()
  } catch (error) {
    if (error?.name === 'AbortError') throw error
    return { code: '', message: fallback, status, payload: null }
  }

  if (!raw) return { code: '', message: fallback, status, payload: null }

  let payload
  try {
    payload = JSON.parse(raw)
  } catch {
    if (/^[\[{]/.test(raw)) return { code: '', message: fallback, status, payload: null }
    return { code: '', message: withStatus(status, raw), status, payload: null }
  }

  const decoded = decodePayload(payload, fallback)
  return { ...decoded, message: withStatus(status, decoded.message), status, payload }
}

export async function readHTTPErrorMessage(response) {
  return (await readHTTPError(response)).message
}
