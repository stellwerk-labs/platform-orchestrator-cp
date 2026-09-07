# Core Module Version Management traceability

Assertion audit: 2026-09-07. Criteria refer to section16 of the reviewed Core
Module Version Management specification. This is an implementation/evidence gap
record, not a claim that every criterion is complete. Passing a suite does not
prove behavior absent from its assertions.

Core owns Module history, lifecycle and Pins. Add-on boundaries are generic,
namespaced contracts and do not require an orchestration add-on to be present.

## Evidence index

Paths are repository-relative unless another component is explicitly named.

| ID | Exact tests and scope |
| --- | --- |
| Lifecycle | integration-tests/module_version_management_api_test.go::TestModuleVersionManagementLifecycleAPI: publication, sequential one-Proposed rejection, promotion, history/comparison, prerelease promotion rejection, successful stable graduation and reservation/archive interactions. |
| Atomicity | integration-tests/module_lifecycle_atomicity_test.go::TestStableGraduationFailuresLeavePrereleaseAndHistoryUnchanged and TestMultiModuleLifecycleTransactionRollsBackPartialChangesAndStaleCommands: real API/SQL snapshots, rejected commands leave no partial Version/pointer/event/receipt writes, corrected retry, exact replay and correlated transactions. |
| Replay | integration-tests/module_command_concurrency_test.go::TestConcurrentModulePublicationReplaysOneImmutableResult: concurrent same-key publication, one immutable result/event. |
| Boundaries | integration-tests/module_lifecycle_boundaries_test.go::TestDistinctConcurrentPublicationsCreateExactlyOneProposedVersion and TestDefectiveDefaultRestorationNeverSkipsTheExactDeprecatedPredecessor: newly added and compile-checked, execution pending. |
| Domain | internal/moduleversions/domain_test.go: selected SemVer, lifecycle and Pin transition tables, not an exhaustive persisted state machine. |
| Compare | internal/api/module_version_management_test.go: structural key differences and typed before/after snapshots. |
| Pin persistence | integration-tests/module_version_management_api_test.go::TestEnvironmentModuleVersionPinPersistenceAndOperationLock: real SQL transitions/notes, but generated Deployment UUIDs, not real Runner outcomes or scoped IAM. |
| Pin scope | internal/api/module_version_management_test.go::TestUnpinIsAuthorizedByScopeRatherThanPinCreator: mocked handler boundary, not a real multi-user journey. |
| Migration | integration-tests/module_version_management_migration_test.go::TestModuleVersionManagementMigrationRoundTrip: populated previous-schema down/up migration preserves opaque history without invented SemVer/digests. |
| Catalogue | integration-tests/modules_test.go::TestDefinitions and integration-tests/resource_types_test.go::TestResourceTypesCrud: Provider reference validation, immutable Resource Types and archive/delete boundaries. |
| Empty deletion | integration-tests/module_version_management_api_test.go::TestEmptyModuleHardDeleteReleasesCreateIdempotency: same slug/key can recreate an eligible deleted identity with a new UUID. |
| Runtime | Data Plane integration-tests/legacy_module_execution_test.go::TestLegacyModuleExecutionAndHistoryRollback and module_pin_execution_test.go::TestManagedModulePinExecutionAndArchivedCarryForward: real Runner, legacy adoption/rollback, encrypted outputs and exact Pin/archive carry-forward. Record compatible revision/command results separately. |
| Bulk scope | Data Plane integration-tests/module_pin_bulk_scope_test.go::TestBulkPinsUseFrozenDeployedVersionsAndRealScopedAuthority: real deployments/scoped principals. Initial execution exposed broken exact bulk replay. Expanded assertions and corrective implementation await re-verification. |
| Clients | CLI flags/parser tests; Console component/live lifecycle/comparison tests; Provider real Terraform lifecycle/import/retention plus direct Terraform/OpenTofu catalogue/Pin journeys. These prove their asserted flows, not all Core contracts. |

## Acceptance criteria

Partial means implementation and some directly inspected evidence exist.
Gap means proof or a product contract is missing. No status is inferred solely
from a function name or an implementation comment.

