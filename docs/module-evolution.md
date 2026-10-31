# Module authoring and Resource Type versioning

Status: implemented and functionally qualified, not a released contract.
User decision: 30 October 2026. Published Module Versions remain immutable.

## Authoring contract

A Draft is a mutable authoring resource belonging to a stable Module identity.
It is separate from Proposed and does not reserve a published SemVer. Saving
uses optimistic concurrency and records the actor. Concurrent editors cannot
silently replace one another's changes.

Every actual test captures a complete immutable definition and exact external
source revision before planning. Editing the Draft cannot change an in-flight
test, historical Deployment, Pin or rollback snapshot. Test references must be
explicit; normal Module Rules, implicit resolution and Rollout planning cannot
adopt a moving Draft. Normal Environment authorization, queue/lease admission,
encryption, Runner execution and rollback remain authoritative.

Publishing freezes the exact reviewed Draft revision as a new Proposed Version
through the existing atomic publication path. It revalidates conformance,
permissions, the one-Proposed invariant, SemVer and source identity. A stale
revision or rejected publication changes neither Draft nor publication history.
Saving and testing never promote or deprecate a published Version implicitly.
Draft edits never rewrite a published Version, including its release notes.

This iteration does not introduce trusted artifact verification or a new
artifact-hosting service. Authors can change a pinned source revision between
tests without assigning another published Semantic Version to the Draft.

## Resource Type contract

Stable Resource Type UUIDs and technical IDs are distinct from immutable
interface versions. Existing built-in and organization-specific identities
receive their exact current interface as version 1, preserving missing versus
empty declarations, timestamps and scope. No historic actor is invented.

Every published Module Version must ultimately bind an exact interface-version
UUID belonging to its Module's stable Resource Type identity. Existing Module
Versions and retained Deployment snapshots keep their previous contract. New
versions are selected explicitly; introducing version 2 must not make old
clients, old Modules, queued work or rollback silently select it. A new version
does not make an incompatible resource graph valid or convert existing values.

Core, CLI, Terraform/OpenTofu and Console expose the same selection and history.
Provider state migration preserves ownership; no forced resource recreation is
used as a substitute for migrating the model. Plugin admission continues to
require published immutable targets and exact normal Deployment validation.

## Acceptance and traceability

| ID | Requirement | Implementation / evidence | State |
| --- | --- | --- | --- |
| RT1 | Backfill exact built-in/custom version 1 without scope collapse | migration 49, migration integration tests | PostgreSQL tests and seeded cross-service migration passed |
| RT2 | Immutable snapshot identities survive restart | migration 49, restart integration test | PostgreSQL and real CP/DP restart passed |
| RT3 | Existing Resource Type create produces version 1 atomically | insertion trigger, integration test | PostgreSQL and real API creation passed |
| RT4 | Destructive downgrade refuses retained interface history | migration 49 guard, atomic refusal test | PostgreSQL guarded downgrade passed; released binaries refuse new schemas |
| RT5 | Preserve unused version-1 catalogue deletion, retain later contracts | immutable trigger, integration test | PostgreSQL deletion and retention tests passed |
| RT6 | Bind existing Module Versions to their exact version-1 identity | migration 49, real contract-binding and model regression tests | Real seeded migration, deployment, queue and rollback passed |
| RT7 | Publish/read/list later versions with actor, CAS and idempotency | Core API, concurrent PostgreSQL writers and real HTTP command/history tests | Concurrent PostgreSQL commands and real human IAM/Console passed |
| RT8 | Conformance, planning, queued execution and rollback use exact contract | CP / DP / shared contracts and real execution | Real planning, queued execution and retained rollback passed |
| RT9 | Client/state compatibility and Console parity | generated API clients, CLI, provider, Console | CLI, Terraform/OpenTofu protocol/live tests and Console passed |
| D1 | Save and edit Draft with CAS and audit | migration 50, authoring API, concurrent real PostgreSQL tests | Real database, CLI, Terraform/OpenTofu and Console passed |
| D2 | Explicit immutable test snapshot through normal deployment path | migration 51, scoped capture/read API, immutable PostgreSQL/HTTP tests; CP / DP / Runner execution | Real scoped capture, admission, queue and Runner execution passed |
| D3 | Old/in-flight tests unchanged after editing | concurrency and retained rollback tests | Real A/B capture, later C edit, queued execution and retained rollback passed |
| D4 | Publish exact reviewed revision atomically, retain lifecycle rules | real HTTP stored-revision publication, stale review, one-Proposed and rollback tests | Real stored-revision publication through API, provider and Console passed |
| D5 | Draft never becomes implicit Default, Pin target or Rollout target | negative admission tests | Pin, published-target, add-on graph/artifact and implicit-selection refusals passed |
| D6 | API, CLI, provider and Console authoring parity | generated clients and end-to-end tests | Generated contracts and real CLI/provider/Console execution passed |
| Q1 | Upgrade seeded pre-change database and restart safely | real migration/restart tests, complete PostgreSQL model suite | Seeded released-schema upgrade, mixed versions, restore, restart and rollback passed with native binaries; final image/chart set pending |
| Q2 | Documentation, upgrade guide, release notes and examples | documentation repositories / component notes | Guides, references, troubleshooting, upgrade notes and Unreleased changelogs prepared; final versions and publication pending |
| Q3 | Existing plugin and ordinary deployment acceptance | explicit rehearsal after local qualification | Ordinary mixed-version deployments passed; deployed commercial-plugin end-to-end qualification remains pending |

