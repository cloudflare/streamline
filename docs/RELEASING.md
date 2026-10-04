# Releasing @cloudflare/streamline

The repository publishes one npm package: `@cloudflare/streamline` from
`packages/cloudflare`. Its Changesets configuration and pending release notes
live in `packages/cloudflare/.changeset/`. All npm commands below run from the
package directory, using Node.js 24 and npm 11.

The [release workflow](../.github/workflows/release.yml) runs on pushes to `main`
and can also be dispatched manually on `main`. It runs on a GitHub-hosted runner
and uses npm's OIDC trusted publishing. It does not require an npm token secret.

## Maintainer Setup

1. In GitHub repository settings, under **Actions → General → Workflow
   permissions**, enable **Allow GitHub Actions to create and approve pull
   requests**. Organization policy must also permit this. The workflow requests
   `contents: write` and `pull-requests: write` for release commits, tags, releases,
   and the version pull request.
2. In the npm settings for `@cloudflare/streamline`, add a **GitHub Actions**
   trusted publisher with these exact values:

   | Field | Value |
   | --- | --- |
   | Organization or user | `cloudflare` |
   | Repository | `streamline` |
   | Workflow filename | `release.yml` |
   | Environment name | Leave empty; this workflow does not set an environment |
   | Allowed actions | Enable direct publishing with `npm publish` |

   Enter the workflow filename alone, not `.github/workflows/release.yml`.
   The workflow grants `id-token: write` so npm can exchange the GitHub identity
   for a short-lived publishing credential. The package's `repository.url` must
   continue to point to `https://github.com/cloudflare/streamline`.

`@cloudflare/streamline` already exists on npm, so these settings can be configured
on the existing package. Renaming the package would require a separate initial
publication and trusted publisher setup for the new name.

See [npm's trusted publishing documentation](https://docs.npmjs.com/trusted-publishers/)
and the [Changesets automation guide](https://changesets.dev/guide/automating) for
the provider requirements. Trusted publishing generates npm provenance when both
the package and its source repository are public. npm does not generate provenance
for packages published from private repositories.

## Normal Release Flow

1. Add a changeset with the consumer-facing change:

   ```bash
   cd packages/cloudflare
   npm ci
   npm run changeset
   npm run changeset -- status
   npm test
   ```

   Commit the generated Markdown file with the implementation. See
   [Contributing](../CONTRIBUTING.md#changesets) for version bump guidance.
2. Merge the change into `main`. After package tests pass, the workflow creates or
   updates a `chore: release @cloudflare/streamline` pull request. Changesets
   consumes the pending notes, updates `package.json`, and generates
   `packages/cloudflare/CHANGELOG.md`. The version script also synchronizes
   `package-lock.json` so `npm ci` works after the version change.
3. Review the version and changelog in the release pull request, then merge it
   when ready to publish. This merge is the release decision. Further changesets
   merged before it is released are collected into the same release pull request.
4. The next run tests the package and its packed consumer, then publishes any
   unpublished package version to the public npm registry. The package's
   `prepack` script compiles the JavaScript and declarations before publication.
   Changesets creates a version tag and GitHub release using the changelog.

Publishing uses the `latest` npm dist-tag. Changesets skips versions that already
exist on npm, so later runs without pending changes or unpublished versions do
not republish the package. The workflow serializes runs on `main` and does not
cancel a release in progress.

This workflow tests `main` before versioning and tests the merged release again
before publishing. If additional `pull_request` checks are configured, GitHub
requires a maintainer to approve those runs on pull requests created with its
built-in token. See [GitHub's workflow triggering documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow#triggering-a-workflow-from-a-workflow).

## Local Inspection and Retry

Inspect the pending release with `npm run changeset -- status`. To preview the
generated version and changelog on a disposable branch or checkout, run
`npm run version-packages`; it updates files and consumes the pending changesets.
Normal contributions should leave versioning to the automated release pull request.

For a failed release, fix the cause and rerun the failed GitHub Actions job or
dispatch **Release** on `main`. For authentication failures, check the npm trusted
publisher fields, permission for direct publishing, the workflow's OIDC permission,
and package repository metadata. No npm token is needed for the retry.

If npm publication succeeded but tag or GitHub release creation failed, verify the
version on npm before retrying. Changesets skips an already published version;
restore the missing tag or GitHub release separately using its changelog.

## Container Images and Application Deployment

This workflow publishes the npm package. Container images are built from
`container/Dockerfile` and published separately, and applications own their Worker
and Container deployments. Coordinate compatible package and image versions when
changing the shared session protocol, and run the relevant `streamline-demo`
integration checks before rolling out those changes.
