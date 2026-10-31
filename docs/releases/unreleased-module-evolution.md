# Module authoring and Resource Type interfaces

Unreleased. Requires the coordinated server, clients, Console and gateway routes.

Module Drafts separate editing from publication. Save several implementations
under one intended SemVer, inspect earlier revisions, capture a reviewed revision
and explicitly deploy that immutable snapshot through the normal Runner. Saving
does not run a deployment or reserve SemVer. Editing leaves earlier captures,
queued work and retained deployments unchanged.

Publication creates a separate immutable Proposed Version from the exact stored
revision and closes the Draft. Promotion remains a separate action. Published
Proposed, Default and historical Versions remain immutable. Discard and archive
retain history. Artifact digests remain optional and do not establish trusted
content verification.

Resource Types keep a stable identity and gain immutable interface versions.
Historical global and organization-specific contracts become version 1 with
exact original content and timestamps. Existing Module Versions retain that
binding. Publishing interface 2 does not move existing Versions or change the
interface selected by older callers that omit the number.

New commands use existing permissions, optimistic concurrency, reasons and
transactional idempotency. Fresh tests recheck authoring and scoped deployment
authority at admission. Pins remain authoritative. A Module without a Default
can be tested using a Rule scoped to both Project and Environment.

## Upgrade impact

CP migrations 49, 50 and 51 retain interface versions, Draft revisions and test
snapshots. DP migrations 36 and 37 retain exact execution provenance. Upgrade
the compatible DP before enabling the new CP workflows, then the Console.
Older CP/DP binaries refuse the new schemas. Use schema-compatible recovery
images or a forward fix; a database restore is a separate recovery operation.
See `../module-evolution-upgrade.md`.

The new generated SDK adds methods to existing client interfaces and therefore
uses the planned shared/v3 path. Its nested module must be separately qualified
and published before consumer image builds.
New clients use capability-specific routes and never substitute a published
Default when a test endpoint is unavailable.

Functional qualification covers real CP/IAM/DP/Runner execution, CLI,
Terraform/OpenTofu, Console, immutable A/B captures, queued admission, retained
rollback, normal teardown and seeded cross-service upgrade/restart. Public SDK
availability, final image/chart qualification and the complete plugin-compatible
release set remain release gates. These notes do not announce a publication.
