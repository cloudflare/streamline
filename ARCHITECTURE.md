# Streamline Architecture

## Purpose

Streamline is a browser-operated media pipeline. A Cloudflare Worker and Durable Object control an ffmpeg process in a Cloudflare Container without keeping a long-lived Worker-to-Container request open. The design prioritizes bounded resource use, session-ID-safe lifecycle changes, and keeping media credentials outside the browser.

Each public session is a one-run reservation created through `await media.sessions.create()`. Its opaque `session.id` is used internally to fence stale requests, but the public API and documentation call it a session ID, never a generation. Starting the same session more than once is rejected; a later run requires a new session. `media.sessions.resume(sessionId)` is only for recovering control of an existing session.

The reference application has two isolated deployments: a private, owner-managed Access application and a public, Turnstile-admitted Playground. The owner profile gives every identity admitted by its Access policy equal application rights. The Playground uses anonymous signed principals, per-principal Container routing, and a global admission coordinator.

## Components

| Component | Responsibility |
|---|---|
| Browser UI | Select source and processing options, manage shared write-only media overrides, upload webcam chunks and annotation snapshots, display fMP4 preview |
| Cloudflare Access | Admit authorized internal users and independently authenticate the container's relay publisher |
| Worker entrypoint | Validate Access JWTs, enforce deployment state, overwrite trusted principal headers, bound request bodies, route requests, and add browser security headers |
| `MediaContainer` Durable Object | Own singleton container access, shared media overrides, relay session state, publisher/viewer WebSockets, and session-ID/identity checks |
| Cloudflare Container | Run the Go server and one ffmpeg process, ingest media, apply processing, and publish complete fMP4 units |
| Astro server | Render application pages and provide local-development proxy routes |

The Go container and reusable `@cloudflare/streamline` session package are in this
repository. The package provides the Durable Object session coordinator and typed
HTTP session client. `../streamline-demo` contains the Astro UI, deployment
profiles, Access policy, and a thin `MediaContainer` subclass that Wrangler
registers for its deployment.

The package is versioned with Changesets and published to npm by GitHub Actions
using trusted publishing. This release process covers the Worker and client
package; Container image publication and application deployment are separate.
Applications must select compatible package and image versions. See
[Releasing](docs/RELEASING.md) for the release process.

## Deployment Profiles

### Owner

The owner deployment is `streamline-demo`.

| Control | Owner value |
|---|---|
| Identity | Owner-managed Access policy; Access application JWT revalidated in Worker code |
| Container instances | 1 |
| Active media sessions | 1 |
| Session duration | 28,800 seconds (8 hours) |
| Maximum frame size | 1920x1080 |
| Maximum frame rate | 30 fps |
| Maximum video bitrate | 8 Mbps |
| Codec | H.264 |
| Slowest accepted preset | `medium` |
| RTMP transport | RTMPS on explicit port 443 |
| Diagnostics | Forced off |

Sequential authenticated-user sessions are allowed. A user may replace their own session, but a different principal cannot evict an active session. Starting a replacement signals the prior ffmpeg process for termination before creating another. Waiting for the prior process to exit is tracked as feature-lock work in `TODO.md`.

### Playground

The Playground deployment is `streamline-demo-playground`. It uses distinct Worker, Container, Durable Object, and coordinator resources. It admits a verified Turnstile request before issuing an anonymous signed-principal cookie, routes each principal to a distinct Container identity, and limits the profile to three active leases globally.

Playground limits are 1280x720, 30 fps, 3 Mbps, and 30 minutes. It accepts webcam and canonical Cloudflare Stream HLS input, produces browser fMP4 preview only, and blocks RTMP credentials, media overrides, metrics, and diagnostics. Its public hostname and Turnstile secrets must be configured before deployment; it fails closed when that configuration is missing.

Owner and playground must remain separate deployments. Sharing source code is acceptable; sharing Durable Object state, container names, Access applications, credentials, or quota counters is not.

## Authentication Boundaries

### Owner Application Requests

