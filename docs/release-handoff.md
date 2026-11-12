# CP qualified artifact pilot

Status: `LOCAL_ONLY`, hosted trial pending.

`make release-checks` runs the source, workflow and real filesystem handoff contracts without Docker, credentials or publication.
Run `bash -n scripts/install-release-tools.sh`, `shellcheck -x scripts/install-release-tools.sh`, and `git diff --check` after editing the installer or workflow.
Use `actionlint .github/workflows/ci.yaml` when available.
`make release-status RUN=<run-id>` reads job status from GitHub without changing the run.

The existing source, integration, security, legal, privacy and reproducibility gates remain required.
For stable main pushes, `stable-version` assigns the existing semantic version and checks source, notes and unused destinations, with the existing policy and no new environment.
For manual candidates, the approved existing RC tag and SHA remain fixed.
`qualified-artifact` builds the full OCI archive and an independent uncached rebuild, inspects both, compares payloads and stages scanned SBOM and provenance evidence.
It uploads only the original archive and qualified evidence, sealed by a complete inventory, under an immutable Actions artifact ID with 7-day retention.
The rebuild report remains evidence; the second archive is not uploaded.

The existing candidate environment approves publication after qualification.
Both publishers require the successful producer's artifact ID, API digest, inventory SHA, image digest and original attempt through `needs` outputs.
They check GitHub's nested artifact run identity against the current run and event head, then check every downloaded file and the exact source/workflow/version/index/provenance binding before reserving a draft.
They extract, reinspect and publish the original bytes, then retain the existing signatures, exact attestations and anonymous verification before exposing the release.
`go version -m` remains a read-only binary metadata check using the runner's local Go tool; no publisher compiles a binary or rebuilds an image.

A publisher-only rerun may consume the successful original producer from attempt `1` during consumer attempt `2`.
Its provenance continues to identify producer attempt `1`.
If a draft or image tag already exists, the existing absence checks refuse publication.
Stable preflight also refuses an older publisher when a newer stable version is already GitHub's latest release, before reserving a draft.
Inspect the draft, registry digests and verification evidence read-only and reconcile manually.
Do not automatically replay, overwrite, retag or resign a partially published release.
If the artifact expired or the producer failed to upload, there is no qualified handoff to resume.

Local contracts do not prove the hosted artifact transfer or rerun behavior.
Before calling the pilot proven, an explicitly authorized hosted RC trial must capture the producer artifact's actual API metadata and successful upload/download, then exercise a publisher-only rerun before any draft or image exists.
Confirm the producer does not rerun, its original archive and provenance remain unchanged, the environment gate still applies, and all existing cryptographic and anonymous checks pass.
Also confirm a rerun after a reserved draft or image refuses and requires read-only reconciliation.
No hosted dispatch or publication is part of local preparation.
The first run still performs 2 full builds; expected savings are in safe resumption and repeated orchestration work, not reduced release qualification.
