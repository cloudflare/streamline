import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MAX_ANNOTATION_BYTES,
  MAX_INGEST_BYTES,
  MAX_START_BYTES,
  StreamlineSessionDO,
} from '../dist/index.js'

const origin = 'https://media.example'
const ownerHeaders = { 'X-Streamline-Principal': 'owner' }

class TestSession extends StreamlineSessionDO {
  applicationRequests = []
  interceptAllApplicationRoutes = false

  async resolveStartConfig(_request, body) {
    return { body, maxSessionSeconds: 60 }
  }

  async handleApplicationRequest(request) {
    this.applicationRequests.push(new URL(request.url).pathname)
    if (this.interceptAllApplicationRoutes || new URL(request.url).pathname === '/application') {
      return new Response('application')
    }
    return undefined
  }
}

function createStorage(initial = []) {
  const values = new Map(initial)
  return {
    get: async (key) => values.get(key),
    put: async (key, value) => values.set(key, value),
    delete: async (key) => values.delete(key),
    transaction: async (operation) => operation({
      put: async (key, value) => values.set(key, value),
    }),
  }
}

function createSession(containerFetch, initialStorage) {
  const storage = createStorage(initialStorage)
  const ctx = {
    storage,
    getWebSockets: () => [],
    waitUntil: () => {},
  }
  const session = new TestSession(ctx, {
    ALLOWED_ORIGINS: origin,
    MAX_SESSION_DURATION_SECONDS: '60',
  })
  session.containerFetch = containerFetch
  return { session, storage }
}

function request(path, init = {}) {
  return new Request(`${origin}${path}`, init)
}

async function prepare(session, principal = 'owner') {
  const response = await session.fetch(request('/relay/prepare', {
    method: 'POST',
    headers: { 'X-Streamline-Principal': principal },
  }))
  assert.equal(response.status, 200)
  return response.json()
}

function startRequest(sessionId, principal = 'owner') {
  return request('/start', {
    method: 'POST',
    headers: { ...ownerHeaders, 'X-Streamline-Principal': principal, 'Content-Type': 'application/json' },
    body: JSON.stringify({
      session_id: sessionId,
      input: { type: 'webcam' },
      pipeline: [],
      output: { mode: 'websocket' },
    }),
  })
}

test('commits a prepared session ID and clears it after stop', async () => {
  const forwarded = []
  const { session, storage } = createSession(async (request) => {
    forwarded.push(request)
    return new Response(null, { status: request.url.endsWith('/start') ? 201 : 204 })
  })
  const { session_id: sessionId } = await prepare(session)

  assert.equal((await session.fetch(startRequest(sessionId))).status, 201)
  assert.equal((await storage.get('running-session-id')), sessionId)
  const forwardedStart = JSON.parse(await forwarded[0].text())
  assert.equal(forwardedStart.output.relay.url, 'wss://media.example/relay/publish')
  assert.match(forwardedStart.output.relay.token, /^[A-Za-z0-9_-]{43}$/)

  const stopped = await session.fetch(request('/stop', {
    method: 'POST',
    headers: { ...ownerHeaders, 'X-Streamline-Session-ID': sessionId },
  }))
  assert.equal(stopped.status, 204)
  assert.equal(await storage.get('relay-session'), undefined)
  assert.equal(await storage.get('running-session-id'), undefined)
  assert.deepEqual(forwarded.map((request) => new URL(request.url).pathname), ['/start', '/stop'])
})

test('cleans up a failed start and an expired running session', async () => {
  const forwarded = []
  const { session, storage } = createSession(async (request) => {
    const path = new URL(request.url).pathname
    forwarded.push(path)
    return new Response(null, { status: path === '/start' ? 500 : 204 })
  })
  const first = await prepare(session)
  assert.equal((await session.fetch(startRequest(first.session_id))).status, 500)
  assert.equal(await storage.get('relay-session'), undefined)
  assert.equal(await storage.get('running-session-id'), undefined)

  session.containerFetch = async (request) => {
    const path = new URL(request.url).pathname
    forwarded.push(path)
    return new Response(null, { status: 204 })
  }
  const second = await prepare(session)
  assert.equal((await session.fetch(startRequest(second.session_id))).status, 204)
  session.relaySession.expiresAt = Date.now() - 1
  await session.expireRelaySession(second.session_id)
  assert.equal(await storage.get('relay-session'), undefined)
  assert.equal(await storage.get('running-session-id'), undefined)
  assert.deepEqual(forwarded, ['/start', '/stop', '/start', '/stop'])
})

