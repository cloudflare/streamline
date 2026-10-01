import type { StreamlineStartConfig } from './types.js'

export interface StreamlineRequestInit extends RequestInit {
  keepalive?: boolean
}

export type FetchLike = (input: RequestInfo | URL, init?: StreamlineRequestInit) => Promise<Response>

export interface StreamlineClientOptions {
  baseUrl: string | URL
  fetcher?: FetchLike
}

export interface StreamlineRequestOptions {
  signal?: AbortSignal
}

export interface StreamlineStopOptions extends StreamlineRequestOptions {
  requestId?: string
  keepalive?: boolean
}

export interface StreamlineStopResult {
  sessionId: string | null
  status: number
  replaced: boolean
}

export class StreamlineRequestError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

// A session reserves one media run and can successfully start it only once.
export class StreamlineSession {
  private readonly baseUrl: string
  private readonly fetcher: FetchLike
  private activeSessionId: string | null
  private startInFlight = false
  private started = false

  private constructor(options: StreamlineClientOptions, sessionId: string) {
    this.baseUrl = new URL(options.baseUrl).toString().replace(/\/$/, '')
    this.fetcher = options.fetcher ?? ((input, init) => globalThis.fetch(input, init))
    this.activeSessionId = sessionId
  }

  get id(): string | null {
    return this.activeSessionId
  }

  static async create(
    options: StreamlineClientOptions,
    requestOptions: StreamlineRequestOptions = {},
  ): Promise<StreamlineSession> {
    const session = new StreamlineSession(options, '')
    const response = await session.request('/relay/prepare', {
      method: 'POST',
      signal: requestOptions.signal,
    })
    await requireSuccess(response)
    const body = await response.json() as { session_id?: unknown }
    if (typeof body.session_id !== 'string' || !body.session_id) {
      throw new StreamlineRequestError('Session creation returned no session ID', response.status)
    }
    session.activeSessionId = body.session_id
    return session
  }

  static resume(options: StreamlineClientOptions, sessionId: string): StreamlineSession {
    return new StreamlineSession(options, sessionId)
  }

  async start<TResponse extends object = Record<string, unknown>, TConfig extends StreamlineStartConfig = StreamlineStartConfig>(
    config: TConfig,
    options: StreamlineRequestOptions = {},
  ): Promise<TResponse> {
    return this.startRequest(config, options)
  }

  // HTTP adapters may forward untrusted application requests for server-side validation.
  async startUnchecked<TResponse extends object = Record<string, unknown>, TConfig extends object = Record<string, unknown>>(
    config: TConfig,
    options: StreamlineRequestOptions = {},
  ): Promise<TResponse> {
    return this.startRequest(config, options)
  }

  private async startRequest<TResponse extends object, TConfig extends object>(
    config: TConfig,
    options: StreamlineRequestOptions,
  ): Promise<TResponse> {
    if (this.started || this.startInFlight) throw new Error('A Streamline session can start only once')
    const sessionId = this.requireSessionId()
    this.startInFlight = true
    try {
      const response = await this.request('/start', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...config, session_id: sessionId }),
        signal: options.signal,
      })
      await requireSuccess(response)
      this.started = true
      return await response.json() as TResponse
    } finally {
      this.startInFlight = false
    }
  }

  async ingest(chunk: BodyInit, options: StreamlineRequestOptions = {}): Promise<void> {
    const response = await this.requestWithSessionId('/ingest', {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: chunk,
      signal: options.signal,
    })
    await requireSuccess(response)
  }

  async annotation(image: BodyInit, options: StreamlineRequestOptions = {}): Promise<void> {
    const response = await this.requestWithSessionId('/api/annotation', {
      method: 'PUT',
      headers: { 'Content-Type': 'image/png' },
      body: image,
      signal: options.signal,
    })
    await requireSuccess(response)
  }

  async metrics<TResponse extends object = Record<string, unknown>>(
    options: StreamlineRequestOptions = {},
  ): Promise<TResponse> {
    const response = await this.requestWithSessionId('/metrics', { signal: options.signal })
    await requireSuccess(response)
    return await response.json() as TResponse
  }

  async stop(options: StreamlineStopOptions = {}): Promise<StreamlineStopResult> {
    const sessionId = this.activeSessionId
    const headers = new Headers()
    if (sessionId) headers.set('X-Streamline-Session-ID', sessionId)
    headers.set('X-Stop-Request-ID', options.requestId ?? crypto.randomUUID())
    const response = await this.request('/stop', {
      method: 'POST',
      headers,
      signal: options.signal,
      keepalive: options.keepalive ?? true,
    })
    const replaced = response.status === 409 && sessionId !== null
    if (!response.ok && !replaced) await requireSuccess(response)
    if ((response.ok || replaced) && this.activeSessionId === sessionId) this.activeSessionId = null
    return { sessionId, status: response.status, replaced }
  }

  private requireSessionId(): string {
    if (!this.activeSessionId) throw new Error('Create or resume a Streamline session before using it')
    return this.activeSessionId
  }

  private requestWithSessionId(path: string, init: StreamlineRequestInit): Promise<Response> {
    const headers = new Headers(init.headers)
    headers.set('X-Streamline-Session-ID', this.requireSessionId())
    return this.request(path, { ...init, headers })
  }

  private request(path: string, init: StreamlineRequestInit): Promise<Response> {
    return this.fetcher(`${this.baseUrl}${path}`, init)
  }
}

export class StreamlineClient {
  private readonly options: StreamlineClientOptions

  constructor(options: StreamlineClientOptions) {
    this.options = options
  }

  readonly sessions = {
    create: async (options: StreamlineRequestOptions = {}): Promise<StreamlineSession> => {
      return StreamlineSession.create(this.options, options)
    },
    resume: (sessionId: string): StreamlineSession => StreamlineSession.resume(this.options, sessionId),
  }
}

export function createStreamline(options: StreamlineClientOptions): StreamlineClient {
  return new StreamlineClient(options)
}

async function requireSuccess(response: Response): Promise<void> {
  if (response.ok) return
  const message = await response.text()
  throw new StreamlineRequestError(message || `Streamline request failed (${response.status})`, response.status)
}

export type {
  StreamlineCompositeInputs,
  StreamlineDirectInput,
  StreamlineInput,
  StreamlineInputTransform,
  StreamlineOutput,
  StreamlineStartConfig,
  StreamOperation,
} from './types.js'
