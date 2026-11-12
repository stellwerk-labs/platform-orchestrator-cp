#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import re
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("stable", Path(__file__).with_name("stable-release-preflight.py"))
stable = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stable)
SHA = "a" * 40


class StableReleaseTests(unittest.TestCase):
    def test_exact_canonical_stable_source(self):
        stable.validate_stable_identity("v3.1.0", SHA, SHA, SHA)
        for tag in ("v03.1.0", "v3.1.0-rc.1", "latest", "v3.1.0+build", "../notes"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                stable.validate_stable_identity(tag, SHA, SHA, SHA)
        for values in ((SHA, "b" * 40, SHA), (SHA, SHA, "b" * 40), ("abc", "abc", "abc")):
            with self.subTest(values=values), self.assertRaises(ValueError):
                stable.validate_stable_identity("v3.1.0", *values)

    def test_reviewed_notes_are_nonempty_in_checked_out_source(self):
        previous_directory = Path.cwd()
        with tempfile.TemporaryDirectory() as temporary_directory:
            notes = Path(temporary_directory, "docs/releases")
            notes.mkdir(parents=True)
            (notes / "v3.1.0.md").write_text("Reviewed notes.\n")
            (notes / "v3.1.1.md").write_text("")
            (notes / "v3.1.2.md").write_text(" \n\t")
            try:
                os.chdir(temporary_directory)
                stable.validate_release_notes("v3.1.0")
                for tag in ("v3.1.1", "v3.1.2", "v3.1.3"):
                    with self.subTest(tag=tag), self.assertRaises(ValueError):
                        stable.validate_release_notes(tag)
            finally:
                os.chdir(previous_directory)

    def test_publication_destinations_fail_closed(self):
        stable.candidate.validate_absence_status(404)
        for status in (200, 401, 403, 429, 500, 503):
            with self.subTest(status=status), self.assertRaises(ValueError):
                stable.candidate.validate_absence_status(status)
        for draft in (True, False):
            with self.subTest(draft=draft), self.assertRaises(ValueError):
                stable.candidate.validate_release_page(
                    "v3.1.0", [{"tag_name": "v3.1.0", "draft": draft}]
                )

    def test_latest_stable_cannot_be_downgraded_by_an_older_publisher(self):
        release = {"tag_name": "v3.10.2", "draft": False, "prerelease": False}
        for tag in ("v3.10.2", "v3.10.3", "v3.11.0", "v4.0.0"):
            with self.subTest(target=tag):
                stable.validate_latest_release(tag, 200, release)
        for tag in ("v3.10.1", "v3.9.20", "v2.99.99"):
            with self.subTest(target=tag), self.assertRaises(ValueError):
                stable.validate_latest_release(tag, 200, release)

    def test_first_stable_allows_only_proven_latest_absence(self):
        stable.validate_latest_release("v1.0.0", 404)
        for status in (401, 403, 429, 500, 503):
            with self.subTest(status=status), self.assertRaises(ValueError):
                stable.validate_latest_release("v1.0.0", status)

    def test_latest_stable_metadata_must_be_canonical_and_published(self):
        release = {"tag_name": "v3.1.0", "draft": False, "prerelease": False}
        records = (None, [], {}, {**release, "tag_name": None},
                   {**release, "tag_name": "v03.1.0"}, {**release, "tag_name": "v3.1.0-rc.1"},
                   {**release, "draft": True}, {**release, "prerelease": True},
                   {**release, "draft": 0}, {**release, "prerelease": "false"})
        for record in records:
            with self.subTest(record=record), self.assertRaises(ValueError):
                stable.validate_latest_release("v3.2.0", 200, record)

    def test_semantic_release_only_assigns_versions_with_existing_rules(self):
        config = json.loads(Path(__file__).parents[1].joinpath(".releaserc.json").read_text())
        self.assertEqual([plugin[0] for plugin in config["plugins"]], ["@semantic-release/commit-analyzer"])
        rules = config["plugins"][0][1]["releaseRules"]
        self.assertEqual(
            rules,
            [
                {"breaking": True, "release": "major"},
                {"revert": True, "release": "patch"},
                {"type": "feat", "release": "minor"},
                {"type": "fix", "release": "patch"},
                {"type": "build", "scope": "deps", "release": "patch"},
                {"type": "*", "release": "patch"},
            ],
        )

    def test_workflow_reserves_builds_verifies_then_publishes(self):
        text = Path(__file__).parents[1].joinpath(".github/workflows/ci.yaml").read_text()
        jobs = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", text, re.M | re.S))
        release = jobs["release"]
        image_step = next(
            step for step in jobs["qualified-artifact"].split("\n      - ") if "uses: docker/build-push-action@" in step
        )
        for required in (
            "python3 scripts/stable-release-preflight.py",
            "github.repository == 'stellwerk-labs/platform-orchestrator-cp'",
            "github.event_name == 'push'",
            "github.ref == 'refs/heads/main'",
            "fetch-depth: 0",
            "persist-credentials: false",
            "RELEASE_TAG: ${{ needs.stable-version.outputs.version }}",
            "SOURCE_SHA: ${{ github.sha }}",
            'gh release create "$RELEASE_TAG" --verify-tag --draft --latest=false',
            '--notes-file "docs/releases/$RELEASE_TAG.md"',
            'gh release edit "$RELEASE_TAG" --draft=false --latest',
            "docker buildx imagetools inspect",
        ):
            self.assertIn(required, release)
        self.assertIn("sbom: generator=docker/buildkit-syft-scanner@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9", image_step)
        self.assertIn("provenance: mode=max", image_step)
        self.assertLess(release.index("stable-release-preflight.py"), release.index("gh release create"))
        self.assertIn("context: .", image_step)
        self.assertIn("org.opencontainers.image.version=${{ env.RELEASE_VERSION }}", image_step)
        self.assertIn("org.opencontainers.image.revision=${{ env.RELEASE_SOURCE_SHA }}", image_step)
        self.assertIn("needs.qualified-artifact.result == 'success'", release)
        self.assertLess(release.index("server-publication.py check"), release.index("gh release create"))
        self.assertLess(release.index("gh release create"), release.index("imagetools inspect"))
        self.assertIn("semantic-release-action@", jobs["stable-version"])
        self.assertNotIn("semantic-release-action@", release)
        self.assertLess(release.index("imagetools inspect"), release.index("gh release edit"))
        self.assertIn("scripts/test-release-versioning.sh", jobs["unit"])
        self.assertNotIn("semantic-release-action@", jobs["candidate-release"])


if __name__ == "__main__":
    unittest.main()
