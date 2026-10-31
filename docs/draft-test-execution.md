# Immutable Draft test execution

Status: capture/storage, catalogue, admission, queue and Runner execution are
implemented and functionally qualified against real CP/IAM/DP/Runner services.
Final packaged upgrade and plugin-compatible release qualification remain.
The accepted authoring requirements are in `module-evolution.md`.

## Identity and storage

A test executes a separate immutable snapshot of one stored authoring revision.
Its Module UUID remains the normal stable Module UUID. Its executable snapshot
UUID is distinct from the Draft UUID and every published Version UUID. The
intended SemVer is descriptive authoring metadata, not a reserved release.

Keep snapshots outside `definition_versions`. Do not insert temporary Proposed
prereleases, consume the one-Proposed slot, label snapshots Deprecated, or clone
the Module under another identity. Those shortcuts would change published
history or bypass identity-based protection.

The snapshot must retain its organization, Module UUID, Draft UUID/revision,
complete definition, exact Resource Type interface UUID, Project/Environment
UUIDs, source identity, actor, reason and capture time. The authoring revision
and contract must exist in the same transaction. Snapshot content and identity
cannot be edited or deleted after capture. Editing, discarding or publishing
the Draft must not invalidate captured work or retained rollback material.

A captured definition is not a successful test. Execution outcomes belong to
ordinary Deployments; creation of a snapshot cannot manufacture an execution
result, licence entitlement or trusted artifact-verification result.

## Capture boundary