| AC | State | Existing evidence and remaining obligation |
| ---: | --- | --- |
| 1 | Partial | Lifecycle publishes Proposed/Unverified; Runtime executes unverified versions. Prove changed-byte SemVer republication preserves history and explicit Proposed use. |
| 2 | Partial | Domain guards, sequential one-Proposed and Replay. Boundaries distinct-key competition awaits execution; exhaustive persisted transitions remain. |
| 3 | Partial | Exact selection and Default-only implicit resolution exist. Record direct Proposed positive/implicit-negative runtime assertions. |
| 4 | Partial | Catalogue safety guards and retained-version Runtime path. Prove all prohibited Deprecated/Defective forward paths with real authorization/outputs. |
| 5 | Partial | Atomicity proves multi-Module rollback, retry, pointers and correlated events. Competing target/restore transactions remain. |
| 6 | Partial | Lifecycle events; Atomicity rejects blank reason and proves failure has no writes. Complete actor/reason/append-only API assertions remain. |
| 7 | Partial | API/generated clients and history/lifecycle surfaces. Complete real CLI and populated Console history/usage/Pin journeys remain. |
| 8 | Partial | Compare and live Console before/after values. Populated adoption and role-scoped pre-promotion review remain. |
| 9 | Gap | Usage and extension links exist. Populated effective/historical Deployment and add-on navigation/access assertions remain. |
| 10 | Direct combined evidence | Migration preserves real previous-schema opaque v0; Runtime executes v0, adopts v1 and rolls back. Require compatible revision-set execution records. |
| 11 | Partial | Lifecycle exercises reservation/contribution APIs and atomic commands. A separately scoped add-on API-only journey remains. |
| 12 | Partial | Core-only runtime/client journeys and absent add-on navigation. Explicit availability-state and install/disable preservation proof remains. |
| 13 | Product-contract gap | Resource Types declare only output_schema, not required-input/parameter/provider/dependency contracts. Module Versions have no declared output interface. Syntax/reference checks cannot establish full pre-publication conformance for unverified external artifacts. An explicit interface/product decision is required. |
| 14 | Partial | Empty identity creation/publication. Direct no-version deployment/resolution rejection remains. |
| 15 | Partial | Permanent type binding/reference retention. Explicit attempted rebind/identity reuse with unchanged persisted rows remains. |
| 16 | Partial | Runtime/Provider exact effective-version Pins. Bulk scope adds Deprecated-active and real scoped-principal assertions, execution pending. |
| 17 | Partial | Runtime preserves Pins; Pin persistence notes/events. Real bulk/scoped audit journey pending. |
| 18 | Partial | Pin persistence success/failure/cancel and operation locking. Generated Deployment IDs do not prove authoritative runtime outcomes/recovery. |
| 19 | Partial | Provider audited Unpin. Distinct actor/event and no implicit infrastructure execution added to Bulk scope, pending. |
| 20 | Partial | Retained-history conflicts and Provider import preserve identity; Empty deletion works. SemVer reuse and non-Version blockers need direct negatives. |
| 21 | Partial | Restoration/Defective guards exist. Boundaries exact-predecessor positive/negative API+SQL assertions await execution. |
| 22 | Gap | Model restoration uses generated IDs. Real exact successful rollback and failure/mismatch/permanent-removal negatives remain. |
| 23 | Partial | Pin persistence rejects wrong operation, restores active on failure/cancel. Real reconciliation/restart/Discard and stale callbacks remain. |
| 24 | Partial | Defective confirmation/capability guards. Full real runtime acceptance, including asynchronous bundle compilation, must pass. |
| 25 | Partial | Domain SemVer and Atomicity out-of-order publication above Default. Persisted equal/build-equivalent/lower rejection after lineage changes remains. |
| 26 | Partial, defect under repair | Frozen UUIDs/transactions. Initial real bulk test found successful same-key replay incorrectly rejected as stale. Re-verification and real positive Discard remain. |
| 27 | Gap | Mocked deletion-impact pending blocker. Actual deletion/tombstones and concurrent Pin/deletion boundaries remain. |
| 28 | Partial | Domain syntax, prerelease promotion rejection and atomic graduation. Direct explicit use and all Default-producing paths need proof. |
| 29 | Partial | Pin persistence/Provider activation preservation/restoration boundary. Historical boundary after terminal removal and add-on approval checks remain. |
| 30 | Gap | Bulk persists snapshots. New real later-Environment/no-inherited-Pin assertion awaits execution. |
| 31 | Gap | No automatic expiry path observed. Time/restart nonmutation and age/inactivity display need assertions. |
| 32 | Partial | Optional notes and reason guards. Immutable notes and no invented publication reason require API/persistence checks. |
| 33 | Partial | Archive cycles and Runtime carry-forward/new-adoption denial. Complete archived publication/promotion/switch/rollback and no implicit execution remain. |
| 34 | Partial | Runtime Pins survive Default/archive. Creator-right revocation, elapsed time and Defective lifecycle must not remove protection implicitly. |
| 35 | Partial | Empty deletion/new UUID and Provider metadata/import preservation. Explicit identity mutation rejection and populated reference stability remain. |
| 36 | Partial | Resource Type archive/retention. New-binding denial versus continued existing-Module publish/promote/deploy needs a real journey. |
| 37 | Partial | Reservation blocks archive, Draft does not. Defective independence, reservation/archive race and owner/expiry tests remain. |
| 38 | Partial | Empty deletion permits same slug/key/new UUID. Explicit no old row/tombstone/event/residual command assertions remain. |
| 39 | Strong failure evidence, contract gap | Atomicity proves complete rollback, including uniqueness failure after provisional deprecation; retry/replay succeeds. Concurrent successor tests and AC13 contract remain. |
| 40 | Partial | Migration/Runtime establish v0 to v1. Post-migration lineage and restoration-to-v0 comparison boundaries remain. |
| 41 | Partial | Runtime/archive and Provider Pin on effective archived version. Wrong-version/new-Environment Pin and archived switch negatives remain. |
| 42 | Partial | Pin scope mocked other-creator case. Real other-creator Unpin/audit added to Bulk scope, pending. Positive Discard and all inherited scopes remain. |
| 43 | Partial | IAM separates Unpin/override/restore. Real override-only denial and revoked-right replay added, pending. Add-on namespace isolation also needs verification. |
| 44 | Partial | Pin persistence/Runtime/Provider preserve note state/version/boundary. Bulk scope adds unauthorized/removed denial, original reason, prior-preview validity and distinct actor, pending. |

## Verification discipline

Run make generate, make lint, make test-unit, make test-integration and make
build against the compatible component set. Use the CI-pinned linter and Go
toolchain. Record each command, revision and result, including environment
failures. Never count unexecuted suites or mocked handlers as runtime proof.

The two Atomicity tests and complete integration package containing them passed
against real services/SQL. Newly added Boundaries and expanded Bulk scope tests
must run after the compatible candidate is rebuilt. Pending states and the
unresolved product contract remain explicit until evidence changes.