Cloudflare Access protects the whole owner hostname with an Allow policy containing the configured owner email and employee email-domain selectors. All identities admitted by that policy have equal application rights. Access adds `Cf-Access-Jwt-Assertion` after authentication.

The Worker performs a second verification with `jose`:

1. Fetch keys from the configured Access team domain.
2. Verify the JWT signature, issuer, audience, lifetime, and `type=app`.
3. Require the identity email and `sub`, then use `sub` as the internal principal.
4. Replace any caller-provided `X-Streamline-Principal` before forwarding to the Durable Object.

Missing configuration or a failed check returns `401`. Origin and content-type checks remain CSRF/request-shape controls; they are not authentication.

### Preview Publisher Requests

`/relay/publish` has a more-specific Access application with a Service Auth policy. The final outbound request contains:

- `CF-Access-Client-Id`
- `CF-Access-Client-Secret`
- `Authorization: Bearer <one-session publisher capability>`

The container relay configuration contains only `url` and `token`, and rejects Access credentials or other unknown fields. The container supplies the token as the bearer capability. Its WSS handshake is intercepted by the Container outbound Worker, which adds the first two headers only for the exact owner hostname and `/relay/publish` path. The long-lived service secret therefore never enters container memory. Access validates the service token before the request reaches the main Worker, which strips both Access credential headers before forwarding to the Durable Object. The Durable Object then hashes the bearer capability and compares it with the stored SHA-256 hash for the requested session ID.

Service Auth and the session capability solve different problems. Service Auth proves the caller is the deployment's container workload. The capability proves the workload is publishing the currently prepared session, not a stale or unrelated session.

During rollout only, the publisher Access application may temporarily use a Bypass policy. The operator runbook creates the service token, deploys publisher-header support, tests a stream, and only then replaces Bypass with Service Auth.

## Credential Model

Browser start requests send media profile names, never RTMP URLs or keys. The only current name is `default`. A separate authenticated, same-origin settings API atomically accepts a write-only RTMPS key and matching Stream Live Input ID for either shared profile and stores the pair in Durable Object storage.

Worker secrets hold:

- `MEDIA_RTMP_INPUT_PROFILE`
- `MEDIA_RTMP_OUTPUT_PROFILE`
- `PUBLISHER_ACCESS_CLIENT_ID`
- `PUBLISHER_ACCESS_CLIENT_SECRET`

Each media profile secret is a JSON object containing `key` and `liveInputId`, so a default pair is installed and rotated atomically. The Worker derives account-independent `videodelivery.net` HLS and `iframe.videodelivery.net` player URLs from the Live Input ID, allowing the input and output profiles to belong to different Stream accounts. It resolves `default` after authenticating the request and forwards only the validated key to the container. The Go container constructs `rtmps://live.cloudflare.com:443/live/<key>`. A complete Durable Object profile takes precedence over the corresponding structured secret; a missing or invalid effective pair fails closed with `503`. The Worker revalidates stored values at use time and overwrites browser-supplied URL, key, and relay fields.

The settings status response returns edit availability, `override`, `default`, or `missing`, validity, update time, and browser-safe URLs derived from the Live Input IDs. It never returns an RTMPS URL or stream key. The annotation view fetches the effective input HLS URL only for RTMPS annotation; the output view fetches the effective player URL only for RTMPS output. Responses are authenticated and `no-store`, and the URLs remain in page memory. Password-style RTMPS key fields are transient. Legacy RTMPS and presentation URL fields are deleted from browser `localStorage` when settings load.

RTMPS profile keys are limited to 2,011 ASCII URL-safe characters. The input uses an RTMPS playback key and the output uses a broadcast stream key. The endpoint, scheme, port, and path prefix are constants in the Go container. Keys are not logged or returned to the browser.

The publisher capability exists in the container only for the active session. Relay storage contains its hash, not its plaintext value. Access service credentials remain in Worker bindings and are injected outside the container sandbox.

## Egress Policy