Migration 49 and the Core API bind historical Modules to version 1 and allow
explicit later interface selection. Old callers still select version 1, and
legacy catalogue reads retain that interface. The functional CP/DP/client/Console
paths are qualified. Final standalone public SDK availability, packaged upgrade
and plugin-compatible release qualification remain. Public release approval is
a separate step.

## Design references

The implementation keeps domain interface versions distinct from concurrency
revisions, API versions and storage formats. Kubernetes documents these separate
concerns in its [CRD versioning guide](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/).
This is a design comparison, not a proposal to turn Resource Types into CRDs.
Provider upgrades follow the existing framework's
[state upgrade contract](https://developer.hashicorp.com/terraform/plugin/framework/resources/state-upgrade).

## Implemented authoring boundary

The authoring API lives under `/orgs/{orgId}/modules/{moduleId}/drafts`.
POST creates the single open Draft for a Module. PUT to its UUID saves a new
revision with `expected_resource_version`, `definition` and a reason. Reads
include retained history at `/{draftId}/revisions/{draftRevision}`. Each edit
preserves earlier content, actor and reason. The UUID never acts as an
executable or published Module Version, and the intended SemVer is not reserved.

POST `/{draftId}/publish` accepts only a reason and the reviewed resource
version. It reads the stored definition, revalidates publication and conformance,
publishes Proposed and closes the Draft in one transaction. Invalid input, a
stale review or an existing Proposed changes neither publication nor authoring
history. A published Draft cannot be edited again. Create another Draft to
continue authoring. POST `/{draftId}/discard` closes an abandoned Draft without
erasing its revisions. Closed history remains readable and prevents hard
deleting the Module; archive the Module instead.

Authoring writes and publication require existing `module.version.publish`
authority; reads use `module.version.read`. Every mutation requires an
idempotency key and reauthorizes even a cached replay. No new IAM role or SDK
dependency is introduced. An idempotent replay returns the original command
receipt; GET returns the current Draft.

Drafts may save an external source before its exact revision is selected. Real
test execution and publication require the usual source identity validation.
Saving through the authoring API does not execute a test. Capture and explicit
deployment are separate operations. An immutable revision is evidence
of an edit, not a deployment result or a trusted artifact verification.

## Exact publication identities and older servers

Resource Type publication names the exact next interface number, alongside the
separate catalogue concurrency revision. Version1 is initial catalogue state;
new publications start at2. Skips, duplicate identities and stale catalogue
revisions are rejected before allocating a new interface. This keeps creation
retries and Terraform import/ownership tied to one intended version.

Direct Module publications with an explicit interface use
`/orgs/{orgId}/modules/{moduleId}/version-publications`; stable graduation uses
`/orgs/{orgId}/modules/{moduleId}/versions/{moduleVersionId}/successor-publications`.
Both reuse the existing atomic publication commands. CLI/provider clients do
not fall back to old handlers if those routes are absent: older handlers could
otherwise ignore the new field and publish against version1. Ordinary existing
version1 calls keep their original routes. Modern publication input rejects
unknown fields.
