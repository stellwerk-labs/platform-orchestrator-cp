# Orchestrator Module Version Management traceability

Assertion audit refreshed: 2026-09-08. Criteria refer to section 16 of the reviewed Orchestrator
Module Version Management specification, with the normative Resource Type
conformance specification for interface checks. This document summarizes the
cross-repository evidence for the Orchestrator release. It records the assertions
listed below; external services using the extension contracts need their own
integration evidence.

The Orchestrator owns Module history, lifecycle and Pins. Add-on boundaries are generic,
namespaced contracts and do not require an orchestration add-on to be present.

## Evidence index

Paths are repository-relative unless another component is explicitly named.

| ID | Exact tests and scope |
| --- | --- |
| Lifecycle | integration-tests/module_version_management_api_test.go::TestModuleVersionManagementLifecycleAPI: publication, sequential one-Proposed rejection, promotion, history/comparison, prerelease promotion rejection, successful stable graduation and reservation/archive interactions. |
| Atomicity | integration-tests/module_lifecycle_atomicity_test.go::TestStableGraduationFailuresLeavePrereleaseAndHistoryUnchanged and TestMultiModuleLifecycleTransactionRollsBackPartialChangesAndStaleCommands: real API/SQL snapshots, rejected commands leave no partial Version/pointer/event/receipt writes, corrected retry, exact replay and correlated transactions. |
| Replay | integration-tests/module_command_concurrency_test.go::TestConcurrentModulePublicationReplaysOneImmutableResult: concurrent same-key publication, one immutable result/event. |
| Boundaries | integration-tests/module_lifecycle_boundaries_test.go::TestDistinctConcurrentPublicationsCreateExactlyOneProposedVersion, TestDefectiveDefaultRestorationNeverSkipsTheExactDeprecatedPredecessor and TestConcurrentStableSuccessorCommandsCreateOneStableProposedVersion: distinct publication, exact predecessor restore and simultaneous stable-successor race coverage. |
| Domain | internal/moduleversions/domain_test.go: selected SemVer, lifecycle and Pin transition tables, not an exhaustive persisted state machine. |
| Compare | internal/api/module_version_management_test.go: structural key differences and typed before/after snapshots. |
| Pin persistence | integration-tests/module_version_management_api_test.go::TestEnvironmentModuleVersionPinPersistenceAndOperationLock: real SQL transitions/notes and operation ownership, using generated Deployment UUIDs rather than real Runner outcomes. End-to-end Runner outcomes for externally owned operations need separate integration evidence. |
| Callbacks | internal/api/module_version_management_test.go::TestPinOverrideReconciliationRequiresCurrentOwnedPendingState and TestPinRollbackRestorationRequiresCurrentOwnedOverriddenState: handler tests reject stale or incorrectly owned callback state; not real Runner outcome proof. |
| Conformance | integration-tests/module_conformance_test.go and internal/moduleconformance: immutable six-field Resource Type contract, explicit output-schema equality, fail-closed validation and graduation rejection. |
| Type concurrency | integration-tests/resource_type_concurrency_test.go::TestResourceTypeArchiveSerializesWithNewModuleBinding: PostgreSQL lock observation and rejection of late new binding. |
| Environment deletion | integration-tests/env_deletion_pins_test.go::TestEnvironmentDeletionTerminallyRemovesPinsExceptPendingOverrides: actual deletion removes active/overridden Pins into retained tombstones and blocks pending overrides without mutation. |
| Pin scope | internal/api/module_version_management_test.go::TestUnpinIsAuthorizedByScopeRatherThanPinCreator plus Data Plane integration-tests/module_pin_bulk_scope_test.go::TestBulkPinsUseFrozenDeployedVersionsAndRealScopedAuthority: scoped Unpin/Discard behavior is covered by both handler boundary and real scoped principals. |
| Migration | integration-tests/module_version_management_migration_test.go::TestModuleVersionManagementMigrationRoundTrip: populated previous-schema down/up migration preserves opaque history without invented SemVer/digests. |
| Catalogue | integration-tests/modules_test.go::TestDefinitions and integration-tests/resource_types_test.go::TestResourceTypesCrud: Provider reference validation, immutable Resource Types and archive/delete boundaries. |
| Empty deletion | integration-tests/module_version_management_api_test.go::TestEmptyModuleHardDeleteReleasesCreateIdempotency: same slug/key can recreate an eligible deleted identity with a new UUID. |
| Runtime | Data Plane integration-tests/legacy_module_execution_test.go::TestLegacyModuleExecutionAndHistoryRollback and module_pin_execution_test.go::TestManagedModulePinExecutionAndArchivedCarryForward: real Runner, legacy adoption/rollback, encrypted outputs and exact Pin/archive carry-forward. Record compatible revision/command results separately. |
| Lifecycle runtime | Data Plane integration-tests/module_lifecycle_execution_test.go::TestManagedModuleLifecycleExecutionBoundaries: implicit Proposed non-use, explicit Proposed permission, Deprecated rejection and exact scoped Defective carry-forward. |
| Bulk scope | Data Plane integration-tests/module_pin_bulk_scope_test.go::TestBulkPinsUseFrozenDeployedVersionsAndRealScopedAuthority: real deployments/scoped principals, frozen Environment set, stale preview and partial-authority no-write failures, successful Pin/replay/concurrent replay, later Environment non-inheritance, bulk Unpin, permission revocation, bulk Discard by another scoped principal, Discard partial-denial/replay/persistence and no infrastructure execution. Focused test and full Data Plane suite passed after the compatible Orchestrator image refresh. |
| Clients | CLI flags/parser tests; Console component/live lifecycle/comparison tests; Provider real Terraform lifecycle/import/retention plus direct Terraform/OpenTofu catalogue/Pin journeys. These prove their asserted flows, not all Orchestrator contracts. |

