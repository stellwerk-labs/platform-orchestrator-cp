#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import re
import subprocess
import textwrap
import unittest

spec = importlib.util.spec_from_file_location("candidate", Path(__file__).with_name("candidate-release.py"))
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)
SHA = "a" * 40


class CandidateReleaseTests(unittest.TestCase):
    def test_integration_dependencies_fail_closed_before_tests(self):
        workflow = Path(__file__).parents[1].joinpath(".github/workflows/ci.yaml").read_text()
        integration = workflow.split("\n  integration:\n", 1)[1].split("\n  commitlint:", 1)[0]
        for name in ("IAM", "DP"):
            self.assertIn(f"{name}_IMAGE: ${{{{ vars.CORE_{name}_INTEGRATION_IMAGE }}}}", integration)
        script = textwrap.dedent(integration.split("        run: |\n", 1)[1].split("\n      - ", 1)[0])
        image = "ghcr.io/stellwerk-labs/dependency@sha256:" + "a" * 64
        for iam, dp, succeeds in ((image, image, True), ("", image, False),
                                  (image, "", False), ("image:v2.2.0", image, False),
                                  (image, "image@sha256:123", False)):
            with self.subTest(iam=iam, dp=dp):
                result = subprocess.run(["/bin/bash", "-c", script],
                                        env={"IAM_IMAGE": iam, "DP_IMAGE": dp},
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode == 0, succeeds, result.stdout + result.stderr)

    def test_canonical_candidate_and_exact_commit(self):
        for tag in ("v3.0.0-rc.1", "v2.4.0-rc.12", "v0.7.0-rc.1"):
            with self.subTest(tag=tag):
                candidate.validate_identity(tag, SHA, SHA, SHA)

    def test_stable_tags_and_noncanonical_or_injected_input_fail(self):
        for tag in ("v3.0.0", "latest", "v03.0.0-rc.1", "v3.0.0-rc.0", "v3.0.0-rc.01",
                    "v3.0.0-rc.1+build", "../notes", "v3.0.0-rc.1\nother", "v3.0.0-rc.1;echo unsafe"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                candidate.validate_identity(tag, SHA, SHA, SHA)

    def test_retargeted_tags_and_mismatched_checkout_fail(self):
        for values in (("a" * 39, SHA, SHA), (SHA, "b" * 40, SHA),
                       (SHA, SHA, "b" * 40), ("A" * 40, SHA, SHA)):
            with self.subTest(values=values), self.assertRaises(ValueError):
                candidate.validate_identity("v3.0.0-rc.1", *values)

    def test_preexisting_environment_requires_real_reviewers(self):
        candidate.validate_environment({
            "name": "public-release-candidate",
            "protection_rules": [{"type": "required_reviewers", "reviewers": [{"type": "Team", "reviewer": {"id": 1}}]}],
        })
        for environment in ({}, {"name": "public-release-candidate"},
                            {"name": "public-release-candidate", "protection_rules": [{"type": "required_reviewers", "reviewers": []}]},
                            {"name": "public-release-candidate", "protection_rules": [{"type": "wait_timer", "wait_timer": 5}]}):
            with self.subTest(environment=environment), self.assertRaises(ValueError):
                candidate.validate_environment(environment)

    def test_only_explicit_not_found_proves_image_absence(self):
        candidate.validate_absence_status(404)
        for status in (200, 401, 403, 429, 500, 503):
            with self.subTest(status=status), self.assertRaises(ValueError):
                candidate.validate_absence_status(status)

    def test_empty_or_other_release_page(self):
        candidate.validate_release_page("v3.0.0-rc.1", [])
        candidate.validate_release_page("v3.0.0-rc.1", [{"tag_name": "v2.1.1", "draft": False}])

    def test_drafts_and_published_releases_reserve_candidate_tag(self):
        for draft in (True, False):
            with self.subTest(draft=draft), self.assertRaises(ValueError):
                candidate.validate_release_page("v3.0.0-rc.1", [{"tag_name": "v3.0.0-rc.1", "draft": draft}])

    def test_malformed_or_unbounded_release_page_fails_closed(self):
        for page in ({"message": "not found"}, [None], [{}], [{"tag_name": "v2.1.1", "draft": "false"}],
                     [{"tag_name": "v2.1.1", "draft": False}] * 101):
            with self.subTest(page=page), self.assertRaises(ValueError):
                candidate.validate_release_page("v3.0.0-rc.1", page)

    def test_workflow_is_manual_repository_guarded_and_approval_gated(self):
        text = Path(__file__).parents[1].joinpath(".github/workflows/ci.yaml").read_text()
        jobs = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", text, re.M | re.S))
        publication = jobs["candidate-release"]
        image_step = next(step for step in publication.split("\n      - ")
                          if "uses: docker/build-push-action@" in step)
        self.assertIn("\n          context: .\n", image_step,
                      "Build the checked-out approved SHA, not the workflow event's default Git context")
        self.assertIn("github.repository == 'stellwerk-labs/platform-orchestrator-", publication)
        self.assertIn("github.event_name == 'workflow_dispatch'", publication)
        self.assertIn("environment: public-release-candidate", publication)
        self.assertIn("- candidate-preflight", publication)
        self.assertIn("--verify-tag --prerelease --latest=false", publication)
        self.assertIn("ref: ${{ inputs.candidate_sha }}", publication)
        self.assertIn("--check-image-absent", publication)
        self.assertNotIn("semantic-release-action@", publication)
        self.assertNotIn("git tag ", publication)
        self.assertNotIn(":latest", publication)
        self.assertIn("/environments/public-release-candidate", jobs["candidate-preflight"])

    def test_all_existing_gates_test_candidate_sha_and_stable_path_stays_separate(self):
        text = Path(__file__).parents[1].joinpath(".github/workflows/ci.yaml").read_text()
        jobs = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", text, re.M | re.S))
        gates = ("unit", "integration") if "unit" in jobs else ("test-unit", "test-integration", "test-authorization-upgrade")
        for gate in gates:
            self.assertIn("- " + gate, jobs["candidate-release"])
            self.assertIn("inputs.candidate_sha || github.ref", jobs[gate])
        stable_condition = next(line for line in jobs["release"].splitlines() if line.strip().startswith("if:"))
        self.assertIn("github.event_name == 'push'", stable_condition)
        self.assertIn("github.ref == 'refs/heads/main'", stable_condition)
        self.assertNotIn("workflow_dispatch", stable_condition)


if __name__ == "__main__":
    unittest.main()
