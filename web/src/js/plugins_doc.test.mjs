import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import { createRequire } from 'node:module'
import { marked } from 'marked'
import { sanitizeHtml, sanitizeUrl, setDOMParser } from './sanitize.mjs'

const require = createRequire(import.meta.url)
const { parseHTML } = require('linkedom')

// Set linkedom parser for test environment
setDOMParser((html) => {
  return parseHTML(`<!DOCTYPE html><html><body>${html}</body></html>`).document
})

function renderDocMarkdown(text, baseUrl) {
  if (!text) return ''
  const html = marked.parse(text)
  return sanitizeHtml(html, { baseUrl, allowImages: false })
}

// Load production plugins store source
const pluginsJsCode = fs
  .readFileSync(new URL('./store/plugins.js', import.meta.url), 'utf8')
  .replace(/^import .*\n/gm, '')

// Load Session A real contract fixtures
const fixtureReady = JSON.parse(
  fs.readFileSync(new URL('../../../local_playground/extension-readme-i18n/fixtures/documentation-ready.json', import.meta.url), 'utf8')
)
const fixtureMissing = JSON.parse(
  fs.readFileSync(new URL('../../../local_playground/extension-readme-i18n/fixtures/documentation-missing.json', import.meta.url), 'utf8')
)
const fixtureTask = JSON.parse(
  fs.readFileSync(new URL('../../../local_playground/extension-readme-i18n/fixtures/translation-task.json', import.meta.url), 'utf8')
)

/**
 * Creates an instance of the REAL production Alpine store from store/plugins.js
 * evaluated in an isolated VM sandbox.
 */
function createRealStore(options = {}) {
  const {
    fetchImpl = async () => ({ ok: true, status: 200, json: async () => ({}) }),
    lang = 'zh',
    timers = {}
  } = options

  let storeInstance = null
  const i18n = {
    lang,
    t: (key) => key
  }

  const activeTimeouts = new Map()
  let timerCounter = 1

  const ctx = vm.createContext({
    Alpine: {
      store(name, value) {
        if (value) {
          storeInstance = value
          return value
        }
        if (name === 'i18n') return i18n
        if (name === 'plugins') return storeInstance
        return {}
      }
    },
    authHeaders: () => ({ 'Content-Type': 'application/json' }),
    readHTTPError: async () => null,
    readHTTPErrorMessage: async (res) => (res && res.statusText) || 'HTTP error',
    mcpAppsHost: {},
    AbortController,
    URLSearchParams,
    console,
    fetch: (...args) => fetchImpl(...args),
    setTimeout: (cb, ms) => {
      const id = timerCounter++
      if (timers.onSetTimeout) {
        timers.onSetTimeout(id, cb, ms)
        return id
      }
      const t = setTimeout(() => {
        activeTimeouts.delete(id)
        cb()
      }, ms)
      activeTimeouts.set(id, t)
      return id
    },
    clearTimeout: (id) => {
      if (timers.onClearTimeout) {
        timers.onClearTimeout(id)
        return
      }
      const t = activeTimeouts.get(id)
      if (t) {
        clearTimeout(t)
        activeTimeouts.delete(id)
      }
    },
    setInterval: (cb, ms) => {
      const id = timerCounter++
      if (timers.onSetInterval) {
        timers.onSetInterval(id, cb, ms)
        return id
      }
      return id
    },
    clearInterval: (id) => {
      if (timers.onClearInterval) {
        timers.onClearInterval(id)
      }
    }
  })

  vm.runInContext(pluginsJsCode, ctx)
  return { store: storeInstance, ctx, activeTimeouts }
}

// ============================================================================
// B1 回归测试：HTML Sanitizer 未知标签递归清理与 XSS 防御
// ============================================================================

test('B1 Sanitizer: recursively sanitizes children of unallowed wrapper tags', () => {
  const input = '<section><img src=x onerror="alert(1)"><a href="javascript:alert(2)">click</a></section>'
  const sanitized = sanitizeHtml(input)

  assert.equal(sanitized.includes('onerror='), false, 'onerror event handler must be stripped')
  assert.equal(sanitized.includes('javascript:'), false, 'javascript: protocol must be stripped')
  assert.equal(sanitized.includes('<img'), false, 'tracking <img> must be stripped')
  assert.equal(sanitized.includes('<a>click</a>') || sanitized.includes('click'), true, 'safe content remains')
})

