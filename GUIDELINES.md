# Code Guidelines

These conventions apply to the Go container in this repository and the TypeScript application in `streamline-demo`.

## General Principles

- Prefer the smallest correct change.
- Keep lifecycle ownership explicit and make stale asynchronous work harmless.
- Bound queues, request bodies, retries, and timeouts by bytes or duration.
- Validate at the network boundary before changing session state.
- Return or log an error at the appropriate ownership boundary; avoid logging the same failure repeatedly.
- Never log credentials, capabilities, signed URLs, or stream keys.
- Write tests that document behavior and failure handling rather than implementation details.

## Go

### Style

- Use standard `gofmt` formatting.
- Keep lines near 120 characters where practical.
- Prefer the standard library for core functionality.
- Use lowercase package names, snake_case file names, short receiver names, and concrete constructors.
- Group standard-library and non-standard-library imports separately.

### Error Handling

- Wrap errors with `fmt.Errorf` and `%w` when callers need to inspect the cause.
- Use sentinel or typed errors when HTTP status mapping depends on failure type.
- Add context without including sensitive source or destination URLs.

```go
if err := h.startFfmpeg(sessionID); err != nil {
    return fmt.Errorf("start ffmpeg: %w", err)
}
```

### Concurrency

- Document which mutex protects each shared field when ownership is not obvious.
- Never hold a mutex across network I/O, process waits, or potentially blocking channel operations.
- Fence goroutines by session ID before mutating shared state.
- Make cancellation idempotent and preserve unwritten media when reconnect behavior requires it.

### Tests

- Use `github.com/stretchr/testify/require` for fail-fast assertions.
- Use table-driven tests for validation and ffmpeg argument matrices.
- Use subtests for behavior with distinct setup or outcomes.
- Hand-write small mocks at protocol boundaries.
- Test byte limits, timeouts, stale session IDs, reconnects, and cleanup races.

## TypeScript

### Style

- Use single quotes and no semicolons.
- Use trailing commas in multiline literals.
- Prefer named exports.
- Use `interface` for object shapes and `type` for unions and aliases.
- Use arrow functions for inline callbacks, not as a default for named functions.
- Avoid `any` in production code; type browser and Cloudflare boundaries explicitly.

### Async Lifecycles

- Await, return, explicitly detach with `void`, or pass every promise to the appropriate runtime context.
- Abort superseded requests and check the current run before applying asynchronous results.
- Serialize webcam uploads and bound queued bytes.
- Keep Astro pages declarative; put DOM wiring and reusable lifecycle behavior in focused library modules.

### Tests

- Use descriptive behavior-oriented names.
- Exercise stop/start races, stale callbacks, retries, queue overflow, and browser API failures.
- Keep pure request/configuration builders independently testable.

## Cloudflare Boundaries

- Prefer bindings over public REST calls between Cloudflare services.
- Stream large or unbounded bodies rather than buffering them in a Worker.
- Use Durable Object storage or instance fields for coordinated state, never module-level mutable request state.
- Treat `Origin` as CSRF protection, not caller authentication.
- Use Web Crypto for tokens and capabilities.
- Keep secrets in bindings or Wrangler secrets, not source, configuration defaults, or browser storage.
