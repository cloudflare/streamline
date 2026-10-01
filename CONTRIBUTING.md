# Contributing

Issues and pull requests are welcome. Keep changes small, explain their user-visible behavior, and include tests for behavior or failure modes that change.

## Before Opening A Pull Request

```bash
cd container
go test ./...

cd ../packages/cloudflare
npm ci
npm test
```

Run the relevant `streamline-demo` checks when a change affects the shared protocol, Container integration, or package API.

## Design Boundaries

- Keep Streamline independent of any application. `streamline-demo` may consume Streamline; Streamline must not import or depend on the demo.
- Preserve session-ID fencing, bounded queues and request bodies, and capability-protected relay connections.
- Do not expose stream keys, relay capabilities, signed URLs, credentials, or arbitrary FFmpeg arguments.
- Keep application authentication and media policy outside the reusable engine and package.

Agent-assisted contributions are welcome. Review generated changes, understand their behavior, and run the applicable checks before submitting. Never include credentials, private media URLs, or other sensitive data in issues, commits, pull requests, or test fixtures.

Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md), not in a public issue.
