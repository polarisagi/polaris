/**
 * DOM-based HTML & Markdown Sanitizer (CONTRACT & PROMPT-B compliance)
 *
 * Enforces a strict whitelist of tags and attributes.
 * Drops script, style, iframe, form, SVG, MathML, audio, video, object, embed, canvas, etc.
 * Drops all on* event handlers, style, srcdoc, and dangerous attributes.
 * Neutralizes javascript:, data:, vbscript: and malformed/encoded protocols.
 * Blocks remote image tracking pixels ("禁止远端图片自动追踪").
 * Resolves relative URLs against source base_url and adds noopener/noreferrer to links.
 */

const DANGEROUS_DROP_TAGS = new Set([
  'script', 'style', 'iframe', 'frame', 'frameset', 'object', 'embed',
  'applet', 'form', 'input', 'textarea', 'button', 'select', 'option',
  'svg', 'math', 'base', 'meta', 'link', 'noscript', 'canvas',
  'audio', 'video', 'source', 'track', 'portal', 'template'
])

const ALLOWED_TAGS = new Set([
  'p', 'br', 'hr', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'strong', 'em', 'b', 'i', 'u', 's', 'del', 'strike', 'ins',
  'blockquote', 'code', 'pre', 'ul', 'ol', 'li',
  'table', 'thead', 'tbody', 'tfoot', 'tr', 'th', 'td',
  'a', 'span', 'div', 'details', 'summary', 'dd', 'dt', 'dl',
  'kbd', 'sub', 'sup'
])

const ALLOWED_ATTRS = {
  a: new Set(['href', 'title', 'target', 'rel']),
  th: new Set(['align', 'colspan', 'rowspan']),
  td: new Set(['align', 'colspan', 'rowspan']),
  details: new Set(['open']),
  code: new Set(['class']),
  pre: new Set(['class']),
  span: new Set(['class']),
  div: new Set(['class'])
}

/**
 * Sanitize and resolve URL safely.
 * Returns null if unsafe or blocked.
 */
export function sanitizeUrl(rawUrl, baseUrl) {
  if (!rawUrl || typeof rawUrl !== 'string') return null

  // Strip all ASCII control characters, newlines, tabs, and whitespace
  // (guards against java\nscript:, jav&#x09;ascript:, etc.)
  const cleaned = rawUrl.replace(/[\u0000-\u0020\u007F-\u009F\s]+/g, '').trim()
  if (!cleaned) return null

  // In-page anchor
  if (cleaned.startsWith('#')) {
    return cleaned
  }

  // Check scheme
  const schemeMatch = cleaned.match(/^([a-zA-Z][a-zA-Z0-9+.-]*):/)
  if (!schemeMatch) {
    // Relative link
    if (baseUrl) {
      try {
        const normalizedBase = baseUrl.endsWith('/') ? baseUrl : baseUrl + '/'
        const resolved = new URL(cleaned, normalizedBase)
        if (resolved.protocol === 'http:' || resolved.protocol === 'https:') {
          return resolved.href
        }
      } catch {
        return null
      }
    }
    // Relative path without baseUrl: allow if safe relative path
    if (cleaned.startsWith('/') || cleaned.startsWith('./') || cleaned.startsWith('../') || !cleaned.includes(':')) {
      return cleaned
    }
    return null
  }

  const scheme = schemeMatch[1].toLowerCase()
  if (scheme === 'http' || scheme === 'https' || scheme === 'mailto') {
    try {
      const parsed = new URL(cleaned)
      if (parsed.protocol === 'http:' || parsed.protocol === 'https:' || parsed.protocol === 'mailto:') {
        return parsed.href
      }
    } catch {
      return null
    }
  }

  // Any other scheme (javascript, data, vbscript, file, etc.) is rejected
  return null
}

let _customParser = null
export function setDOMParser(parser) {
  _customParser = parser
}

