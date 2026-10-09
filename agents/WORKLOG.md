# Worklog

## Complete raw tar metadata coverage

- Retain every raw layer header, PAX extension, padding and trailing byte in a
  separate metadata stream, excluding only regular bodies already retained and
  inspected separately. Validate monotonic body offsets and exact body sizes.
  Keep the existing binary-only policy bound to per-file executable evidence.
- Real OCI fixtures with seeded credentials in arbitrary PAX metadata and after
  tar EOF both reproduced the gap before the correction. Mandatory strict scans
  reject both afterward without printing their values. Whiteout coverage remains.

## Organisation licence fallback

- A valid or grace organisation grant keeps precedence. Expired or revoked
  organisation grants fall back to a valid or grace installation grant without
  merging feature sets or deleting grant, revocation or audit evidence.
- Operator status, discovery, service preflight and transactional effects use
  the same scope-selection rule. Disabled or incompatible installations remain
  blocked. All enabled organisations count against the soft enablement limit;
  a lower renewal limit does not disable existing organisations.
- Imports hold an exclusive transaction guard across document savepoints;
  decisions hold its shared counterpart through their effects. Effect and
  discovery transactions explicitly use READ COMMITTED, independent of a
  session's default isolation.
- Unit, real PostgreSQL/API, concurrent revocation and rejected-document tests
  cover these rules. Overlay mutations demonstrate that the fallback and
  isolation regressions fail when their fixes are removed. No schema change.

## Immutable BuildKit SBOM generator

- Pinned BuildKit's existing SBOM generator to
  docker/buildkit-syft-scanner@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9
  in both candidate/stable builds and any existing dependency-seed pair.
  Official registry descriptors verify the index and Linux amd64/arm64 manifests.
- Preserve maximum provenance and all SBOM attestations. This existing generator
  is distinct from the unchanged external Syft 1.54.1 image used after inspection.
  Workflow contracts require the exact generator pin and reject mutable sbom:true.
  No build, push, or publication was executed for this tooling correction.

## Exact ORAS installer version validation

- Corrected shell ERE escaping in the Linux ORAS1.3.4 installer. The regression executes the actual installer expression with valid and invalid version output, rejecting wildcard dots and version suffixes without changing approved tool/version/archive checksums.

## Server artifact foundation

- Added pinned image bases, deterministic Go build settings, architecture-specific /licenses collection, exact SDK source-license provenance, and safe OCI payload inspection/scanning.
- Added helper fixture tests with mandatory pinned go-licenses and gitleaks, including both architectures, changed/missing legal content, binary secrets and whiteouts.
- Fixture/static evidence only. Production builds, hosted suites, public SDK resolution, independent full-image reproducibility and cryptographic attestation verification remain separate required evidence.
- See DECISIONS.md and LEARNINGS.md for the scope and tool limitations.

## Prepublication artifact wiring

- Candidate, stable and existing dependency-seed publishers now build complete local OCI layouts, preserve BuildKit SPDX/provenance, rebuild without instruction cache, inspect/scan both payload sets, compare actual platform bytes and generate pinned offline Syft SPDX before publication.
- ORAS copies the exact inspected layout with unchanged attestation/index bytes. BuildKit itself is digest-pinned; source epoch and gzip/media settings are explicit. Evidence distinguishes platform equality from envelope/index variance.
- Existing human-review environments, repository/event guards, strict suites and duplicate-destination checks remain. No publication was executed as part of implementation.

## Scope and checkout portability corrections

- Restricted the application-prefix exception to its root module and exact same-checkout shared SDK, shipping the actual own SDK license explicitly. Similarly prefixed external modules fail closed. Mandatory helper tests and shell checks passed.
- Made workflow contracts identify the exact component module from go.mod rather than checkout directory name. Added a neutral source-checkout regression and unknown-module rejection, preserving DP publisher and IAM migrator assertions in isolated private CI.

## Exact nullable legal-notice correction

- Reused the reviewed client collector's exact nullable v1.2.0 module checksum, original header hash and authoritative Apache-2.0 supplement hash. Preserve original bytes and record the supplemental source separately; reject replacements, different versions, hash changes and prefix collisions. Other unknown licenses remain fatal.
- The native IAM production graph does not include nullable. DP production does, and CP uses the DP SDK. No exception is silently extended to nullable v1.1.0.

## Strict binary scanner qualification

- Added a source/hash/buildinfo-bound exception for the actual IAM migrator's compiler-adjacent benign validation literals. Keep raw Gitleaks reports only in memory, fail malformed/tool-error/unapproved findings, and report sanitized policy/count only.
- Added real Go ELF and mutation/additional-secret regressions, plus artifact inline-allow and ignore-file suppression regressions. Reject artifact ignore files and isolate stdin scanning. Deduplicate identical raw/merged bytes and batch non-ELF binary strings without exceptions.
- Helper tests cover notice collection and architecture targets unchanged. Production OCI scanning, Syft and independent payload reproducibility are separate evidence and must be recorded from actual runs before qualification.
- Added no-exception relative-path secret scanning. Shared OCI reference validation now protects Syft platform copies and all dormant image publishers from digest-only descriptor/media-type loss; wrong index/platform/media-type regressions fail closed.

## Complete compressed-layer metadata scanning

- Reproduced scanner bypasses in arbitrary PAX metadata and post-tar trailing
  bytes, then retained every non-regular-body tar byte alongside the separately
  inspected original file bodies. Actual seeded credentials now fail closed.
- Reproduced gzip FNAME metadata bypass. Added bounded RFC 1952 member decoding
  with zlib CRC/ISIZE validation, retaining optional fields, fixed headers,
  header checksums, trailers and inter-member padding for strict scanning.
  Legitimate Distroless FNAME layers remain supported.
- Real two-member OCI tests cover independent FNAME, FCOMMENT and FEXTRA
  credentials; malformed members and output/metadata/member bounds fail closed.
  Scanner logs never contain fixture credentials. Actual final artifact rescans
  remain required; helper evidence does not qualify different artifact bytes.
- The new raw stream exposed the public Distroless container home. Exempt only
  exact `/home/nonroot/` from the developer-home pattern, never from secret or
  other concrete scans. Private homes and a credential under that path fail.

## Isolated CP public-source scanner parity

- Based this separate branch on e8e5ace0d5ac9332c26d490c293f077ec75e95bf.
  Original release/control worktrees, contexts and tar archives remain frozen.
- The real tar-built CP image produced exactly 82 findings from the same seven
  public files verified for DP. Every file hash, module version/sum, logical
  path, rule/location and per-file count matches the existing reviewed policy.
- Ported only the three DP scanner/test files' exact delta, byte-identical to
  DP afterward. All 30 helper tests pass, with strict negative regressions.
- Actual unchanged CP image export rescan passes in 65.48 seconds, accepting
  exactly 82 public-source findings and zero binary exceptions. This does not
  replace root's separate artifact/SBOM/publication review.
- Independently compared every other tracked file with the frozen source:
  Go/schema/modules, Dockerfile, production build/notice helpers and the sole
  Go embed pattern (model migration SQL) are unchanged. Root decides image
  qualification and any later source/CI repin; no new source identity claimed.
