import assert from 'node:assert/strict'
import test from 'node:test'
import { readHTTPError } from './http_error.mjs'

test('reads public REST message without exposing the error envelope', async () => {
  const response = new Response(JSON.stringify({ code: 'bad_input', message: '字段无效', error: 'details' }), {
    status: 400,
    headers: { 'Content-Type': 'application/json' },
  })
  const result = await readHTTPError(response)
  assert.equal(result.message, 'HTTP 400: 字段无效')
  assert.equal(result.code, 'bad_input')
  assert.equal(response.bodyUsed, true)
})

test('supports string and OpenAI nested errors', async () => {
  assert.equal((await readHTTPError(new Response('{"error":"denied"}', { status: 403 }))).message, 'HTTP 403: denied')
  assert.equal((await readHTTPError(new Response('{"error":{"message":"quota exceeded"}}', { status: 429 }))).message, 'HTTP 429: quota exceeded')
})

test('keeps old plain text and uses a stable default for empty or malformed JSON', async () => {
  assert.equal((await readHTTPError(new Response('legacy failure', { status: 502 }))).message, 'HTTP 502: legacy failure')
  assert.equal((await readHTTPError(new Response('', { status: 503 }))).message, 'HTTP 503')
  assert.equal((await readHTTPError(new Response('{"message":', { status: 400 }))).message, 'HTTP 400')
})

test('propagates abort errors from a consumed response body', async () => {
  const response = {
    status: 499,
    text: async () => { throw new DOMException('aborted', 'AbortError') },
  }
  await assert.rejects(readHTTPError(response), { name: 'AbortError' })
})
