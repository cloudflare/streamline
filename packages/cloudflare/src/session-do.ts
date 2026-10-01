import { Container, type StopParams } from '@cloudflare/containers'
import { normalizeEmptyRequestBody, readBodyWithLimit } from './request-body.js'

export interface StreamlineSessionEnvironment {
  ALLOWED_ORIGINS: string
  MAX_SESSION_DURATION_SECONDS: string
}

export interface ResolvedSessionStart {
  body: Record<string, unknown>
  maxSessionSeconds: number
}

export interface StreamlineRelaySession {
  sessionId: string
  principal: string
  prepareExpiresAt?: number
  startExpiresAt?: number
  expiresAt?: number
  publisherTokenHash?: string
  legacyContainer?: boolean
}

interface LegacyRelaySession extends Omit<StreamlineRelaySession, 'sessionId' | 'legacyContainer'> {
  generation: string
}

class RelayCleanupPendingError extends Error {}

export const MAX_START_BYTES = 64 * 1024
export const MAX_INGEST_BYTES = 1024 * 1024
export const MAX_ANNOTATION_BYTES = 5 * 1024 * 1024
export const MAX_PUBLISHER_MESSAGE_BYTES = 2 * 1024 * 1024

const RELAY_PREPARE_TTL_MS = 15 * 60 * 1000
const RELAY_START_TTL_MS = 2 * 60 * 1000
const RELAY_EXPIRY_GRACE_MS = 5 * 60 * 1000
const SLOW_INGEST_MS = 2_000
const CONTAINER_STARTUP_TIMEOUT_MS = 25_000
const PRINCIPAL_HEADER = 'X-Streamline-Principal'
const RUNNING_SESSION_ID_KEY = 'running-session-id'
const LEGACY_RUNNING_GENERATION_KEY = 'running-generation'
const SESSION_ID_HEADER = 'X-Streamline-Session-ID'
const LEGACY_GENERATION_HEADER = 'X-Relay-Generation'

function randomCapability(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32))
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '')
}