The container class exports `ContainerProxy`, enables HTTPS interception, and registers an outbound handler only for its own Worker hostname. A root-owned startup wrapper installs the ephemeral Containers CA, refreshes the system trust store, and immediately drops privileges before starting the Go process as `streamline`. The outbound Worker adds Access credentials only to the exact HTTPS relay publisher handshake. Other HTTP, HTTPS, and raw TLS media traffic uses direct container internet access.

Cloudflare Containers catch-all interception cannot proxy RTMPS because it is raw TLS rather than HTTP, despite using port 443. `allowedHosts` also enables catch-all TLS interception, so the deployment intentionally uses neither that property nor a catch-all outbound handler. The application compensates by accepting no RTMP URL or key in browser start requests, resolving only Worker-owned profiles, validating keys as bounded single path segments in both layers, and constructing only the fixed Cloudflare Stream RTMPS endpoint in the container. This controls the authenticated request/SSRF surface, but it is not a network egress sandbox if the trusted container process itself is compromised.

The Worker additionally accepts only canonical HTTPS `https://videodelivery.net/<video-id>/manifest/video.m3u8` HLS inputs. This prevents an authenticated browser from turning ffmpeg into a general-purpose URL fetcher.

## Request Boundaries

| Route | Maximum request body | Authentication/state checks |
|---|---:|---|
| `POST /relay/prepare` | None | Access application JWT, principal ownership, session replacement |
| `POST /start` | 64 KiB | Access application JWT, named media policy, operation and encoding limits |
| `GET /api/media-overrides` | None | Access application JWT, profile status and derived presentation URLs, no RTMPS values |
| `PUT /api/media-overrides` | 4 KiB | Access application JWT, same origin, atomic key/Live Input ID pair, bounded key policy |
| `POST /ingest` | 1 MiB | Access application JWT, principal ownership, `X-Streamline-Session-ID` header |
| `PUT /api/annotation` | 5 MiB | Access application JWT, principal ownership, `X-Streamline-Session-ID` header, PNG validation |
| `GET /metrics` | None | Access application JWT, principal ownership, `X-Streamline-Session-ID` header |
| `POST /stop` | None | Access application JWT, principal ownership, `X-Streamline-Session-ID` header |
| `/relay/publish` WebSocket message | 2 MiB fMP4 payload | Access Service Auth, publisher capability, active session |

Unexpected oversized bodies return `413` before they enter container logic. The container requires 1–16 pipeline operations and exactly one encode operation. Both the container and production Worker restrict encoding to H.264. Only overlay, subtitle, filter, and encode operations are accepted. Each operation rejects parameters outside its exact allowlist:

- Overlay requires one of two exact image/position pairs: `/app/assets/streamline-logo.png` at `top-right`, or `annotation` at `full`.
- Subtitle accepts only optional `source: auto`.
- Amount-based filters require `amount` and reject `degrees`; `flip` accepts neither; `rotate` requires `degrees` and rejects `amount`. Existing preset and numeric ranges remain enforced.
- Encode accepts only `codec`, `preset`, `bitrate`, `resolution`, `fps`, and `gop`; when present, `codec` must be `h264`. The Go server enforces all supported values and bounds.

Synthetic `test` input is a narrow local diagnostic contract: its pipeline must be exactly one parameterless `encode` operation. The fixed test source and encoding settings reject overlays, filters, subtitles, and encode overrides instead of ignoring them. Production forces `diagnostics=false`, rejects synthetic input in the Worker, and returns `404` for `/test-stream`.

## Media Flows

### Webcam to Browser Preview

1. Browser authenticates through Access.
2. Browser calls `await media.sessions.create()`, which prepares a new session through `POST /relay/prepare`.
3. Durable Object stores the authenticated principal, session ID, and a 15-minute relay preparation expiry.
4. Browser initializes MediaSource and connects the session-ID-fenced `/relay/view` WebSocket.
5. Browser calls `POST /start` with `input.type=webcam` and `output.mode=websocket`.
6. Worker validates policy and injects relay URL and publisher capability.
7. Container starts a session and connects outbound to `/relay/publish`; the outbound Worker adds Access service credentials to that exact WSS handshake.
8. Browser uploads serialized, byte-bounded MediaRecorder chunks to `/ingest`.
9. Container writes complete `ftyp+moov` and `moof+mdat` units to the publisher WebSocket.
10. Durable Object forwards complete fMP4 units to the viewer WebSocket.
11. Browser appends units to MediaSource with a bounded queue.