test('migrates and stops a persisted generation-fenced session', async () => {
  const forwarded = []
  const { session, storage } = createSession(async (request) => {
    forwarded.push(request)
    return new Response(null, { status: 204 })
  }, [
    ['relay-session', {
      generation: 'legacy-generation',
      principal: 'owner',
      expiresAt: Date.now() + 60_000,
    }],
    ['running-generation', 'legacy-generation'],
  ])

  const stopped = await session.fetch(request('/stop', {
    method: 'POST',
    headers: { ...ownerHeaders, 'X-Streamline-Session-ID': 'legacy-generation' },
  }))

  assert.equal(stopped.status, 204)
  assert.equal(forwarded[0].headers.get('X-Relay-Generation'), 'legacy-generation')
  assert.equal(forwarded[0].headers.has('X-Streamline-Session-ID'), false)
  assert.equal(await storage.get('relay-session'), undefined)
  assert.equal(await storage.get('running-generation'), undefined)
  assert.equal(await storage.get('running-session-id'), undefined)
})

test('rejects another principal for every owner-scoped route', async () => {
  const { session } = createSession()
  const { session_id: sessionId } = await prepare(session)
  const stranger = 'stranger'
  const cases = [
    startRequest(sessionId, stranger),
    request('/ingest', {
      method: 'POST',
      headers: { 'X-Streamline-Principal': stranger, 'X-Streamline-Session-ID': sessionId, 'Content-Type': 'application/octet-stream' },
      body: new Uint8Array([1]),
    }),
    request('/api/annotation', {
      method: 'PUT',
      headers: { 'X-Streamline-Principal': stranger, 'X-Streamline-Session-ID': sessionId, 'Content-Type': 'image/png' },
      body: new Uint8Array([1]),
    }),
    request('/metrics', { headers: { 'X-Streamline-Principal': stranger, 'X-Streamline-Session-ID': sessionId } }),
    request('/stop', { method: 'POST', headers: { 'X-Streamline-Principal': stranger, 'X-Streamline-Session-ID': sessionId } }),
    request(`/relay/view?session_id=${sessionId}`, {
      headers: { 'X-Streamline-Principal': stranger, Origin: origin, Upgrade: 'websocket' },
    }),
  ]

  for (const request of cases) assert.equal((await session.fetch(request)).status, 403)
})

test('session-ID-fences requests before they reach the container', async () => {
  const forwarded = []
  const { session } = createSession(async (request) => {
    forwarded.push(new URL(request.url).pathname)
    return new Response(null, { status: 204 })
  })
  const { session_id: sessionId } = await prepare(session)

  assert.equal((await session.fetch(startRequest('stale-session-id'))).status, 409)
  assert.equal((await session.fetch(request('/ingest', {
    method: 'POST',
    headers: { ...ownerHeaders, 'X-Streamline-Session-ID': sessionId, 'Content-Type': 'application/octet-stream' },
    body: new Uint8Array([1]),
  }))).status, 204)
  assert.deepEqual(forwarded, ['/ingest'])
})

test('enforces body limits from bytes rather than Content-Length', async () => {
  const { session } = createSession()
  const cases = [
    { path: '/start', method: 'POST', maximum: MAX_START_BYTES, contentType: 'application/json', body: (bytes) => JSON.stringify({ data: 'x'.repeat(bytes - 11) }) },
    { path: '/ingest', method: 'POST', maximum: MAX_INGEST_BYTES, contentType: 'application/octet-stream', body: (bytes) => new Uint8Array(bytes) },
    { path: '/api/annotation', method: 'PUT', maximum: MAX_ANNOTATION_BYTES, contentType: 'image/png', body: (bytes) => new Uint8Array(bytes) },
  ]

  for (const { path, method, maximum, contentType, body } of cases) {
    const exact = await session.fetch(request(path, {
      method,
      headers: { 'Content-Type': contentType },
      body: body(maximum),
    }))
    assert.equal(exact.status, 401)
    const oversized = await session.fetch(request(path, {
      method,
      headers: { 'Content-Type': contentType, 'Content-Length': '1' },
      body: body(maximum + 1),
    }))
    assert.equal(oversized.status, 413)
  }
})

test('reserves SDK routes before application routes', async () => {
  const { session } = createSession()
  session.interceptAllApplicationRoutes = true
  const cases = [
    ['/relay/prepare', 405],
    ['/relay/publish', 400],
    ['/relay/view', 400],
    ['/start', 405],
    ['/ingest', 405],
    ['/api/annotation', 405],
    ['/metrics', 401],
    ['/stop', 405],
  ]

  for (const [path, status] of cases) {
    const response = await session.fetch(request(path))
    assert.equal(response.status, status)
  }
  const applicationResponse = await session.fetch(request('/application'))
  assert.equal(await applicationResponse.text(), 'application')
  assert.deepEqual(session.applicationRequests, ['/application'])
})
