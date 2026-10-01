# Streamline Session API Contract

## Status

This is the current unpublished self-hosted Worker API implemented by
`@cloudflare/streamline`. It is deliberately binding-shaped: application code
uses a session and typed pipeline configuration rather than Durable Object or
container methods. A future native Cloudflare binding can preserve this session
model, but no native binding exists today.

The current package has two layers:

- `@cloudflare/streamline/client` is a browser-safe typed client for session
  control through an application's authenticated Worker origin.
- `@cloudflare/streamline` provides `StreamlineSessionDO` and
  `ContainerProxy` for Worker implementers. The application provides identity,
  media-profile policy, and Wrangler configuration.

`streamline-demo` is the reference implementation. Its symbolic RTMP profile
names resolve to server-side media credentials; browser code never receives or
sends an RTMP URL, stream key, relay capability, or Access service credential.

## Worker Integration

An application exports a concrete Durable Object subclass for Wrangler. The
base class owns generic session coordination, container startup, relay sockets,
expiry, and session-ID fencing. The application owns authentication and policy;
its application routes run only after the base class has handled all session and
relay routes.

```ts
import {
  ContainerProxy,
  StreamlineSessionDO,
  type ResolvedSessionStart,
} from '@cloudflare/streamline'

export { ContainerProxy }

export class MediaContainer extends StreamlineSessionDO<Env> {
  protected async resolveStartConfig(
    request: Request,
    body: Record<string, unknown>,
  ): Promise<ResolvedSessionStart> {
    // The outer Worker has already authenticated the request.
    // Resolve symbolic RTMP profiles and apply this application's limits here.
    return {
      body: resolveAndValidateMediaConfig(request, body),
      maxSessionSeconds: 60 * 60,
    }
  }
}
```

`ContainerProxy` and `StreamlineSessionDO` must be imported from the same
`@cloudflare/streamline` package instance. The Containers SDK keeps outbound
handlers in a module-local registry, and the proxy must see the handler
registered by the concrete session class.

The outer Worker authenticates every browser request, strips caller-supplied
trusted-principal and service-token headers, and forwards the verified principal
to the session Durable Object. The base class binds the resulting session ID to
that principal. The base class does not authenticate an arbitrary caller by
itself.

## Session Lifecycle

`await media.sessions.create()` reserves one one-run session and returns an
object with `session.id`. A session can successfully start only once; create a
new session for a later media run. `media.sessions.resume(sessionId)` remains
available only to recover control when application state has retained an existing
session ID.

Every state-changing operation belongs to that session:

```text
create -> start -> ingest / annotation / metrics -> stop
```

Creation prepares the relay and is required before `start`, `ingest`,
`annotation`, or `metrics`. The client adds the session ID to each applicable
request. The service uses session IDs as internal stale-request fences and
rejects stale IDs and requests from a different authenticated principal. Session
IDs are not externally called generations.

| Client method | HTTP request | Session-ID behavior |
| --- | --- | --- |
| `sessions.create()` | `POST /relay/prepare` | Creates one session and returns its opaque ID. |
| `session.start(config)` | `POST /start` | Client adds `session_id` to JSON. |
| `session.ingest(chunk)` | `POST /ingest` | Client adds `X-Streamline-Session-ID`. |
| `session.annotation(png)` | `PUT /api/annotation` | Client adds `X-Streamline-Session-ID`. |
| `session.metrics()` | `GET /metrics` | Client adds `X-Streamline-Session-ID`. |
| `session.stop()` | `POST /stop` | Client sends `X-Streamline-Session-ID` and a stop request ID. |

Non-2xx responses throw `StreamlineRequestError`, which includes `status`.
`stop` is special: a `409` means a newer session has replaced this session,
so it returns `{ replaced: true }` rather than throwing.

## 1. Create A Session

`baseUrl` is the application Worker origin, not a direct container URL. Browser
authentication, such as a Cloudflare Access session cookie, is sent to that
origin normally.

```ts
import { createStreamline } from '@cloudflare/streamline/client'

const media = createStreamline({
  baseUrl: 'https://media.example',
})

const session = await media.sessions.create()
console.log(session.id) // A server-generated opaque session ID.
```

Use `media.sessions.resume(sessionId)` only when application state has retained
the current session ID and the same authenticated user is recovering control.

## Start Configuration

`session.start()` accepts the following public configuration. An application
can apply stricter policy, including restricting profile names, output settings,
and input URLs.

```ts
type StreamlineStartConfig = {
  pipeline: StreamOperation[]
  output:
    | { mode: 'websocket'; format?: 'fmp4' }
    | { mode: 'rtmp'; profile: string }
} & (
  | { input: { type: 'webcam' } | { type: 'hls'; url: string } | { type: 'rtmp'; profile: string } }
  | {
      inputs: [
        { type: 'hls'; url: string } | { type: 'rtmp'; profile: string },
        {
          type: 'webcam'
          transform: {
            scale: number
            position: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right'
          }
        },
      ]
    }
)
```

The pipeline is an ordered list with one required `encode` operation and up to
16 operations total. Current operations are:

```ts
type StreamOperation =
  | { op: 'overlay'; params: { image: '/app/assets/cf-logo.png'; position: 'top-right' } }
  | { op: 'overlay'; params: { image: 'annotation'; position: 'full' } }
  | { op: 'subtitle'; params: { source: 'auto' } }
  | {
      op: 'filter'
      params:
        | { preset: 'blur' | 'brightness' | 'contrast' | 'gamma' | 'saturation' | 'sharpen'; amount: number }
        | { preset: 'flip' }
        | { preset: 'rotate'; degrees: 0 | 90 | 180 | 270 }
    }
  | {
      op: 'encode'
      params: {
        codec: 'h264'
        preset?: 'ultrafast' | 'superfast' | 'veryfast' | 'faster' | 'fast' | 'medium'
        bitrate?: string
        resolution?: string
        fps?: number
        gop?: number
      }
    }
```

The current engine accepts only the allowlisted operation fields and values. It
does not expose arbitrary ffmpeg arguments. `session_id` and, for preview
output, `output.relay` are server-owned fields. Do not supply either one.

The reference application's HLS policy accepts only canonical Cloudflare Stream
manifests at `https://videodelivery.net/<video-id>/manifest/video.m3u8`, with no
query string or fragment. Its current rotation policy accepts `0`, `90`, `180`,
or `270` degrees. Other applications can apply stricter policy.

## 2. RTMP Input With Overlay And RTMP Output

Profiles are names selected by the application. The Worker resolves them to
protected RTMP configuration before starting the container.

```ts
const session = await media.sessions.create()

const result = await session.start({
  input: { type: 'rtmp', profile: 'primary-input' },
  pipeline: [
    {
      op: 'overlay',
      params: { image: '/app/assets/cf-logo.png', position: 'top-right' },
    },
    {
      op: 'encode',
      params: {
        codec: 'h264',
        preset: 'fast',
        bitrate: '1500k',
        resolution: '1280x720',
        fps: 30,
      },
    },
  ],
  output: { mode: 'rtmp', profile: 'primary-output' },
})

console.log(result) // Application-defined start response, normally { status: 'started', ... }.
```

RTMP output runs independently after `start` returns. It does not create a
browser preview relay.

## 3. HLS Input With Subtitles And RTMP Output

Use a canonical Cloudflare Stream HLS manifest as the input and a symbolic
server-owned output profile. `source: 'auto'` discovers and burns available HLS
subtitles. It never accepts a subtitle URL or RTMP key from browser code.

```ts
const streamVideoId = 'your-cloudflare-stream-video-id'
const session = await media.sessions.create()

const result = await session.start<{
  status: 'started'
  subtitle?: { state: 'ready' | 'unavailable'; language?: string; warning?: string }
}>({
  input: {
    type: 'hls',
    url: `https://videodelivery.net/${streamVideoId}/manifest/video.m3u8`,
  },
  pipeline: [
    { op: 'subtitle', params: { source: 'auto' } },
    {
      op: 'encode',
      params: {
        codec: 'h264',
        preset: 'fast',
        bitrate: '1500k',
        resolution: '1280x720',
        fps: 30,
      },
    },
  ],
  output: { mode: 'rtmp', profile: 'default' },
})

if (result.subtitle?.state === 'unavailable') {
  console.warn(result.subtitle.warning ?? 'No compatible subtitle track was found')
}
```

In `streamline-demo`, `default` is resolved to the server-managed RTMP output
profile. A subtitle fetch or conversion failure does not fail the media session:
the start response reports `subtitle.state: 'unavailable'` and streaming continues
without burn-in.

## 4. Webcam Input With Filters And Preview Output

Webcam input is delivered separately through `session.ingest()`. A preview
output produces fragmented MP4 over the relay described below.

```ts
const session = await media.sessions.create()

await session.start({
  input: { type: 'webcam' },
  pipeline: [
    { op: 'filter', params: { preset: 'brightness', amount: 0.1 } },
    { op: 'filter', params: { preset: 'flip' } },
    { op: 'overlay', params: { image: 'annotation', position: 'full' } },
    {
      op: 'encode',
      params: {
        codec: 'h264',
        preset: 'veryfast',
        bitrate: '1500k',
        resolution: '1280x720',
        fps: 30,
        gop: 60,
      },
    },
  ],
  output: { mode: 'websocket', format: 'fmp4' },
})
```

For durable remote preview, open the viewer relay after preparation and before
starting the session, as shown in the next section. The service rejects a
publisher when no viewer for its session ID is connected.

## 5. Send Webcam Media Chunks

Send `MediaRecorder` WebM chunks in order. The SDK fences each request by session
ID but does not reorder concurrent calls, so keep one upload in flight
at a time.

```ts
const stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: true })
const recorder = new MediaRecorder(stream, { mimeType: 'video/webm;codecs=vp8,opus' })
let uploadTail = Promise.resolve()

recorder.addEventListener('dataavailable', (event) => {
  if (event.data.size === 0) return
  uploadTail = uploadTail
    .then(() => session.ingest(event.data))
    .catch((error) => reportUploadFailure(error))
})