### Direct HLS or RTMPS to Browser Preview

The control and relay path is the same, but ffmpeg reads the input directly after `/start` returns. HLS comes from the validated browser video ID. RTMPS comes from the server-side input profile. In annotation mode, the browser separately fetches the HLS URL derived from that profile's Live Input ID to show the raw source. Webcam annotation does not make this request. The autonomous container publisher avoids a long-lived Worker-to-Container response.

### Direct Background With Webcam Picture-in-Picture

Picture-in-picture uses an additive `inputs` request containing exactly two ordered inputs. The first is the selected direct HLS or server-managed `default` RTMPS source and has no transform. The second is browser webcam video with a required scale from zero to one and one of four corner positions. Existing single-input flows continue to use `input`.

The Worker reconstructs both entries, replacing an RTMPS profile with its validated key and discarding browser-supplied media fields. The container opens the direct source as ffmpeg input zero, accepts video-only MediaRecorder WebM through `/ingest` as input one, normalizes their timestamps, scales the webcam relative to the output dimensions, and composites it over the background. RTMPS output explicitly maps optional audio from input zero; RTMPS sources copy AAC while HLS sources are encoded to AAC. Browser preview retains HLS audio and remains video-only for RTMPS.

Mixed sessions start ffmpeg immediately and allow 20 seconds for the first webcam chunk, then 10 seconds between chunks. Direct-only RTMPS sessions retain automatic process restart. Mixed direct/webcam failures are terminal because a restarted ffmpeg process would require a fresh WebM initialization stream; the browser must create a new session and MediaRecorder instead of splicing later chunks into a new process.

### Any Input to RTMPS Output

The browser selects RTMP output by profile name. The Worker replaces the output with the effective Durable Object profile or deployment default. The container constructs the fixed RTMPS destination from its key, and ffmpeg publishes directly to it; no preview publisher capability is created for output media. The browser output panel fetches the player URL derived from the paired Live Input ID.

## Session Lifecycle

Every session-changing path carries a session ID. The Durable Object binds each relay session ID to the verified Access principal. The container stores the active session ID and rejects stale ingest, annotation, metrics, stop, publisher, and viewer operations. These internal stale-request fences do not make session IDs public generations.

Before changing a prepared relay to starting state, the Durable Object applies an independent 25-second wall-clock deadline while waiting for the container instance and port 8080. Client cancellation propagates through the Worker and aborts the readiness wait, while the browser allows 45 seconds for the complete start operation. This avoids sending `POST /start` during a cold start without retrying that state-changing request. If readiness times out or capacity is unavailable, the Worker returns `503`; a prepare-only relay can be cleared without starting a container just to stop it.

The Go server owns an independent maximum-session timer. A timer records the container session ID, and its callback stops ffmpeg and the relay publisher only if that ID is still active. Starting or stopping a session cancels the previous timer, preventing an old deadline from killing a newer session.

The Durable Object schedules session-ID-fenced cleanup when it writes relay state. A preparation record expires after 15 minutes if `/start` does not commit it and remains in place during the bounded readiness wait. After readiness succeeds, the Durable Object writes a two-minute starting record before sending the state-changing container request, bounding ambiguous Worker or isolate failures. Only a successful container response atomically records the running session ID and full deadline. A committed relay remains stoppable for a five-minute grace period beyond the deployment profile's process deadline. At relay expiry, the Durable Object stops the matching container session before deleting state and closing sockets; failed expiry stops retry after one minute. The container activity timeout is renewed while a committed session is active, so autonomous HLS and RTMPS processing is not mistaken for idle work. An idle container with no valid session is destroyed while holding the lifecycle lock before queued starts proceed. This does not replace the Go process timer; the controls intentionally exist at both coordination and process layers.

