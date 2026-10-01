# Style Review Checklist

Use this checklist for the dedicated cleanup pass after functional behavior is stable.

- Confirm `AGENTS.md` and `GUIDELINES.md` match the code and current public Cloudflare guidance.
- Run all formatters, tests, type checks, vet, race tests, builds, and deployment dry-runs.
- Remove repeated code that has one clear shared abstraction.
- Name repeated protocol values, limits, timeouts, and user-facing strings.
- Replace workarounds with fixes at the layer that owns the problem.
- Check mutex scope, goroutine ownership, cancellation, stale-session fencing, and bounded queues.
- Check request validation, secret handling, URL redaction, and error status mapping.
- Remove comments that restate code and add comments where lifecycle or protocol invariants are non-obvious.
- Ensure critical behavior is tested and each test documents useful behavior.
- Remove dead exports, routes, compatibility aliases, generated artifacts, and stale documentation.
- Review public names, examples, and prose for an external audience.
