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
