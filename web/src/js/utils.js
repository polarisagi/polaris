export function sessionToken() {
  return sessionStorage.getItem('polaris_token') || ''
}

export function authHeaders() {
  const t = sessionToken()
  return t ? { 'X-Session-Token': t, 'Content-Type': 'application/json' } : { 'Content-Type': 'application/json' }
}

/**
 * 消息文本显示规范化（M13-Interface-WebUI.md §9）
 * 剥离工具调用 XML、投递指令标签、控制 token
 */
export function levelGe(level, min) {
  const order = { debug: 0, info: 1, warn: 2, error: 3 }
  return (order[level] ?? -1) >= (order[min] ?? 99)
}

export function sanitizeContent(text) {
  if (!text) return ''
  return text
    .replace(/<tool_calls?>[\s\S]*?<\/tool_calls?>/gi, '')
    .replace(/<function_calls?>[\s\S]*?<\/function_calls?>/gi, '')
    .replace(/\[\[[\w_*]+\]\]/g, '')
    .replace(/^\s*(NO_REPLY|no_reply)\s*$/m, '')
    .replace(/\[System:[^\]]*\]/g, '')
    .trim()
}
