# Manual release candidates

The normal main-branch release remains unchanged. A separate manual CI path can
publish an explicitly approved, already tagged release candidate without updating
stable/latest channels. It is enabled only in the original `stellwerk-labs`
repository, not forks or private mirrors.

## Before dispatch

- The manual workflow must first be available on the default branch. Transfer
  workflow/helper/docs changes separately from the feature through an approved
  workflow-only change. Use `[skip release]` on every bootstrap commit and its
  merge/squash message, and verify no other pending main commits would release.
  An ordinary `ci:` commit can trigger a stable patch in the existing configuration.
- Obtain the required source/tag/registry publication approvals. Adding this
  workflow does not grant permission to publish.
- Finish all application and migration gates on the candidate revision. Include
  reviewed notes at `docs/releases/<candidate-tag>.md` in that same revision.
- Create the approved tag separately. Its canonical format is `vX.Y.Z-rc.N`, with
  `N >= 1`. Record the full 40-character commit SHA, not a branch name or short SHA.
- An administrator must first configure `public-release-candidate` with required
  reviewers. Restrict the allowed deployment branches/tags according to the
  repository's release policy. The preflight refuses a missing environment or
  one without reviewers; the workflow does not provision it.
- Confirm the established GHCR package is public and anonymously readable. The
  candidate path refuses an existing image tag and fails closed on ambiguous
  registry or authorization errors. It does not change package visibility.

## Dispatch and outputs

Select the CI workflow's manual dispatch with `candidate_tag` and `candidate_sha`.
All existing test jobs check out the supplied SHA. Preflight and the protected
publication job verify that checkout, tag and approved SHA agree. The protected
job must receive the configured environment review before it runs.

The candidate job reserves a GitHub prerelease using the existing tag, explicitly
leaves GitHub latest unchanged, and publishes only the exact RC image tag for
Linux amd64 and arm64. Build provenance, an SBOM and the source revision label are
included. The final step reports the immutable image digest. Independently verify
anonymous pulls, architecture manifests and the coordinated component set before
advertising the candidate. Nested `shared/` Go module tags remain separate release
units; a root application tag does not publish them.

No Git tag is created or moved by this workflow, and semantic-release is not
invoked by manual dispatch. Stable publication still follows the normal reviewed
main-branch release process.

## Partial publication and recovery

If image publication fails after the GitHub prerelease is created, treat that RC
identifier as consumed. Inspect the failure and prepare a newly approved `rc.N+1`
with updated notes. Do not rerun publication in order to overwrite an existing
candidate, delete its evidence or move its tag. A partially published candidate is
not a usable release.

## Local checks

Run `PYTHONDONTWRITEBYTECODE=1 python3 scripts/candidate-release_test.py`.
These standard-library tests validate identity, reviewer requirements, registry
absence handling and workflow separation. They do not publish artifacts or prove
that remote environment protection, credentials or registry access are configured.