function parseHTMLToDoc(html) {
  if (_customParser) {
    return _customParser(html)
  }
  if (typeof DOMParser !== 'undefined') {
    const parser = new DOMParser()
    return parser.parseFromString(`<!DOCTYPE html><html><body>${html}</body></html>`, 'text/html')
  }
  if (typeof globalThis.DOMParser !== 'undefined') {
    const parser = new globalThis.DOMParser()
    return parser.parseFromString(`<!DOCTYPE html><html><body>${html}</body></html>`, 'text/html')
  }
  throw new Error('No DOMParser available in environment')
}

/**
 * Sanitize HTML string using DOM tree traversal
 * @param {string} rawHtml - Unsanitized HTML
 * @param {object} options - { baseUrl, allowImages }
 * @returns {string} Clean HTML
 */
export function sanitizeHtml(rawHtml, options = {}) {
  if (!rawHtml || typeof rawHtml !== 'string') return ''

  const doc = parseHTMLToDoc(rawHtml)
  const body = doc.body || doc

  function cleanNode(node) {
    if (!node) return

    // Comment node: remove
    if (node.nodeType === 8) {
      node.remove()
      return
    }

    // Element node
    if (node.nodeType === 1) {
      const tag = node.nodeName.toLowerCase()

      // Drop dangerous elements and their entire subtrees
      if (DANGEROUS_DROP_TAGS.has(tag)) {
        node.remove()
        return
      }

      // Handle images: prevent remote image tracking ("禁止远端图片自动追踪")
      if (tag === 'img') {
        const alt = node.getAttribute('alt')
        if (alt) {
          const placeholder = doc.createTextNode(`[图片: ${alt}]`)
          node.parentNode.replaceChild(placeholder, node)
        } else {
          node.remove()
        }
        return
      }

      // Unknown or unallowed tag: unwrap its children into the parent and recursively clean them
      if (!ALLOWED_TAGS.has(tag)) {
        const parent = node.parentNode
        if (parent) {
          const children = Array.from(node.childNodes || [])
          for (const child of children) {
            parent.insertBefore(child, node)
            cleanNode(child)
          }
          parent.removeChild(node)
        } else {
          node.remove()
        }
        return
      }

      // Process and filter attributes
      const allowedAttrs = ALLOWED_ATTRS[tag] || new Set()
      const attrs = Array.from(node.attributes || [])
      for (const attr of attrs) {
        const name = attr.name.toLowerCase()

        // Strip on* event handlers, style, srcdoc, form actions, etc.
        if (name.startsWith('on') || name === 'srcdoc' || name === 'style' || name === 'action' || name === 'formaction' || name.startsWith('data-')) {
          node.removeAttribute(attr.name)
          continue
        }

        if (!allowedAttrs.has(name)) {
          node.removeAttribute(attr.name)
          continue
        }

        // Special check for a[href]
        if (tag === 'a' && name === 'href') {
          const safeUrl = sanitizeUrl(attr.value, options.baseUrl)
          if (safeUrl) {
            node.setAttribute('href', safeUrl)
          } else {
            node.removeAttribute('href')
          }
        }

        // Class validation (allow safe identifier strings for syntax highlighting)
        if (name === 'class') {
          const val = attr.value.trim()
          if (!/^[a-zA-Z0-9_\-\s]+$/.test(val)) {
            node.removeAttribute('class')
          }
        }
      }

      // Always secure external links with target and rel
      if (tag === 'a' && node.hasAttribute('href')) {
        node.setAttribute('target', '_blank')
        node.setAttribute('rel', 'noopener noreferrer')
      }

      // Recursively process children (copy array to avoid index shifting during node removal)
      const children = Array.from(node.childNodes || [])
      for (const child of children) {
        cleanNode(child)
      }
    }
  }

  const nodes = Array.from(body.childNodes || [])
  for (const node of nodes) {
    cleanNode(node)
  }

  return body.innerHTML
}