test('B1 Sanitizer: preserves valid formatting elements inside unallowed wrappers', () => {
  const input = '<section><p>Paragraph <b>bold</b> <a href="https://example.com">link</a></p></section>'
  const sanitized = sanitizeHtml(input)

  assert.equal(sanitized.includes('<section>'), false, 'unallowed wrapper must be removed')
  assert.equal(sanitized.includes('<p>Paragraph <b>bold</b>'), true, 'inner formatting preserved')
  assert.equal(sanitized.includes('href="https://example.com/"'), true, 'safe link preserved with normalized URL')
  assert.equal(sanitized.includes('rel="noopener noreferrer"'), true, 'security attributes attached')
})

test('B1 Sanitizer: strips HTML comments and dangerous embedded payloads', () => {
  const input = '<div><!-- <script>alert(1)</script> -->Visible content</div>'
  const sanitized = sanitizeHtml(input)

  assert.equal(sanitized.includes('<!--'), false, 'comments must be removed')
  assert.equal(sanitized.includes('alert'), false)
  assert.match(sanitized, /Visible content/)
})

// ============================================================================
// B2 回归测试：已安装插件与直装条目的 catalog_id 映射
// ============================================================================

test('B2 TargetCatalogId: installed row with pl_ runtime ID correctly resolves to catalog_id', () => {
  const { store } = createRealStore()

  const installedRow = {
    id: 'pl_01932b6e1234',
    catalog_id: 'custom-market/calculator-tool',
    name: 'Calculator'
  }
  assert.equal(store.targetCatalogId(installedRow), 'custom-market/calculator-tool')
})

test('B2 TargetCatalogId: uncataloged direct install ext_ ID is retained', () => {
  const { store } = createRealStore()

  const directInstallRow = {
    id: 'ext_custom_repo_abc123',
    catalog_id: 'ext_custom_repo_abc123',
    name: 'Direct Installed Extension'
  }
  assert.equal(store.targetCatalogId(directInstallRow), 'ext_custom_repo_abc123')
})

test('B2 TargetCatalogId: normal catalog market entry uses entry.id', () => {
  const { store } = createRealStore()

  const catalogEntry = {
    id: 'polarisagi/polaris-plugins-official/weather-pro',
    name: 'Weather Pro'
  }
  assert.equal(store.targetCatalogId(catalogEntry), 'polarisagi/polaris-plugins-official/weather-pro')
})

test('B2 loadDocumentation: requests documentation using target catalog ID, never pl_xxx', async () => {
  let requestedUrl = null
  const { store } = createRealStore({
    fetchImpl: async (url) => {
      requestedUrl = url
      return {
        ok: true,
        status: 200,
        json: async () => fixtureReady
      }
    }
  })

  store.showDetail = true
  store.detail = {
    id: 'pl_runtime_987654',
    catalog_id: 'polarisagi/polaris-plugins-official/weather-pro'
  }
  store.catalog = [
    { id: 'polarisagi/polaris-plugins-official/weather-pro', type: 'plugin' }
  ]

  const entry = store.currentDocEntry()
  await store.loadDocumentation(entry)

  assert.ok(requestedUrl, 'fetch should have been called')
  const parsed = new URL(requestedUrl, 'https://example.test')
  assert.equal(parsed.searchParams.get('catalog_id'), 'polarisagi/polaris-plugins-official/weather-pro')
  assert.notEqual(parsed.searchParams.get('catalog_id'), 'pl_runtime_987654')
})

// ============================================================================
// B3 回归测试：后端 ready/en/untranslated 与 missing/no-source 状态条件
// ============================================================================

test('B3 docNeedsTranslation: returns true for ready status with English fallback and untranslated reason', () => {
  const { store } = createRealStore({ lang: 'zh' })
  store.docLocale = 'zh-CN'

  // Backend returned ready doc with English fallback
  store.doc = {
    catalog_id: 'custom/tool',
    status: 'ready',
    resolved_locale: 'en',
    markdown: '# English Documentation',
    fallback_reason: 'untranslated',
    stale: false,
    source: { sha256: 'a'.repeat(64) }
  }

  assert.equal(store.hasDocSource(), true, 'has source document')
  assert.equal(store.docNeedsTranslation(), true, 'should need translation into Chinese')
})

