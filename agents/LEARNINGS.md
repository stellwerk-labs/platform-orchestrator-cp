# Learnings

- Release verification must distinguish signed annotated RC tags from the
  existing stable version owner's lightweight tags. Verify the source signature
  and exact remote ref in both cases; do not silently change the stable publisher.
- Git SSH verification trusts keys listed in allowed signers. A different
  principal with the same key is not an untrusted-key fixture; use a separate key.

- go-licenses v2.0.1 does not recognize the SDK root EUPL text. Exact declared SDK provenance must be handled explicitly; external unknown licenses remain fatal.
- gitleaks directory scans skip binary files. Stream printable binary strings through the same pinned scanner, without saving extracted secrets.
- Standard-library packages have no Go module record. Dependency notice collection must not treat their package paths as external modules.
- Published golib and graph modules use the same EUPL text. Explicit declarations must enumerate actual module identities, never use an owner-wide exception. Prefix-based classifier exclusions need collision guards.
- A content-only private-reference scan misses sensitive names. Scan relative artifact paths and rescan the complete generated evidence staging tree before publication.
- BuildKit local OCI exports need an explicit image name/tag to populate attestation subjects. An untagged export is not qualified evidence and is not a valid substitute in a tagged reproducibility pair.
- Go's adjacent string pool can create a secret-rule match across benign source literals. Require exact per-file evidence and source/build-metadata binding, never suppress a whole rule or binary class.
- Gitleaks8.30.1 always loads the scan target's .gitleaksignore even with --gitleaks-ignore-path /dev/null. Reject artifact ignore files explicitly, disable inline allow comments and isolate stdin scanner working directories.
- ORAS local-layout digest-only resolution can return application/octet-stream and omit image dependencies. Resolve a descriptor-verified root tag, select the platform and verify its destination manifest and blobs. Unprivileged Syft needs a writable mode1777 temporary filesystem and explicit cache location.
- Parsed tar entries omit arbitrary PAX fields and trailing bytes; gzip decompression also discards optional wrapper fields. Scan retained non-body tar bytes and every validated gzip member's wrapper separately from executable bodies.

## Cross-component source-notice parity

- CP's compiled dependency graph includes the same exact public reciprocal
  dependency fixtures as DP. Classify each actual file and module identity
  before porting a policy; matching module names alone are insufficient.
- Scanner-only changes can be reviewed independently of unchanged production
  Go, schema, module, build and embedded inputs. Preserve that distinction in
  image source labels and qualification evidence.

## Integration fixture coverage

- The earlier API/internal release gates did not run `integration-tests/Makefile`
  `test`, which exercises roughly 300 real server/client cases against the
  compose fixture. A passing `internal/...` API suite cannot establish that
  generated client response comparisons or legacy migration round trips still
  match the fully migrated schema. Include this separate suite before treating
  the source as CI-ready.
- Migration round-trip tests must start from a disposable schema at the version
  under test. Running an old Down script directly against the current schema
  crosses later foreign keys and triggers; migration 49 also deliberately
  refuses to discard retained Resource Type snapshots.
