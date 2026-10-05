# Security Policy

## Supported Versions

Security fixes are made against the current default branch and the latest
`@cloudflare/streamline` npm release. Package fixes use the
[Changesets release process](docs/RELEASING.md); application and Container image
rollouts are managed separately.

## Reporting A Vulnerability

Use this repository's private GitHub vulnerability-reporting flow. Do not open a public issue or disclose exploit details before a fix is available.

Include the affected component, reproduction steps, expected and actual behavior, impact, and any proposed mitigation. Do not include credentials, stream keys, relay capabilities, signed URLs, or private media.

## Scope

Relevant reports include authentication and authorization boundaries, Durable Object session isolation, relay capability handling, request or queue bounds, Container egress, secret exposure, and FFmpeg input validation. The reference application has additional deployment-specific security controls in its own repository.