async function hashCapability(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

function constantTimeEqual(left: string, right: string): boolean {
  const length = Math.max(left.length, right.length)
  let difference = left.length ^ right.length
  for (let index = 0; index < length; index++) {
    difference |= (left.charCodeAt(index) || 0) ^ (right.charCodeAt(index) || 0)
  }
  return difference === 0
}

async function fetchIngestWithDiagnostics(
  request: Request,
  bytes: number,
  fetcher: () => Promise<Response>,
): Promise<Response> {
  const rawRequestId = request.headers.get('X-Ingest-Request-ID')
  const requestId = rawRequestId && /^[A-Za-z0-9_-]{1,64}$/.test(rawRequestId) ? rawRequestId : null
  const startedAt = Date.now()
  try {
    const response = await fetcher()
    const durationMs = Date.now() - startedAt
    if (!response.ok || durationMs >= SLOW_INGEST_MS) {
      console.warn('Ingest proxy request', { layer: 'durable-object', requestId, bytes, durationMs, status: response.status })
    }
    return response
  } catch (error) {
    console.error('Ingest proxy request failed', {
      layer: 'durable-object',
      requestId,
      bytes,
      durationMs: Date.now() - startedAt,
      error: error instanceof Error ? error.message : String(error),
    })
    throw error
  }
}

// Applications implement policy and optional application-owned DO routes while this class owns session coordination.
export abstract class StreamlineSessionDO<Env extends StreamlineSessionEnvironment> extends Container<Env> {
  defaultPort = 8080
  sleepAfter = '15m'
  enableInternet = true
  interceptHttps = true
  entrypoint = ['/usr/local/bin/streamline-entrypoint', '/usr/local/bin/streamline']
  envVars = {
    ALLOWED_ORIGINS: this.env.ALLOWED_ORIGINS,
    MAX_SESSION_DURATION_SECONDS: this.env.MAX_SESSION_DURATION_SECONDS,
  }
  private relaySession: StreamlineRelaySession | undefined
  private runningSessionId: string | undefined
  private controlTail: Promise<void> = Promise.resolve()

  protected abstract resolveStartConfig(
    request: Request,
    body: Record<string, unknown>,
  ): Promise<ResolvedSessionStart>

  protected handleStartConfigError(_error: unknown): Response | undefined {
    return undefined
  }

  protected ensurePublisherAccess(): void {}

  protected getRelayPublisherUrl(request: Request): URL {
    const relayUrl = new URL('/relay/publish', request.url)
    relayUrl.protocol = relayUrl.protocol === 'https:' ? 'wss:' : 'ws:'
    return relayUrl
  }

  protected async onRelaySessionCleared(_session: StreamlineRelaySession): Promise<void> {}

  protected async handleApplicationRequest(_request: Request): Promise<Response | undefined> {
    return undefined
  }

  onStop({ exitCode, reason }: StopParams) {
    console.warn('Media container stopped', { exitCode, reason })
    this.ctx.waitUntil(this.cleanupStoppedContainer())
  }

  async onActivityExpired() {
    await this.withControlLock(async () => {
      try {
        const session = await this.getRelaySessionLocked()
        if (session?.expiresAt) {
          this.renewActivityTimeout()
          return
        }
      } catch (error) {
        if (!(error instanceof RelayCleanupPendingError)) throw error
      }
      await this.destroy()
    })
  }

  async fetch(request: Request): Promise<Response> {
    try {
      return await this.handleFetch(request)
    } catch (error) {
      if (error instanceof RelayCleanupPendingError) {
        return new Response('Relay cleanup is pending', { status: 503 })
      }
      throw error
    }
  }

  async webSocketMessage(socket: WebSocket, message: string | ArrayBuffer) {
    const attachment = socket.deserializeAttachment() as { role?: string; sessionId?: string } | null
    if (attachment?.role === 'viewer') {
      socket.close(1008, 'viewer messages are not accepted')
      return
    }
    if (attachment?.role !== 'publisher') return
    const outputEnded = message === '{"type":"eos"}'
    if (typeof message === 'string' && !outputEnded) {
      socket.close(1003, 'unsupported publisher message')
      return
    }
    if (message instanceof ArrayBuffer && message.byteLength > MAX_PUBLISHER_MESSAGE_BYTES) {
      socket.close(1009, 'publisher message too large')
      return
    }

    let session: StreamlineRelaySession | undefined
    try {
      session = await this.withControlLock(() => this.getRelaySessionLocked())
    } catch (error) {
      if (!(error instanceof RelayCleanupPendingError)) throw error
      socket.close(1012, 'relay cleanup pending')
      return
    }
    if (!session || attachment.sessionId !== session.sessionId) return

    const viewers = this.ctx.getWebSockets('viewer').filter((viewer) => {
      const viewerAttachment = viewer.deserializeAttachment() as { sessionId?: string } | null
      return viewerAttachment?.sessionId === session.sessionId
    })
    if (viewers.length === 0) return

    this.renewActivityTimeout()
    for (const viewer of viewers) {
      try {
        viewer.send(message)
        if (outputEnded) viewer.close(1000, 'output ended')
      } catch (error) {
        console.warn('Relay viewer send failed', { error: String(error) })
        viewer.close(1011, 'relay send failed')
      }
    }
  }

  webSocketError(socket: WebSocket) {
    const attachment = socket.deserializeAttachment() as { role?: string; sessionId?: string } | null
    console.warn('Relay WebSocket error', attachment)
    socket.close(1011, 'relay socket error')
  }

  webSocketClose(socket: WebSocket, code: number, reason: string, wasClean: boolean) {
    const attachment = socket.deserializeAttachment() as { role?: string; sessionId?: string } | null
    console.warn('Relay WebSocket closed', { role: attachment?.role, code, reason, wasClean })

    if (attachment?.role !== 'viewer') return
    const hasViewer = this.ctx.getWebSockets('viewer').some((viewer) => {
      if (viewer === socket || viewer.readyState !== 1) return false
      const viewerAttachment = viewer.deserializeAttachment() as { sessionId?: string } | null
      return viewerAttachment?.sessionId === attachment.sessionId
    })
    if (hasViewer) return

    for (const publisher of this.ctx.getWebSockets('publisher')) {
      const publisherAttachment = publisher.deserializeAttachment() as { sessionId?: string } | null
      if (publisherAttachment?.sessionId === attachment.sessionId) {
        publisher.close(1012, 'viewer disconnected')
      }
    }
  }

  // This public method is a scheduled callback invoked by the Containers SDK.
  async expireRelaySession(sessionId: string): Promise<void> {
    await this.withControlLock(() => this.expireRelaySessionLocked(sessionId))
  }

  protected async withControlLock<T>(operation: () => Promise<T>): Promise<T> {
    const previous = this.controlTail
    let release: () => void
    this.controlTail = new Promise((resolve) => {
      release = resolve
    })
    await previous
    try {
      return await operation()
    } finally {
      release!()
    }
  }

  private async handleFetch(request: Request): Promise<Response> {
    const url = new URL(request.url)
    if (url.pathname === '/relay/prepare') {
      return this.withControlLock(async () => {
        if (request.method !== 'POST') return new Response('Method not allowed', { status: 405 })
        const bodylessRequest = await normalizeEmptyRequestBody(request)
        if (!bodylessRequest) return new Response('Relay preparation does not accept a body', { status: 400 })
        request = bodylessRequest
        const principal = request.headers.get(PRINCIPAL_HEADER)
        if (!principal) return new Response('Unauthorized', { status: 401 })
        const previousSession = await this.getRelaySessionLocked(true)
        if (previousSession && previousSession.principal !== principal) {
          return new Response('Another user has an active session', { status: 409 })
        }
        if (previousSession && !previousSession.prepareExpiresAt) {
          const stopUrl = new URL('/stop', request.url)
          const stopResponse = await super.fetch(new Request(stopUrl, {
            method: 'POST',
            headers: this.stopHeaders(previousSession),
          }))
          if (!stopResponse.ok) return new Response('Could not stop the previous relay session', { status: 502 })
          await this.clearRunningSessionIdLocked(previousSession.sessionId)
        }
        await this.storeRelaySessionLocked({
          sessionId: crypto.randomUUID(),
          principal,
          prepareExpiresAt: Date.now() + RELAY_PREPARE_TTL_MS,
        })
        this.closeRelaySockets('relay session replaced')
        return Response.json({ session_id: this.relaySession!.sessionId })
      })
    }

    if (url.pathname === '/relay/publish' || url.pathname === '/relay/view') {
      return this.acceptRelayWebSocket(request, url.pathname === '/relay/publish' ? 'publisher' : 'viewer')
    }

    if (url.pathname === '/start') {
      if (request.method !== 'POST') return new Response('Method not allowed', { status: 405 })
      const bodyBytes = await readBodyWithLimit(request, MAX_START_BYTES)
      if (!bodyBytes) return new Response('Start request exceeds the 64KiB limit', { status: 413 })
      let body: Record<string, unknown>
      try {
        const parsed: unknown = JSON.parse(new TextDecoder().decode(bodyBytes))
        if (!isRecord(parsed)) throw new Error('expected an object')
        body = parsed
      } catch {
        return new Response('Start request must contain valid JSON', { status: 400 })
      }

      return this.withControlLock(async () => {
        const sessionId = body.session_id
        const session = await this.getRelaySessionLocked()
        const ownershipError = this.sessionOwnershipError(request, session)
        if (ownershipError) return ownershipError
        if (!session || typeof sessionId !== 'string' || sessionId !== session.sessionId) {
          return new Response('Relay session is no longer current', { status: 409 })
        }
        if (session.startExpiresAt || session.expiresAt) {
          return new Response('Relay session is already committed', { status: 409 })
        }

        let start: ResolvedSessionStart
        try {
          start = await this.resolveStartConfig(request, body)
        } catch (error) {
          const response = this.handleStartConfigError(error)
          if (response) return response
          throw error
        }
        if (!Number.isInteger(start.maxSessionSeconds) || start.maxSessionSeconds <= 0) {
          return new Response('Media session duration is invalid', { status: 503 })
        }
        body = start.body

        const startupError = await this.waitForContainerStartup(request.signal)
        if (startupError) return startupError
        const relayUrl = this.getRelayPublisherUrl(request)
        let publisherToken: string | undefined
        if ((body.output as Record<string, unknown>).mode === 'websocket') {
          try {
            this.ensurePublisherAccess()
          } catch (error) {
            const response = this.handleStartConfigError(error)
            if (response) return response
            throw error
          }
          publisherToken = randomCapability()
          body = injectPreviewRelay(body, { url: relayUrl.toString(), token: publisherToken })
        }
        const startingSession: StreamlineRelaySession = {
          ...session,
          publisherTokenHash: publisherToken ? await hashCapability(publisherToken) : undefined,
          prepareExpiresAt: undefined,
          startExpiresAt: Date.now() + RELAY_START_TTL_MS,
        }
        await this.storeRelaySessionLocked(startingSession)
        const headers = new Headers(request.headers)
        headers.delete('Content-Length')
        try {
          const response = await super.fetch(new Request(request, { headers, body: JSON.stringify(body) }))
          if (response.ok) await this.commitRunningRelaySessionLocked(startingSession, start.maxSessionSeconds)
          else await this.stopAfterFailedStartLocked(startingSession)
          return response
        } catch (error) {
          await this.stopAfterFailedStartLocked(startingSession)
          throw error
        }
      })
    }

    if (url.pathname === '/ingest') {
      if (request.method !== 'POST') return new Response('Method not allowed', { status: 405 })
      if (request.headers.get('Content-Type')?.split(';', 1)[0].trim() !== 'application/octet-stream') {
        return new Response('Content-Type must be application/octet-stream', { status: 415 })
      }
      const body = await readBodyWithLimit(request, MAX_INGEST_BYTES)
      if (!body) return new Response('Ingest request exceeds the 1MiB limit', { status: 413 })
      request = new Request(request, { body })
      const sessionIdError = await this.withControlLock(async () => {
        const session = await this.getRelaySessionLocked()
        const ownershipError = this.sessionOwnershipError(request, session)
        if (ownershipError) return ownershipError
        if (!this.requestMatchesSession(request, session)) {
          return new Response('Relay session is no longer current', { status: 409 })
        }
        return session
      })
      if (sessionIdError instanceof Response) return sessionIdError
      return fetchIngestWithDiagnostics(request, body.byteLength, () => super.fetch(
        this.forwardRequestToContainer(request, sessionIdError),
      ))
    }

    if (url.pathname === '/api/annotation') {
      if (request.method !== 'PUT') return new Response('Method not allowed', { status: 405 })
      if (request.headers.get('Content-Type')?.split(';', 1)[0].trim() !== 'image/png') {
        return new Response('Content-Type must be image/png', { status: 415 })
      }
      const body = await readBodyWithLimit(request, MAX_ANNOTATION_BYTES)
      if (!body) return new Response('Annotation image exceeds the 5MB limit', { status: 413 })
      request = new Request(request, { body })
      const sessionIdError = await this.withControlLock(async () => {
        const session = await this.getRelaySessionLocked()
        const ownershipError = this.sessionOwnershipError(request, session)
        if (ownershipError) return ownershipError
        if (!this.requestMatchesSession(request, session)) {
          return new Response('Relay session is no longer current', { status: 409 })
        }
        return session
      })
      if (sessionIdError instanceof Response) return sessionIdError
      return super.fetch(this.forwardRequestToContainer(request, sessionIdError))
    }

    if (url.pathname === '/metrics') {
      if (request.method !== 'GET') return new Response('Method not allowed', { status: 405 })
      const sessionIdError = await this.withControlLock(async () => {
        const session = await this.getRelaySessionLocked()
        const ownershipError = this.sessionOwnershipError(request, session)
        if (ownershipError) return ownershipError
        if (!this.requestMatchesSession(request, session)) {
          return new Response('Relay session is no longer current', { status: 409 })
        }
        return session
      })
      if (sessionIdError instanceof Response) return sessionIdError
      return super.fetch(this.forwardRequestToContainer(request, sessionIdError))
    }

    if (url.pathname === '/stop') {
      return this.withControlLock(async () => {
        if (request.method !== 'POST') return new Response('Method not allowed', { status: 405 })
        const bodylessRequest = await normalizeEmptyRequestBody(request)
        if (!bodylessRequest) return new Response('Stop does not accept a body', { status: 400 })
        request = bodylessRequest
        const session = await this.getRelaySessionLocked(true)
        const ownershipError = this.sessionOwnershipError(request, session)
        if (ownershipError) return ownershipError
        console.info('Relay stop request', {
          layer: 'durable-object',
          requestId: request.headers.get('X-Stop-Request-ID'),
          hasRequestedSessionId: request.headers.has('X-Streamline-Session-ID'),
          hasActiveSessionId: Boolean(session),
        })
        if (!session && request.headers.has(SESSION_ID_HEADER)) return new Response(null, { status: 204 })
        if (session && !this.requestMatchesSession(request, session)) {
          return new Response('Relay session is no longer current', { status: 409 })
        }
        if (session?.prepareExpiresAt) {
          await this.clearRelaySessionLocked(session.sessionId, 'relay preparation stopped')
          return new Response(null, { status: 204 })
        }
        const response = await super.fetch(this.forwardRequestToContainer(request, session))
        if (response.ok) {
          await this.clearRunningSessionIdLocked(session?.sessionId)
          await this.clearRelaySessionLocked(session?.sessionId, 'relay session stopped')
        }
        return response
      })
    }

    const applicationResponse = await this.handleApplicationRequest(request)
    if (applicationResponse) return applicationResponse
    return super.fetch(request)
  }

  private async acceptRelayWebSocket(request: Request, role: 'publisher' | 'viewer'): Promise<Response> {
    return this.withControlLock(() => this.acceptRelayWebSocketLocked(request, role))
  }

  private async acceptRelayWebSocketLocked(request: Request, role: 'publisher' | 'viewer'): Promise<Response> {
    if (request.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
      return new Response('Expected Upgrade: websocket', { status: 400 })
    }
    const session = await this.getRelaySessionLocked()
    if (role === 'publisher') {
      const authorization = request.headers.get('Authorization')
      const token = authorization?.startsWith('Bearer ') ? authorization.slice('Bearer '.length) : ''
      if (!session?.publisherTokenHash || !token
        || !constantTimeEqual(await hashCapability(token), session.publisherTokenHash)) {
        return new Response('Unauthorized', { status: 401 })
      }
    } else {
      const ownershipError = this.sessionOwnershipError(request, session)
      if (ownershipError) return ownershipError
      if (request.headers.get('Origin') !== new URL(request.url).origin) {
        return new Response('WebSocket origin not allowed', { status: 403 })
      }
    }

    const sessionId = role === 'viewer' ? new URL(request.url).searchParams.get('session_id') : session?.sessionId
    if (!session || sessionId !== session.sessionId) return new Response('Relay session is no longer current', { status: 409 })
    if (role === 'publisher') {
      const hasViewer = this.ctx.getWebSockets('viewer').some((viewer) => {
        const viewerAttachment = viewer.deserializeAttachment() as { sessionId?: string } | null
        return viewer.readyState === 1 && viewerAttachment?.sessionId === session.sessionId
      })
      if (!hasViewer) return new Response('Relay viewer is not connected', { status: 409 })
    }

    const pair = new WebSocketPair()
    const [client, server] = Object.values(pair)
    server.serializeAttachment({ role, sessionId })
    for (const existing of this.ctx.getWebSockets(role)) existing.close(1012, `${role} replaced`)
    this.ctx.acceptWebSocket(server, [role])
    return new Response(null, { status: 101, webSocket: client })
  }

  private async loadRelaySession(): Promise<StreamlineRelaySession | undefined> {
    if (this.relaySession) return this.relaySession
    const stored = await this.ctx.storage.get<StreamlineRelaySession | LegacyRelaySession>('relay-session')
    if (!stored) return undefined
    if ('sessionId' in stored) {
      this.relaySession = stored
      return this.relaySession
    }

    // Existing Durable Objects can retain an active pre-session-ID container across a deploy.
    // Preserve its value as the session ID and use the old header only until it is stopped.
    const { generation, ...legacySession } = stored
    const migrated: StreamlineRelaySession = {
      ...legacySession,
      sessionId: generation,
      legacyContainer: true,
    }
    await this.ctx.storage.put('relay-session', migrated)
    const runningGeneration = await this.ctx.storage.get<string>(LEGACY_RUNNING_GENERATION_KEY)
    if (runningGeneration === generation) {
      await this.ctx.storage.put(RUNNING_SESSION_ID_KEY, generation)
    }
    await this.ctx.storage.delete(LEGACY_RUNNING_GENERATION_KEY)
    this.relaySession = migrated
    return this.relaySession
  }

  private async getRelaySessionLocked(includeExpired = false): Promise<StreamlineRelaySession | undefined> {
    const session = await this.loadRelaySession()
    const expiresAt = session?.expiresAt ?? session?.startExpiresAt ?? session?.prepareExpiresAt
    if (!session || !expiresAt || expiresAt > Date.now()) return session
    await this.expireRelaySessionLocked(session.sessionId)
    const retained = await this.loadRelaySession()
    if (retained?.sessionId === session.sessionId) {
      if (includeExpired) return retained
      throw new RelayCleanupPendingError()
    }
    return undefined
  }

  private async expireRelaySessionLocked(sessionId: string): Promise<void> {
    const session = await this.loadRelaySession()
    if (!session || session.sessionId !== sessionId) return
    const expiresAt = session.expiresAt ?? session.startExpiresAt ?? session.prepareExpiresAt
    if (!expiresAt) return
    if (expiresAt > Date.now()) {
      await this.scheduleRelayExpiry(session)
      return
    }
    if (session.expiresAt || session.startExpiresAt) {
      const stopUrl = new URL('/stop', this.env.ALLOWED_ORIGINS)
      try {
        const response = await super.fetch(new Request(stopUrl, {
          method: 'POST',
          headers: this.stopHeaders(session),
        }))
        if (!response.ok) {
          console.error('Could not stop expired media session', { status: response.status })
          await this.scheduleRelayExpiryRetry(sessionId)
          return
        }
      } catch (error) {
        console.error('Could not stop expired media session', {
          error: error instanceof Error ? error.message : String(error),
        })
        await this.scheduleRelayExpiryRetry(sessionId)
        return
      }
    }
    await this.clearRunningSessionIdLocked(sessionId)
    await this.clearRelaySessionLocked(sessionId, 'relay session expired')
  }

  private async storeRelaySessionLocked(session: StreamlineRelaySession): Promise<void> {
    await this.ctx.storage.put('relay-session', session)
    this.relaySession = session
    try {
      await this.scheduleRelayExpiry(session)
    } catch (error) {
      this.relaySession = undefined
      await this.ctx.storage.delete('relay-session')
      this.deleteSchedules('expireRelaySession')
      throw error
    }
  }

  private async scheduleRelayExpiry(session: StreamlineRelaySession): Promise<void> {
    const expiresAt = session.expiresAt ?? session.startExpiresAt ?? session.prepareExpiresAt
    this.deleteSchedules('expireRelaySession')
    if (expiresAt) await this.schedule(new Date(expiresAt), 'expireRelaySession', session.sessionId)
  }

  private async waitForContainerStartup(signal: AbortSignal): Promise<Response | null> {
    const timeoutSignal = AbortSignal.timeout(CONTAINER_STARTUP_TIMEOUT_MS)
    const startupSignal = AbortSignal.any([signal, timeoutSignal])
    let rejectCancellation: (reason?: unknown) => void = () => {}
    const cancellation = new Promise<never>((_, reject) => {
      rejectCancellation = reject
    })
    const cancelWait = () => rejectCancellation(
      startupSignal.reason instanceof Error ? startupSignal.reason : new Error('Container startup cancelled'),
    )
    if (startupSignal.aborted) cancelWait()
    else startupSignal.addEventListener('abort', cancelWait, { once: true })

    try {
      const readiness = this.startAndWaitForPorts({
        ports: this.defaultPort,
        cancellationOptions: {
          abort: startupSignal,
          instanceGetTimeoutMS: CONTAINER_STARTUP_TIMEOUT_MS,
          portReadyTimeoutMS: CONTAINER_STARTUP_TIMEOUT_MS,
        },
      })
      this.ctx.waitUntil(readiness.catch(() => {}))
      await Promise.race([readiness, cancellation])
      if (signal.aborted) return new Response('Request cancelled', { status: 499 })
      return null
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error)
      const normalizedMessage = message.toLowerCase()
      console.error('Media container did not become ready', { error: message })
      if (signal.aborted) return new Response('Request cancelled', { status: 499 })
      if (!timeoutSignal.aborted && normalizedMessage.includes('you are requesting too many containers per second')) {
        return new Response(message, { status: 429, headers: { 'Retry-After': '2' } })
      }
      if (!timeoutSignal.aborted && !normalizedMessage.includes('there is no container instance that can be provided')) {
        return new Response(`Failed to start container: ${message}`, { status: 500 })
      }
      return new Response('Media container is still starting; try again.', {
        status: 503,
        headers: { 'Retry-After': '2' },
      })
    } finally {
      startupSignal.removeEventListener('abort', cancelWait)
    }
  }

  private async scheduleRelayExpiryRetry(sessionId: string): Promise<void> {
    this.deleteSchedules('expireRelaySession')
    await this.schedule(60, 'expireRelaySession', sessionId)
  }

  private async clearRelaySessionLocked(sessionId: string | undefined, reason: string): Promise<void> {
    const session = await this.loadRelaySession()
    if (!session || !sessionId || session.sessionId !== sessionId) return
    this.relaySession = undefined
    await this.ctx.storage.delete('relay-session')
    this.deleteSchedules('expireRelaySession')
    this.closeRelaySockets(reason, sessionId)
    await this.onRelaySessionCleared(session)
  }

  private async commitRunningRelaySessionLocked(session: StreamlineRelaySession, maxSessionSeconds: number): Promise<void> {
    const runningSession: StreamlineRelaySession = {
      ...session,
      startExpiresAt: undefined,
      expiresAt: Date.now() + maxSessionSeconds * 1000 + RELAY_EXPIRY_GRACE_MS,
    }
    await this.ctx.storage.transaction(async (transaction) => {
      await transaction.put('relay-session', runningSession)
      await transaction.put(RUNNING_SESSION_ID_KEY, session.sessionId)
    })
    this.relaySession = runningSession
    this.runningSessionId = session.sessionId
    await this.scheduleRelayExpiry(runningSession)
  }

  private async clearRunningSessionIdLocked(sessionId: string | undefined): Promise<void> {
    if (!sessionId) return
    const runningSessionId = this.runningSessionId ?? await this.ctx.storage.get<string>(RUNNING_SESSION_ID_KEY)
    if (runningSessionId !== sessionId) return
    this.runningSessionId = undefined
    await this.ctx.storage.delete(RUNNING_SESSION_ID_KEY)
  }

  private async stopAfterFailedStartLocked(session: StreamlineRelaySession): Promise<void> {
    const stopUrl = new URL('/stop', this.env.ALLOWED_ORIGINS)
    try {
      const response = await super.fetch(new Request(stopUrl, {
        method: 'POST',
        headers: this.stopHeaders(session),
      }))
      if (response.ok) {
        await this.clearRunningSessionIdLocked(session.sessionId)
        await this.clearRelaySessionLocked(session.sessionId, 'media start failed')
        return
      }
      console.error('Could not stop media after failed start', { status: response.status })
    } catch (error) {
      console.error('Could not stop media after failed start', {
        error: error instanceof Error ? error.message : String(error),
      })
    }
    const cleanupSession = {
      ...session,
      prepareExpiresAt: undefined,
      startExpiresAt: undefined,
      expiresAt: Date.now(),
    }
    this.relaySession = cleanupSession
    await this.ctx.storage.put('relay-session', cleanupSession)
    await this.scheduleRelayExpiryRetry(session.sessionId)
  }

  private async cleanupStoppedContainer(): Promise<void> {
    const sessionId = this.runningSessionId ?? await this.ctx.storage.get<string>(RUNNING_SESSION_ID_KEY)
    if (!sessionId) return
    await this.withControlLock(async () => {
      await this.clearRunningSessionIdLocked(sessionId)
      await this.clearRelaySessionLocked(sessionId, 'media container stopped')
    })
  }

  private sessionOwnershipError(request: Request, session: StreamlineRelaySession | undefined): Response | null {
    const principal = request.headers.get(PRINCIPAL_HEADER)
    if (!principal) return new Response('Unauthorized', { status: 401 })
    if (session && principal !== session.principal) return new Response('Relay session belongs to another user', { status: 403 })
    return null
  }

  private requestMatchesSession(request: Request, session: StreamlineRelaySession | undefined): boolean {
    if (!session) return false
    if (request.headers.get(SESSION_ID_HEADER) === session.sessionId) return true
    return session.legacyContainer === true && request.headers.get(LEGACY_GENERATION_HEADER) === session.sessionId
  }

  private forwardRequestToContainer(request: Request, session: StreamlineRelaySession | undefined): Request {
    if (!session?.legacyContainer) return request
    const headers = new Headers(request.headers)
    headers.delete(SESSION_ID_HEADER)
    headers.set(LEGACY_GENERATION_HEADER, session.sessionId)
    return new Request(request, { headers })
  }

  private stopHeaders(session: StreamlineRelaySession): HeadersInit {
    return {
      [session.legacyContainer ? LEGACY_GENERATION_HEADER : SESSION_ID_HEADER]: session.sessionId,
      'X-Stop-Request-ID': crypto.randomUUID(),
    }
  }

  private closeRelaySockets(reason: string, sessionId?: string) {
    for (const role of ['publisher', 'viewer']) {
      for (const socket of this.ctx.getWebSockets(role)) {
        const attachment = socket.deserializeAttachment() as { sessionId?: string } | null
        if (sessionId && attachment?.sessionId !== sessionId) continue
        socket.close(1012, reason)
      }
    }
  }
}

function injectPreviewRelay(body: Record<string, unknown>, relay: { url: string; token: string }): Record<string, unknown> {
  const output = body.output
  if (!isRecord(output)) throw new Error('Resolved start request has no output')
  return { ...body, output: { ...output, relay } }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