## Media Processing Safety

- Static overlays are restricted to `/app/assets/streamline-logo.png` at the top-right.
- Dynamic overlays use the literal `annotation` source at full-frame and a server-owned pipe.
- Picture-in-picture accepts only an HLS or RTMPS primary input followed by one webcam input with a bounded scale and corner position.
- Arbitrary filesystem paths are rejected by both Worker policy and Go protocol validation.
- PNG annotation bodies are validated and replayed with fixed 5 fps wall-clock pacing.
- Annotation writes are nonblocking and bounded so slow overlay updates cannot stall media processing.
- The media session configuration has no arbitrary ffmpeg argument escape hatch.
- ffmpeg stderr removes configured sensitive URLs and then redacts any remaining HTTP, HTTPS, RTMP, or RTMPS URL; keys are never logged separately.
- The runtime image has no shell login user for the process and runs as `streamline`, not root.

## Browser Security

Production Astro responses add:

- Content Security Policy with `script-src 'self'` and `frame-ancestors 'none'`.
- `X-Content-Type-Options: nosniff`.
- `X-Frame-Options: DENY`.
- `Referrer-Policy: no-referrer`.
- Restrictive Permissions Policy.
- `Cache-Control: no-store` for HTML; authentication failures, disabled-profile responses, and media-override responses are also non-cacheable.

`hls.js` is bundled into the application; production no longer executes JavaScript from a CDN. Inline style remains allowed because the current Astro UI uses component and dynamic inline styles.

Unsigned Stream Live Input IDs are sensitive presentation metadata but are not treated as ingest credentials. If signed playback is enabled, the Worker must mint short-lived principal-scoped playback tokens; static signed URLs must not be persisted.

## Failure Behavior

- Missing or unknown deployment profile: `503`.
- Disabled playground profile: `503` for every route and asset.
- Missing or invalid Access application JWT: `401`.
- Missing required Worker secrets: `/start` fails closed with `503`; deployment tooling refuses an actual owner deploy.
- Missing or invalid effective RTMPS key or Live Input ID: `/start` fails closed with `503`.
- Container readiness timeout or unavailable capacity: `503` with `Retry-After: 2`; allocation rate limiting: `429`; other startup failures: `500`.
- Unknown media profile, arbitrary HLS host, excessive encoding request, or unsupported operation: `400`.
- Wrong relay principal or stale session ID: `403` or `409` depending on the lifecycle conflict.
- Invalid publisher capability: WebSocket upgrade rejected with `401`.
- Oversized body/message: `413` or WebSocket close code `1009`.
- Arbitrary HLS or browser-supplied RTMP destination: rejected by Worker policy before container startup.
- RTMP URL in a start request: rejected; only the server-owned RTMPS port-443 profile is accepted.

## Operational Change Order

Access policy changes are not atomic with Worker deployments. For a fresh owner installation, the safe order is:

1. Install the paired RTMPS key and Live Input ID profile secrets.
2. Create the Access service token and install publisher secrets while the path still has temporary Bypass.
3. Verify tests, owner dry-run, playground dry-run, and the required secret names.
4. Deploy the owner Worker and container.
5. Complete an authenticated preview smoke test.
6. Replace publisher Bypass with Service Auth.
7. Test another preview and verify owner, employee, anonymous, and service boundaries.

The exact commands and rollback guidance are in `../streamline-demo/README.md`.

## Deliberate Limitations

- The owner profile is shared by authorized internal users but remains singleton-routed.
- Access is identity and admission control, not multi-tenant isolation.
- The playground has no per-user state or global quota coordinator and remains disabled.
- There is no arbitrary URL input, arbitrary overlay file, browser-provided stream key, or production raw-ffmpeg escape hatch.
- There is no automatic container eviction policy for a public queue because public admission is not enabled.