test('B3 docNeedsTranslation: returns false for missing status with no_source_document', () => {
  const { store } = createRealStore({ lang: 'zh' })
  store.docLocale = 'zh-CN'

  // Backend returned missing status because upstream has no README
  store.doc = fixtureMissing

  assert.equal(store.hasDocSource(), false, 'has no source document')
  assert.equal(store.docNeedsTranslation(), false, 'cannot translate when no source exists')
})

test('B3 docNeedsTranslation: returns true when translation is stale', () => {
  const { store } = createRealStore({ lang: 'zh' })
  store.docLocale = 'zh-CN'

  store.doc = {
    catalog_id: 'custom/tool',
    status: 'ready',
    resolved_locale: 'zh-CN',
    markdown: '# 较旧的中文说明',
    stale: true,
    fallback_reason: null
  }

  assert.equal(store.docNeedsTranslation(), true, 'stale translation needs re-translation')
})

test('B3 docNeedsTranslation: returns false when target translation is already ready and fresh', () => {
  const { store } = createRealStore({ lang: 'zh' })
  store.docLocale = 'zh-CN'

  store.doc = fixtureReady

  assert.equal(store.docNeedsTranslation(), false, 'already translated and fresh')
})

test('B3 Builtin vs Custom entry visibility logic', () => {
  const { store } = createRealStore({ lang: 'zh' })
  store.marketplaces = [
    { id: 'polarisagi/polaris-plugins-official', is_builtin: 1 }
  ]

  const builtinEntry = {
    id: 'polarisagi/polaris-plugins-official/weather-pro',
    marketplace_id: 'polarisagi/polaris-plugins-official'
  }
  const customEntry = {
    id: 'community-market/weather-pro',
    marketplace_id: 'community-market'
  }

  assert.equal(store.isBuiltinEntry(builtinEntry), true)
  assert.equal(store.isBuiltinEntry(customEntry), false)

  // Verify HTML condition matches behavior:
  // Builtin entries show platform maintainer message, never trigger user translation
  store.doc = {
    status: 'ready',
    resolved_locale: 'en',
    markdown: '# Weather Pro',
    fallback_reason: 'untranslated',
    source: { sha256: 'a'.repeat(64) }
  }

  const showBuiltinNotice = store.isBuiltinEntry(builtinEntry) && store.docNeedsTranslation()
  const showCustomTranslateBtn = !store.isBuiltinEntry(customEntry) && store.docNeedsTranslation()

  assert.equal(showBuiltinNotice, true, 'builtin entry shows maintainer notice when untranslated')
  assert.equal(showCustomTranslateBtn, true, 'custom entry shows translation trigger button')
})

// ============================================================================
// B4 回归测试：异步 POST 与轮询绑定详情世代（_docGen），防串页覆盖
// ============================================================================

test('B4 Generation Guard: in-flight translation POST discarded when switching entries', async () => {
  let resolvePost = null

  const { store } = createRealStore({
    fetchImpl: async (url, opts) => {
      if (opts?.method === 'POST') {
        return new Promise((resolve) => {
          resolvePost = () => resolve({
            ok: true,
            status: 202,
            json: async () => ({ task_id: 'task_from_entry_A', retry_after: 2 })
          })
        })
      }
      return { ok: true, status: 200, json: async () => ({ status: 'missing' }) }
    }
  })

  // 1. Open Entry A and start translation
  store.showEntryDetail = true
  store.entryDetail = { id: 'custom/entry-A' }
  store._activeDocKey = 'custom/entry-A::zh-CN'

  const translationPromise = store.triggerTranslation(store.entryDetail)
  assert.equal(store.docTranslating, true)

  // 2. User switches to Entry B while POST is in-flight
  store.resetDocState()
  store.entryDetail = { id: 'custom/entry-B' }
  store._activeDocKey = 'custom/entry-B::zh-CN'

  // 3. Entry A's POST completes now
  resolvePost()
  await translationPromise

  // 4. Assert: Entry A's task was NOT adopted, no polling started for Entry A
  assert.equal(store.docPollingTaskId, null, 'Old task ID must be discarded due to generation mismatch')
  assert.equal(store.docTranslating, false)
})

