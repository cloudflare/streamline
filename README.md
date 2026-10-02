# Streamline

Streamline is a Go media engine for Cloudflare Containers. It receives webcam, Cloudflare Stream HLS, or application-resolved RTMPS input; applies a bounded processing pipeline; and produces fMP4 preview or RTMPS output.

This repository contains the Container server and the reusable `@cloudflare/streamline` Worker/Durable Object package. It does not deploy a Worker by itself. An application owns authentication, media policy, Wrangler configuration, and Container deployment.

## Build

Requirements: Go 1.25.4+, Docker, and Node.js 22.12+ for `@cloudflare/streamline`.
Use Node.js 24 and npm 11 for package development and release tooling.

```bash
# Container tests and image
cd container
go test ./...
docker build -t streamline:latest .

# Reusable Cloudflare package
cd ../packages/cloudflare
npm ci
npm test
```

The Container image includes FFmpeg, CA certificates, and Liberation/DejaVu fonts. The Go module uses Gorilla WebSocket; Testify is test-only. `@cloudflare/streamline` has a peer dependency on `@cloudflare/containers`.

## Deploy With An Application

Use an application such as [Streamline Demo](https://github.com/cloudflare/streamline-demo) to deploy Streamline. Applications consume a released `@cloudflare/streamline` package and configure a versioned Container image from the Cloudflare Registry. The application owns Worker configuration, auth, user interface, and media policy.

## Package Releases

`@cloudflare/streamline` in `packages/cloudflare` is the only npm package published
from this repository. [Changesets](packages/cloudflare/.changeset/README.md) tracks
version changes and release notes. The [release workflow](.github/workflows/release.yml)
creates a release pull request on `main`, then tests and publishes the versioned
package through npm trusted publishing when that pull request is merged.

See the [release guide](docs/RELEASING.md) for maintainer setup and release steps.
Container image publication and application deployment are managed separately.

## Documentation

- [Architecture](ARCHITECTURE.md): components, trust boundaries, limits, and media flow.
- [Session API contract](docs/SESSION_API_CONTRACT.md): Worker integration and typed session protocol.
- [`@cloudflare/streamline`](packages/cloudflare/README.md): package API and Durable Object base class.
- [Demo deployment guide](https://github.com/cloudflare/streamline-demo): owner and Playground deployments.
- [Contributing](CONTRIBUTING.md) and [security reporting](SECURITY.md).
- [Releasing](docs/RELEASING.md): Changesets, GitHub Actions, and npm trusted publishing.

## Security Model

Applications authenticate callers and resolve any sensitive media credentials before requests reach Streamline. The engine preserves session-ID fencing, bounded request and queue sizes, and capability-protected preview relay connections. Do not expose stream keys, relay capabilities, or arbitrary FFmpeg arguments to browsers.