recorder.start(250)

// On shutdown: recorder.stop(); await uploadTail; then await session.stop().
```

`ingest` sends `application/octet-stream`; the current Worker and Durable Object
each bound an individual request to 1 MiB. Applications should bound their local
queue and stop the session if the browser cannot drain it.

## 6. Preview Media Relay

The relay is media-only. It is not a control channel and does not accept browser
messages.

```ts
function openViewer(origin: string, sessionId: string): Promise<WebSocket> {
  const url = new URL('/relay/view', origin)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  url.searchParams.set('session_id', sessionId)

  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url)
    socket.binaryType = 'arraybuffer'
    socket.addEventListener('open', () => resolve(socket), { once: true })
    socket.addEventListener('error', () => reject(new Error('preview relay failed')), { once: true })
  })
}

const viewer = await openViewer('https://media.example', session.id!)

viewer.addEventListener('message', (event) => {
  if (typeof event.data === 'string') {
    if (event.data === '{"type":"eos"}') mediaSource.endOfStream()
    return
  }
  const unit = new Uint8Array(event.data as ArrayBuffer)
  if (unit.length === 0) return
  sourceBuffer.appendBuffer(unit)
})
```

The production path is:

```text
Container publisher WebSocket -> Durable Object relay -> browser viewer WebSocket -> MediaSource
```

After `start`, the Durable Object injects a private publisher relay URL and a
one-session capability into the container request. The container uses that
capability to connect. The Worker outbound handler adds its service credential
only for that outbound publisher connection. The Durable Object verifies both
the publisher capability and active viewer session ID, then fans binary frames
to viewers. Browser viewers authenticate through the application's normal
same-origin policy and must never connect to `/relay/publish`.

Binary frames contain a complete fMP4 initialization unit (`ftyp` plus `moov`)
or media unit (`moof` plus `mdat`). A terminal `{"type":"eos"}` text frame
marks end of output.

Production callers need bounded reconnect, MSE append, and buffer-retention
logic. `streamline-demo` currently supplies these browser responsibilities; the
reusable browser helper package is deferred.

## 7. Send A Canvas PNG Overlay

Include the `annotation` overlay operation at start time, then publish PNG
snapshots for the active session.

```ts
function canvasPng(canvas: HTMLCanvasElement): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob)
      else reject(new Error('Canvas could not produce a PNG'))
    }, 'image/png')
  })
}

const png = await canvasPng(overlayCanvas)
await session.annotation(png)
```

`annotation` sends `PUT /api/annotation` with `image/png`. Send snapshots only
after a successful `start`; the current request limit is 5 MiB. Replacing a
snapshot is intentional: Streamline retains the latest complete annotation, not
an unbounded annotation history.

## 8. Read Session Metrics

Metrics are an application-defined response typed by the caller. The demo
returns container activity, ffmpeg progress, output freshness, subscriber state,
reconnect-buffer state, and ingest-write state.

```ts
type Metrics = {
  running?: boolean
  sessionActive?: boolean
  outputMode?: 'websocket' | 'rtmp'
  ffmpeg?: {
    state?: string
    frame?: number
    fps?: number
    bitrate?: string
    outTimeUs?: number
    speed?: number
    dropFrames?: number
  }
  outputAgeMs?: number
  restartCount?: number
  lastFfmpegExitError?: string
  outputSubscriber?: boolean
  reconnectBufferBytes?: number
  ingestWriting?: boolean
}

const metrics = await session.metrics<Metrics>()
if (metrics.outputAgeMs !== undefined && metrics.outputAgeMs > 15_000) {
  reportStalePreview(metrics)
}
```

Metrics remain session-ID- and principal-fenced. They are not a general public
container-inspection API.

## 9. Stop A Session

Always stop in a `finally` block. Stop requests carry a unique ID for operational
correlation and default to `keepalive: true` so best-effort cleanup can continue
during page teardown.

```ts
try {
  // Prepare, start, ingest, and display media.
} finally {
  const stopped = await session.stop({
    requestId: crypto.randomUUID(),
    keepalive: true,
  })

  if (stopped.replaced) {
    // A newer session already owns this session identity.
  }
}
```

Stopping clears the active session ID in that client object after either a
successful response or replacement. A caller can safely retry a failed stop with
`media.sessions.resume(sessionId)` while its authenticated session remains
valid.

## Limits And Boundaries

| Concern | Current contract |
| --- | --- |
| Start body | JSON object, maximum 64 KiB. |
| Webcam ingest request | `application/octet-stream`, maximum 1 MiB. |
| Annotation request | `image/png`, maximum 5 MiB. |
| Publisher relay frame | Maximum 2 MiB fMP4 payload. |
| Prepare lifetime | 15 minutes before expiry. |
| Start commitment lifetime | 2 minutes while container startup completes. |
| Session ownership | Verified application principal plus opaque session ID. |
| Preview credentials | Private, server-injected; never a browser API parameter. |

`startUnchecked()` exists for application adapters whose configuration extends
the base schema or is intentionally validated on the server. It still applies
session-ID fencing. Direct callers should prefer the typed `start()` method.