## Acceptance criteria

Status reflects the current cross-repo assertion audit. "Covered" means the
release-required Orchestrator criterion has direct assertions in at least one
product surface. Hardening notes are useful follow-up coverage, not release
blockers unless the product owner asks for stricter proof than the written spec.

| AC | State | Evidence and qualification |
| ---: | --- | --- |
| 1 | Covered | Lifecycle, Replay, Conformance, CLI/Provider/Console and real Runtime evidence cover immutable SemVer publication, Proposed/Unverified, explicit use and managed execution. Optional hardening: byte-for-byte same-SemVer mutation snapshot. |
| 2 | Covered | Lifecycle/domain/atomicity/client evidence covers lifecycle transitions and one-Proposed semantics; distinct publication and stable-successor races are direct tests. Exhaustive every-transition API table is hardening. |
| 3 | Covered | Lifecycle runtime proves implicit deploy keeps Default and explicit Proposed use requires the scoped permission. |
| 4 | Covered | Runtime rejects Deprecated/Defective normal forward use and permits only exact confirmed Defective carry-forward with capability. |
| 5 | Covered | SQL snapshots and client restore cycles cover atomic promotion/restoration pointers, retries and no partial writes. Arbitrary network/disk fault injection is not required by the spec. |
| 6 | Covered | Lifecycle/atomicity/client/provider evidence covers actor/reason/correlation and append-only event expectations. Hardening: one API matrix for every blank-reason/action pair. |
| 7 | Covered | CP API plus CLI, Provider and Console tests cover history semantics across product surfaces. |
| 8 | Covered | Comparison, Console before/after rendering, usage panels and runtime explicit-use permissions cover pre-promotion review. Hardening: one populated usage/adoption UI E2E before promotion. |
| 9 | Covered for the Orchestrator | Usage source/UI expose Environment/Pin links; runtime evidence gives real Deployment linkage. Navigation links populated by an external service need separate integration evidence. |
| 10 | Covered | Migration, Runtime and stock Helm upgrade preserve v0 identity/source/absent declarations, adopt managed v1 and support exact rollback. |
| 11 | Covered for the Orchestrator | Public neutral APIs and clients cover version/usage/event/atomic contracts without DB access. An external add-on service using every contract is reference integration. |
| 12 | Covered | The Orchestrator's lifecycle and navigation work without an add-on. Plugin uninstall/disable preservation is reference integration. |
| 13 | Covered | Resource Types now carry a six-field declarative `module_contract`; Module Versions carry explicit `output_schema`; conformance fails closed before Proposed/graduation. The normative contract is declaration validation, not external artifact execution. |
| 14 | Covered | Empty shell creation/recreation and first publication are covered. Hardening: direct DP no-version deployment denial. |
| 15 | Covered | Resource Type mutation/deletion conflicts, Module publication conformance and Resource Type archive/new-binding serialization cover immutable type binding and retained identity. |
| 16 | Covered | Runtime/Provider cover exact effective Pin/Unpin, Deprecated-active Pins, scoped principals and restricted Defective carry-forward permissions. |
| 17 | Covered | Direct and bulk Pin evidence covers preservation, exact records, no writes on partial denial/stale preview, replay/concurrent replay and audit actor checks. |
| 18 | Covered for the Orchestrator | CP validates operation-owned pending state and authoritative Deployment records for callback reconciliation; state-machine failure/cancel/success cases are covered. Runner outcomes for externally owned operations need separate integration evidence. |
| 19 | Covered | Real scoped Unpin by another actor, event actor/reason/boundary and unchanged deployment count are covered. |
| 20 | Covered | Retained published history, archive instead of delete, SemVer reuse barriers, Provider import and empty-shell residue absence are covered. |
| 21 | Covered | Defective Default clears the pointer and exact predecessor restore is enforced by direct API/SQL and client evidence. |
| 22 | Covered for the Orchestrator | CP requires current operation-owned `overridden` state and validates authoritative rollback Deployment target; rollback initiated by an external service needs separate integration evidence. |
| 23 | Covered for the Orchestrator | Operation ownership, pending locks, stale callbacks, terminal removed state, Environment deletion pending blocker and exposed bulk Discard are covered. Restart/out-of-order external callbacks remain reference integration. |
| 24 | Covered | Runtime proves Defective Pin persistence, exact confirmation and scoped capability requirements. |
| 25 | Covered | Publication floor, rollback/restore lineage and lower/stale rejection cases are covered. Build-metadata identity denial through every selector is hardening. |
| 26 | Covered | Data Plane bulk test covers frozen atomic multi-Environment Pin/Unpin/Discard, including successful replay/concurrent replay and real exposed Discard. |
| 27 | Covered | Deletion preview plus actual delete path cover active/overridden tombstones, pending blocker and unchanged state on conflict. Async DP destroy completion is broader Environment lifecycle evidence. |
| 28 | Covered | Prerelease promotion rejection, explicit Proposed use permission and stable-successor stable-target rules are covered. |
| 29 | Covered | Note boundary stability, restoration boundary changes, recreated Pin boundary and removed Unpin/Discard event retention are covered. Approval invalidation by an external service needs separate integration evidence. |
| 30 | Covered | Bulk runtime creates a later Environment, deploys it and asserts no inherited Pin. |
| 31 | Covered | No scheduler mutates Proposed by age; UI renders activity timestamps; Proposed persists until explicit transition. Hardening: clock-advanced long-lived Proposed test. |
| 32 | Covered | Optional notes, mandatory reasons, optional digest omission and no invented legacy digest/declaration are covered. Hardening: explicit release-note immutability after metadata/lifecycle update. |
| 33 | Covered | Archive cycles, exact existing Environment carry-forward, new Environment rejection and no infrastructure execution caused by Pin/archive operations are covered. |
| 34 | Covered | Pins persist through Default changes, archive, notes and Defective state, with scoped removal only and post-revocation replay denial. |
| 35 | Covered | New UUID after empty reuse, retained import identity and metadata/archive reference preservation are covered. Hardening: explicit API UUID/slug mutation denial. |
| 36 | Covered | Archive/unarchive plus archive/new-binding serialization and late binding rejection are covered; existing bound Modules retain Resource Type identity. Hardening: direct publish/promote existing-bound Module while Resource Type archived. |
| 37 | Covered | Reservation blocks archive, Draft does not and Defective safety transition is independent. Hardening: concurrent reservation/archive race. |
| 38 | Covered | Empty deletion removes old identity, catalogue events, lifecycle events and create receipt residue, then permits same slug/idempotency with a new UUID. |
| 39 | Covered | Stable graduation success, failure snapshots, conformance gate, corrected retry/replay and simultaneous competing successor commands cover atomicity. |
| 40 | Covered | v0 migration, managed v1 successor and normal managed lineage after v1 are covered. |
| 41 | Covered | Archived Pin persistence and real new Pin on archived effective version are covered; restricted new Environment/version-switch paths are rejected. |
| 42 | Covered | Handler and runtime evidence prove other scoped principals may Unpin/Discard, unauthorized scopes cannot mutate and actor/reason persist. |
| 43 | Covered | IAM scopes, legacy grants and runtime revocation checks prove operation-override rights do not grant direct Orchestrator Unpin/Discard. Delegated external-service identities need separate integration evidence. |
| 44 | Covered | Note append preserves Pin resource version/protection/boundary and removed-note rejection is covered. Approval preservation by an external service needs separate integration evidence. |

## Genuine remaining Orchestrator work

None identified in the audited AC1-AC44 Orchestrator release criteria after the
CP/Data Plane reruns and distinct scoped bulk Discard proof. External services
using the operation contracts need separate evidence for success, failure,
cancellation and rollback outcomes.

## Non-blocking hardening candidates

- One populated UI/E2E usage/adoption journey before promotion.
- Direct Data Plane no-version deployment denial for an empty shell.
- Explicit release-note immutability after lifecycle/metadata updates.
- Existing-bound Module publish/promote while Resource Type is archived.
- Concurrent reservation/archive race.

## Verification discipline

Run make generate, make lint, make test-unit, make test-integration and make
build against the compatible component set. Use the CI-pinned linter and Go
toolchain. Record each command, revision and result, including environment
failures. Never count unexecuted suites or mocked handlers as runtime proof.

The refreshed Orchestrator integration suite passed 300 tests in 9.188s with the
Resource Type archive/new-binding lock and stable-successor race tests included.
The refreshed Data Plane suite passed 501 tests in 117.663s, including real
scoped bulk Pin/Unpin/Discard coverage. These local/private gates do not replace
public source tags, public artifact digests, anonymous installation or final
publication approval.
