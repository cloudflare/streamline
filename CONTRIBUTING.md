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

## Changesets

Use Node.js 24 and npm 11 for package development. Include a changeset when a
change to `@cloudflare/streamline` should reach consumers, including API changes,
bug fixes, runtime dependency updates, and changes to the published package.

```bash
cd packages/cloudflare
npm run changeset
npm run changeset -- status
```

Select `@cloudflare/streamline`, choose a patch, minor, or major bump, and write a
short description of the effect on consumers. Commit the generated file under
`packages/cloudflare/.changeset/` with your pull request. Use patch for compatible
fixes, minor for compatible new features, and major for breaking changes; Changesets
applies the selected bump literally, including while the package is at `0.x`.

Documentation-only changes, test-only changes, and release tooling changes that
do not affect the published package do not need a changeset. Container-only
changes do not produce npm releases; add a package changeset if the Worker package
also needs to change to stay compatible.

The release workflow updates package versions, the lockfile, and the changelog in
a separate pull request. See [Releasing](docs/RELEASING.md) for the maintainer flow.

## Design Boundaries

- Keep Streamline independent of any application. `streamline-demo` may consume Streamline; Streamline must not import or depend on the demo.
- Preserve session-ID fencing, bounded queues and request bodies, and capability-protected relay connections.
- Do not expose stream keys, relay capabilities, signed URLs, credentials, or arbitrary FFmpeg arguments.
- Keep application authentication and media policy outside the reusable engine and package.

Agent-assisted contributions are welcome. Review generated changes, understand their behavior, and run the applicable checks before submitting. Never include credentials, private media URLs, or other sensitive data in issues, commits, pull requests, or test fixtures.

Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md), not in a public issue.
