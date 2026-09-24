#!/usr/bin/env bash
# Use the same commitlint dependencies and repository rules locally and in CI.
set -euo pipefail

if [[ $# != 2 ]]; then
  echo "Usage: bash scripts/check-release-commits.sh BASE HEAD" >&2
  exit 2
fi

release_base=$(git rev-parse --verify --end-of-options "${1}^{commit}")
release_head=$(git rev-parse --verify --end-of-options "${2}^{commit}")
release_base=$(git merge-base "$release_base" "$release_head")
release_root=$(git rev-parse --show-toplevel)
release_linter='wagoid/commitlint-github-action@sha256:86a04e0a99128551a7555c269d2b675c3c85f61358cf7dd558f6b873b66f561a'

# No GitHub token or Git directory enters the container. The entire range is
# supplied as data, avoiding event-specific API pagination and empty dispatches.
git log -z --format='%H%x00%B' "$release_base..$release_head" |
  docker run --rm -i --platform linux/amd64 --network none --read-only \
    --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=bind,source=$release_root/scripts,target=/checks,readonly" \
    --mount "type=bind,source=$release_root/commitlint.config.mjs,target=/config/commitlint.config.mjs,readonly" \
    --entrypoint sh "$release_linter" -c \
    'node --test /checks/lint-release-commits.test.mjs && node /checks/lint-release-commits.mjs'
