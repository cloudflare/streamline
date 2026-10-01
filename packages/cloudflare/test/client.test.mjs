import assert from 'node:assert/strict'
import test from 'node:test'

import { createStreamline, StreamlineRequestError } from '../dist/client.js'

test('creates a single-use session and maps it to session-ID-fenced HTTP requests', async () => {
  const calls = []
  const media = createStreamline({
    baseUrl: 'https://media.example',
    fetcher: async (input, init = {}) => {
      calls.push({ url: String(input), init })
      const path = new URL(String(input)).pathname
      if (path === '/relay/prepare') return Response.json({ session_id: 'session-1' })
      if (path === '/start') return Response.json({ started: true })
      if (path === '/metrics') return Response.json({ running: true })
      if (path === '/stop') return new Response('replaced', { status: 409 })
      return new Response(null, { status: 204 })
    },
  })

  const session = await media.sessions.create()
  const started = await session.start({
    input: { type: 'webcam' },
    pipeline: [],
    output: { mode: 'websocket' },
    session_id: 'caller-controlled-session',
  })
  await session.ingest(new Uint8Array([1, 2, 3]))
  await session.annotation(new Uint8Array([4, 5, 6]))
  const metrics = await session.metrics()
  const stopped = await session.stop({ requestId: 'stop-1' })

  assert.deepEqual(started, { started: true })
  assert.deepEqual(metrics, { running: true })
  assert.deepEqual(stopped, { sessionId: 'session-1', status: 409, replaced: true })
  assert.equal(session.id, null)
  assert.deepEqual(calls.map((call) => new URL(call.url).pathname), [
    '/relay/prepare',
    '/start',
    '/ingest',
    '/api/annotation',
    '/metrics',
    '/stop',
  ])
  assert.equal(JSON.parse(String(calls[1].init.body)).session_id, 'session-1')
  assert.equal(new Headers(calls[2].init.headers).get('X-Streamline-Session-ID'), 'session-1')
  assert.equal(new Headers(calls[3].init.headers).get('X-Streamline-Session-ID'), 'session-1')
  assert.equal(new Headers(calls[4].init.headers).get('X-Streamline-Session-ID'), 'session-1')
  assert.equal(new Headers(calls[5].init.headers).get('X-Streamline-Session-ID'), 'session-1')
  assert.equal(new Headers(calls[5].init.headers).get('X-Stop-Request-ID'), 'stop-1')
  await assert.rejects(session.start({}), { message: 'A Streamline session can start only once' })
})

test('rejects failed requests from resumed sessions', async () => {
  const session = createStreamline({
    baseUrl: 'https://media.example',
    fetcher: async () => new Response('unavailable', { status: 503 }),
  }).sessions.resume('session-2')

  await assert.rejects(session.metrics(), (error) => {
    assert.ok(error instanceof StreamlineRequestError)
    assert.equal(error.status, 503)
    assert.equal(error.message, 'unavailable')
    return true
  })
})

test('calls the platform fetch with its original receiver', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = function (input) {
    assert.strictEqual(this, globalThis)
    assert.equal(String(input), 'https://media.example/metrics')
    return Promise.resolve(Response.json({ running: true }))
  }
  try {
    const session = createStreamline({ baseUrl: 'https://media.example' }).sessions.resume('session-3')
    assert.deepEqual(await session.metrics(), { running: true })
  } finally {
    globalThis.fetch = originalFetch
  }
})