Capture requires the exact reviewed authoring revision, Module publication
authority and Environment deployment authority. Resolve the Project and
Environment server-side; do not trust unrelated caller-supplied UUID pairs.
Revalidate the complete definition and exact interface conformance before
capturing. An unresolved external source cannot execute. A source-revision
label alone must not turn a moving source URL into frozen executable content.
For the first implementation, accept inline source or explicit `git::https://`
and `git::ssh://` sources whose sole `ref` query selects a full commit matching
`source_revision`. Reject moving refs, shortened hashes, shallow-clone options,
local paths and unqualified archive/registry URLs. Keep digest optional.
Terraform forwards `ref` to Git; `depth` changes checkout behavior and cannot
be used with a raw commit in this contract. See the [official module source
reference](https://developer.hashicorp.com/terraform/language/modules/sources).
Digest remains optional; declaring a digest is not trusted content verification.

Capture does not claim an Environment execution lease. Ordinary admission
obtains that lease and reevaluates current protection at actual execution.
Use the existing transactional command receipts and outbox, including permission
rechecks on replay. Events contain identifiers and reasons, never source,
inputs, outputs, credentials or complete definitions.

## Catalogue and admission

Add explicit executable provenance to the internal catalogue. A Draft test has
no published lifecycle status or published SemVer. Keep the public published
lifecycle enum unchanged; internal executable metadata can distinguish
`published` from `draft_test` without pretending that a snapshot is Proposed.

Ordinary rules and published-version list/read APIs must not return tests.
Pins and add-on targets must continue to resolve only published Version UUIDs.
The explicit deployment selection must name immutable test snapshot UUIDs,
never a moving Draft ID or an unqualified authoring revision.

The internal catalogue can overlay an explicitly selected test on the same
Module's rules only after verifying organization, Module and exact Environment
binding. Reject a conflicting selection for the same Module. Handle Modules
without a Default, so testing the first implementation does not require a
dummy published Version. Rule creation uses the stable catalogue entry, not a
Default definition. A rule for an unpublished Module still cannot compile an
ordinary deployment without an explicit test selection. Retained exact test coordinates may be reconstructed
only for the bound Environment's genuine carry-forward, destroy or rollback.

Central admission must recognize that executable provenance explicitly. Extend
its canonical intent with an exact test-selection subset of resolved Module
targets. Omitted selection must never make an unknown UUID a test implicitly.
Keep old canonical intent/digests unchanged when the new field is absent.

For fresh tests, reauthorize Module authoring/test authority at actual admission
as well as normal scoped deployment authority. Do not trust a client boolean or
an old capture permission. Preserve the existing exact carry-forward and
rollback rules for already-authorized retained execution.

Run all ordinary Pin checks against the stable Module UUID before admitting a
test. A test is not a Pin override. Keep Environment admission epochs, queue
serialization, leases, plugin reservations and completion receipts authoritative.
The plugin target-validation boundary must reject a test UUID independently of
any UI filtering or catalogue label.

## Data Plane, queue and Runner

Carry explicit immutable test selection through public request validation,
idempotency fingerprints, queued intent, graph construction and durable artifact
requirements. Queued execution must keep its captured snapshot when the author
edits the Draft. The planner must not re-read its latest authoring head.

Use the current generated Control Plane client. The catalogue overlay must
provide the same provider/dependency/co-provisioning inputs as a published
definition. Persist executable provenance, exact snapshot UUID and interface
binding with the graph and bundle material. Reject conflicting reconstruction
metadata; do not substitute today's Draft or Default.

Use the existing encrypted outputs/logs, IaC generation, bundle delivery,
Runner execution and result processing. Do not create another execution engine.
Historical snapshots must survive Runner restart, failed deployment recovery,
queue promotion and rollback without requiring an open Draft.

## Required verification before enabling execution

- Execute snapshot A, edit the Draft to B while A is queued/running, and prove
  A's graph, bundle, source, output contract and outcome still belong to A.
- Run B under the same intended SemVer; published history and the one-Proposed
  slot remain unchanged. Publish a separately reviewed exact stored revision.
- Reject stale captures, moving external sources, cross-org/Module/Environment
  snapshot reuse, revoked authority and simultaneous contradictory selections.
- Reject test UUIDs as ordinary published selections, Pin targets and plugin
  rollout targets, including attempts to forge executable provenance.
- Reject a test that would violate an active or pending Pin, even with a stale
  graph, queue delay or plugin operation in flight.
- Test a new Module without a Default, ordinary published deployments alongside
  tests, retained destroy, rollback, restart and retry after ambiguous outcomes.
- Verify real IAM, PostgreSQL, Data Plane, queue and Runner components. Pure
  metadata tests or an HTTP substitute cannot establish execution conformance.
- Exercise CLI, Terraform and OpenTofu, Console and upgrade from the released
  schema. Preserve existing provider state and historical deployments.

Only after those checks should the UI or clients present capture-and-execute as
a working test action. Authoring CRUD and a stored snapshot alone are partial
implementation.

## Implemented capture boundary

Migration 51 retains `module_draft_test_snapshots` separately from published
Versions. The storage trigger checks the reviewed Draft revision and exact
content, Module/contract binding and live Environment scope. Updates and deletes
are forbidden; downgrade refuses retained snapshots. Environment UUIDs are
historical coordinates, so snapshots survive ordinary Environment deletion and
cannot be reused by a new Environment with the same slug.

`POST /orgs/{orgId}/projects/{projectId}/envs/{envId}/modules/{moduleId}/drafts/{draftId}/test-snapshots`
captures the reviewed revision with a reason and command key. It requires Module
publication and scoped deployment-write permission, both checked before receipt
replay. The resolved Environment UUID is part of command scope. Replacement
source in the request is rejected. Capturing does not acquire an execution
lease, reserve SemVer, change a Default or report a successful test.

`GET /orgs/{orgId}/projects/{projectId}/envs/{envId}/modules/{moduleId}/draft-test-snapshots/{snapshotId}`
requires Module read and scoped deployment-read permission. It returns only the
exact scoped immutable definition with `provenance: draft_test`, without a
published lifecycle status. Published-version APIs do not resolve snapshot UUIDs.

Real PostgreSQL and validated HTTP tests cover A-to-B authoring, replay, stale
capture, a recreated Environment slug, retained closure/restart, wrong scopes,
contract mismatch, moving source, immutable storage and guarded downgrade.
Real execution, queued A/B selection after a later edit, current permission
rechecks, retained rollback and normal Environment teardown also passed. This
functional proof does not qualify a different final image/chart release set.

## Implemented CP admission boundary

The internal admission request accepts `draft_test_snapshots` only as an exact
subset of resolved Module targets. Snapshot UUIDs do not become published
Versions by omission or lookup fallback. Empty selection is omitted from the
canonical intent, preserving historical intent bytes and receipt digests.

At actual admission, the CP rechecks scoped deployment authority and, for fresh
tests, current Module publication authority. Normal stable-Module Pin checks
run before test resolution. Snapshot reads verify exact Module, organization,
Project and Environment binding. Carry-forward and rollback without fresh
authoring authority additionally require a prior successful admission for that
exact snapshot in the same Environment. Capturing material is insufficient.

Real PostgreSQL tests exercise missing/revoked fresh policy, replay, forged
selection/scope, prior-success evidence and active Pins. These establish CP
protection behavior, not full IAM/DP/Runner execution qualification. The Data Plane request, queue and bundle paths carry exact test provenance.
Full CP/IAM/DP/Runner qualification remains required before presenting this as
a released deployment feature.

## Additive executable catalogue

The existing `/internal/orgs/{orgId}/projects/{projectId}/envs/{envId}/module-catalogue`
endpoint and its generated published-module DTO remain unchanged. The new
`module-execution-catalogue` endpoint uses separate generated types, so existing
SDK callers do not acquire pointer-typed lifecycle fields or a new lifecycle
value. It authenticates the configured Data Plane admission principal on every
request. Internal material lookup grants no execution authority; current actor
permissions are still checked at actual admission. This also permits trusted
bundle reconstruction without depending on an author's later login session.

A test-aware request explicitly maps stable Module UUIDs to immutable snapshot
UUIDs. The planner loads only those exact records in the requested Environment,
checks Pins even when an add-on planning flag is present, and overlays the
Module's existing rules before loading published Defaults. A first Module can
therefore be tested without a dummy publication. Ordinary Rules without a
published Default produce a conflict; they never select a Draft implicitly.

The execution DTO carries `draft_test` with its frozen definition and scoped
capture metadata. Published lifecycle, SemVer, migration and verification fields
are absent for tests. Published entries retain their established wire values,
including historical empty SemVer. The DP validates identity, scope coordinates,
source, inputs, providers, dependencies, output contract and parameter types
against the frozen definition before creating graph configuration.

The Data Plane uses the additive execution DTO for explicitly selected and
retained test material. Ordinary published requests keep their established
transport. Fresh test requests use a separate `module-test-deployments` route;
older servers must reject that route rather than ignore a test selection.
Queue promotion retains immutable snapshot coordinates and rechecks authority.
A Runner execution rehearsal remains outstanding.

### Retained sources and explicit published selection

`retained_draft_test_snapshots` lists exact Module/snapshot pairs needed by a
stored graph or bundle. These entries receive no new rules. Selecting B for a
new deployment can therefore retain A's exact sources for existing resources
without allowing A to replace B's selection.

A trusted runtime can request `material_only` with `are_rules_ignored` to read
immutable material after current Pins or catalogue lifecycle have changed.
This read forbids fresh selections and grants no deployment permission. Actual
admission still enforces current Pins, scoped permissions and retained-success
evidence. An ordinary planning request cannot use this flag to bypass admission.

`selected_published_module_versions` is distinct from retained material. It
selects an exact published Version before Default lookup, including a first
Proposed Version in a Module without a Default. A contradictory Environment
Pin is rejected; the selected published definition is loaded in full, rather
than using only its lifecycle metadata.

An active Pin is also an exact published selection. The catalogue loads that
pinned definition before looking for Defaults, including when the protected
Version is the first Proposed Version and the Module has no Default. Reading
or selecting it does not promote it or create a Default.

Before a Module has a Default, a new Rule must name both `project_id` and
`env_id`. The catalogue resolves every matching Rule before graph construction;
an unresolved global Rule would otherwise block unrelated deployments. Scope
first-implementation tests to their test Environment, then publish a Default
before creating broader Rules. Within that Environment, every matching Rule
still needs a published Default, Pin, or explicit immutable selection. A moving
Draft is never selected implicitly.
