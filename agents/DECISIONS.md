# Decisions

- Add a nonpublishing manual mode to the existing CI workflow, reusing all four
  source gates rather than duplicating them. Require an exact existing tag/source,
  authenticated signature/ref checks, matching workflow bytes and an explicitly
  reviewed nonempty history base. Exclude both publishers before any write job.
- Preserve the automatic stable tag owner. Lightweight stable tags are exact
  authenticated ref bindings, not signed-tag evidence; RCs and annotated stable
  tags still require verified tag signatures.

- Preserve the complete pinned Distroless base and BuildKit SBOM/provenance.
- Reproducibility compares actual platform payload manifests/configurations/layers/binaries/notices. Attestation and full index variance is recorded, not suppressed.
- Collect production notice graphs separately for each Linux architecture and all shipped executables. Restrict SDK classifier exceptions to exact declared module identities and verified legal bytes/source provenance.
- The application's classifier prefix may cover only its own root module and exact shared SDK from the same source checkout. Other similarly prefixed module identities fail; the own SDK receives the actual source root LICENSE explicitly.
- The sole binary false-positive policy is bound to an exact reviewed finding, adjacent source literal hashes, ELF architecture, dependency version/checksum, no replacement and Go toolchain metadata. Ordinary-file scanning has no such exception, and artifact-owned ignore controls are rejected.

## Exact public reciprocal-source scan parity

- Reuse the reviewed DP seven-file policy without broadening any identity,
  version, checksum, complete-file hash, logical path or finding-set boundary.
  Preserve source/legal bytes and strict metadata, binary and path scans.
- This isolated scanner is an independently pinned qualification tool. The
  existing CP image retains its actual e8e5ace source identity; new scanner
  commits do not authorize relabeling or claiming a different public release.

## Canonical Environment deletion policy, CP54 (2026-11-03)

Persist a constrained nullable force/delete-rules pair without backfill. Public
DTOs expose one optional read-only object. Explicit DELETE merges true flags by
OR and writes status/outbox in one transaction. Destroy-only admissions lock the
row and recheck current policy on new and replayed requests. Existing authorized
work may finish after escalation. Final deletion uses canonical rule policy and
can bind the lifecycle UUID. Unknown deleting/delete_failed rows require explicit
re-deletion even for unbound internal callers; only active/unbound trusted direct
force deletion retains its legacy fallback. No DP policy table or plugin change.

## Installation settings and Provider retirement

- Retain the breaking Provider retirement DELETE202 contract and major application version. Require complete compatible managed-state proofs before physical deletion. Mirror installation maintenance inside effect transactions and admit only exact package upgrade operations while paused.


## 2026-11-12: qualified release handoff pilot

- One read-only producer retains both complete builds and all artifact qualification, then seals the original OCI archive and evidence for the existing RC and stable publishers.
- Preserve source, workflow, event and original producer-attempt identities across publisher-only reruns. Existing publication protections, collision refusal and cryptographic/anonymous checks remain.
- Reject a stale stable publisher before reservation when a newer stable release is already latest. Preserve existing concurrency; no additional lifecycle lock or release framework.


## 2026-11-12: CP catalogue summaries and database TLS

- Preserve existing permissions, confirmation and default behavior. PostgreSQL TLS is opt-in through `DATABASE_SSLMODE`; use existing driver and certificate mechanisms.
- Catalogue filters use complete optional summaries, omit version data for core-only readers, and keep unavailable distinct from false. Verification remains unimplemented; its filter is intentionally present with an explicit limitation.