test('B4 Polling: dynamic retry_after delay and modal close stops polling', async () => {
  const scheduledDelays = []
  let timeoutCallback = null

  const { store } = createRealStore({
    timers: {
      onSetTimeout: (id, cb, ms) => {
        scheduledDelays.push(ms)
        timeoutCallback = cb
      },
      onClearTimeout: () => {
        timeoutCallback = null
      }
    },
    fetchImpl: async (url) => {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          status: 'running',
          retry_after: 3
        })
      }
    }
  })

  store.showEntryDetail = true
  store.entryDetail = { id: 'custom/calc' }
  store._activeDocKey = 'custom/calc::zh-CN'

  // Start polling with task that specifies retry_after: 5
  store.startPollingTask('task_123', store.entryDetail, store._docGen, 5)

  assert.equal(store.docPollingTaskId, 'task_123')
  assert.equal(scheduledDelays[0], 5000, 'first poll delayed by initial retry_after (5s)')

  // Simulate first tick
  if (timeoutCallback) {
    const cb = timeoutCallback
    timeoutCallback = null
    await cb()
  }

  // Next poll should schedule with backend returned retry_after (3s)
  assert.equal(scheduledDelays[1], 3000, 'next poll delayed by server retry_after (3s)')

  // Closing modal cleans up timer and polling task
  store.resetDocState()
  assert.equal(store.docPollingTaskId, null)
  assert.equal(store._docPollTimer, null)
})

test('B4 Polling: poll completion reloads doc only if generation still matches', async () => {
  let docReloadUrl = null
  let pollCallback = null

  const { store } = createRealStore({
    timers: {
      onSetTimeout: (id, cb, ms) => {
        pollCallback = cb
      },
      onClearTimeout: () => {}
    },
    fetchImpl: async (url) => {
      if (url.includes('/tasks/')) {
        return {
          ok: true,
          status: 200,
          json: async () => ({ status: 'ready' })
        }
      }
      if (url.includes('/documentation')) {
        docReloadUrl = url
        return {
          ok: true,
          status: 200,
          json: async () => fixtureReady
        }
      }
      return { ok: true, status: 200, json: async () => ({}) }
    }
  })

  store.showEntryDetail = true
  store.entryDetail = { id: 'custom/alpha' }
  store._activeDocKey = 'custom/alpha::zh-CN'

  store.startPollingTask('task_alpha', store.entryDetail, store._docGen, 1)

  // Switch to beta before poll completes
  store.resetDocState()
  store.entryDetail = { id: 'custom/beta' }
  store._activeDocKey = 'custom/beta::zh-CN'

  // Old poll resolves
  await pollCallback()

  // Verify documentation for custom/alpha was NOT requested
  assert.equal(docReloadUrl, null, 'Must not reload old entry documentation when generation changed')
})

// ============================================================================
// 综合测试：Session A 真实响应加载与 Markdown 渲染
// ============================================================================

test('Full Integration: loadDocumentation handles Session A documentation-ready fixture', async () => {
  const { store } = createRealStore({
    fetchImpl: async () => ({
      ok: true,
      status: 200,
      json: async () => fixtureReady
    })
  })

  store.showEntryDetail = true
  store.entryDetail = { id: fixtureReady.catalog_id }

  await store.loadDocumentation(store.entryDetail)

  assert.equal(store.docLoading, false)
  assert.equal(store.docError, null)
  assert.ok(store.doc)
  assert.equal(store.doc.status, 'ready')
  assert.equal(store.doc.catalog_id, 'polarisagi/polaris-plugins-official/weather-pro')

  const renderedHtml = renderDocMarkdown(store.doc.markdown, store.doc.source?.base_url)
  assert.match(renderedHtml, /<h1[^>]*>天气查询专业版<\/h1>/)
  assert.match(renderedHtml, /<p>获取实时天气预报与预警信息。<\/p>/)
})

test('Full Integration: loadDocumentation handles Session A documentation-missing fixture', async () => {
  const { store } = createRealStore({
    fetchImpl: async () => ({
      ok: true,
      status: 200,
      json: async () => fixtureMissing
    })
  })

  store.showEntryDetail = true
  store.entryDetail = { id: fixtureMissing.catalog_id }

  await store.loadDocumentation(store.entryDetail)

  assert.equal(store.docLoading, false)
  assert.equal(store.docError, null)
  assert.ok(store.doc)
  assert.equal(store.doc.status, 'missing')
  assert.equal(store.doc.fallback_reason, 'no_source_document')
  assert.equal(store.hasDocSource(), false)
  assert.equal(store.docNeedsTranslation(), false)
})
