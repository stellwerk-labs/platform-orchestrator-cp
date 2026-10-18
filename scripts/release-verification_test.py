#!/usr/bin/env python3
"""Real signed Git fixtures and nonpublishing workflow-mode regressions."""
import ast
import itertools
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = (ROOT / ".github/workflows/ci.yaml").read_text()
JOBS = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", WORKFLOW, re.M | re.S))
spec = importlib.util.spec_from_file_location("verification", ROOT / "scripts/release-verification.py")
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)


def condition(job, values):
    expression = next(line.strip()[4:] for line in JOBS[job].splitlines()
                      if line.strip().startswith("if: "))
    expression = expression.removeprefix("${{").removesuffix("}}").strip()
    for name, value in sorted(values.items(), key=lambda item: -len(item[0])):
        expression = expression.replace(name, repr(value))
    expression = re.sub(r"!(?!=)", " not ", expression).replace("&&", " and ").replace("||", " or ").strip()
    tree = ast.parse(expression, mode="eval")
    allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.UnaryOp, ast.Not,
               ast.Compare, ast.Eq, ast.NotEq, ast.Constant)
    if any(not isinstance(node, allowed) for node in ast.walk(tree)):
        raise ValueError("unexpected expression in the bounded Boolean wiring check")
    return bool(eval(compile(tree, "<workflow condition>", "eval"), {"__builtins__": {}}, {}))


class VerificationWorkflowTests(unittest.TestCase):
    def values(self, **overrides):
        values = {
            "github.repository": "stellwerk-labs/platform-orchestrator-cp",
            "github.event_name": "workflow_dispatch", "github.ref": "refs/heads/main",
            "inputs.tests_only": True, "cancelled()": False,
            "needs.candidate-preflight.result": "skipped",
            "needs.verification-preflight.result": "success",
        }
        values.update(overrides)
        return values

    def test_tests_only_dispatch_never_selects_either_publisher(self):
        for repository, ref in itertools.product(
                ("stellwerk-labs/platform-orchestrator-cp", "example/fork"),
                ("refs/heads/main", "refs/heads/release/candidate", "refs/tags/v3.1.0")):
            values = self.values(**{"github.repository": repository, "github.ref": ref})
            for job in ("candidate-preflight", "candidate-release", "release"):
                self.assertFalse(condition(job, values), job)
            self.assertTrue(condition("verification-preflight", values))

    def test_all_four_gates_require_the_selected_manual_preflight(self):
        gates = ("unit", "integration", "integration-postgres", "commitlint")
        for mode, candidate, verification, cancelled in itertools.product(
                (True, False), ("success", "failure", "skipped"),
                ("success", "failure", "skipped"), (True, False)):
            values = self.values(**{"inputs.tests_only": mode, "cancelled()": cancelled,
                                    "needs.candidate-preflight.result": candidate,
                                    "needs.verification-preflight.result": verification})
            expected = not cancelled and (verification if mode else candidate) == "success"
            for gate in gates:
                self.assertEqual(condition(gate, values), expected, (gate, values))
                self.assertIn("needs: [candidate-preflight, verification-preflight]", JOBS[gate])

    def test_ordinary_pr_and_push_keep_all_source_gates(self):
        for event in ("pull_request", "push"):
            values = self.values(**{"github.event_name": event,
                                    "needs.verification-preflight.result": "skipped"})
            for gate in ("unit", "integration", "integration-postgres", "commitlint"):
                self.assertTrue(condition(gate, values))
            self.assertEqual(condition("release", values), event == "push")
            self.assertFalse(condition("candidate-release", values))
        values = self.values(**{"inputs.tests_only": False})
        self.assertTrue(condition("candidate-release", values))
        self.assertFalse(condition("release", values))

    def test_verification_has_read_only_permissions_and_explicit_source_history(self):
        job = JOBS["verification-preflight"]
        self.assertIn("contents: read", job)
        self.assertNotIn(": write", job)
        self.assertNotIn("environment:", job)
        self.assertIn("fetch-depth: 0", job)
        self.assertIn("persist-credentials: false", job)
        self.assertIn("ref: ${{ inputs.candidate_sha }}", job)
        self.assertIn("${{ github.workflow_sha }}", job)
        self.assertIn("${{ inputs.verification_base_sha }}", job)
        self.assertIn("GH_TOKEN: ${{ github.token }}", job)
        self.assertNotIn("--allowed-signers-file", job)
        history = JOBS["commitlint"]
        self.assertIn('base="$VERIFICATION_BASE_SHA"', history)
        self.assertIn('bash scripts/check-release-commits.sh "$base" "$head"', history)
        for publisher in ("candidate-release", "release"):
            for gate in ("unit", "integration", "integration-postgres", "commitlint"):
                self.assertIn("- " + gate, JOBS[publisher])

    def test_github_signature_records_bind_exact_verified_source_and_tag(self):
        repository = "stellwerk-labs/platform-orchestrator-cp"
        source, tag_object = "a" * 40, "b" * 40
        commit = {"sha": source, "commit": {"verification": {"verified": True}}}
        ref = {"ref": "refs/tags/v3.1.0", "object": {"type": "tag", "sha": tag_object}}
        tag = {"sha": tag_object, "tag": "v3.1.0", "verification": {"verified": True},
               "object": {"type": "commit", "sha": source,
                          "url": f"https://api.github.com/repos/{repository}/git/commits/{source}"}}
        verification.validate_signatures(repository, "v3.1.0", source, tag_object, commit, ref, tag)
        for key, value in (("sha", "c" * 40), ("tag", "v3.1.1"),
                           ("verification", {"verified": False}),
                           ("object", {"type": "tag", "sha": source})):
            with self.subTest(key=key), self.assertRaises(ValueError):
                verification.validate_signatures(repository, "v3.1.0", source, tag_object,
                                                 commit, ref, {**tag, key: value})
        with self.assertRaises(ValueError):
            verification.validate_signatures(repository, "v3.1.0", source, tag_object,
                                             {"sha": source}, ref, tag)
        for altered in ({**ref, "ref": "refs/tags/v3.1.1"},
                        {**ref, "object": {"type": "tag", "sha": "c" * 40}}):
            with self.assertRaises(ValueError):
                verification.validate_signatures(repository, "v3.1.0", source, tag_object,
                                                 commit, altered, tag)

    def test_stable_lightweight_ref_preserves_automatic_tag_owner(self):
        repository, source = "stellwerk-labs/platform-orchestrator-cp", "a" * 40
        commit = {"sha": source, "commit": {"verification": {"verified": True}}}
        ref = {"ref": "refs/tags/v3.1.0", "object": {"type": "commit", "sha": source}}
        verification.validate_signatures(repository, "v3.1.0", source, source, commit, ref)
        with self.assertRaises(ValueError):
            verification.validate_signatures(repository, "v3.1.0-rc.1", source, source, commit,
                                             {**ref, "ref": "refs/tags/v3.1.0-rc.1"})


class VerificationGitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "source"
        self.repo.mkdir()
        # Disposable fixture keys, never the operator's signing agent/config.
        self.env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                        PYTHONDONTWRITEBYTECODE="1")
        key = self.root / "signer"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)],
                       env=self.env, check=True, capture_output=True)
        self.signers = self.root / "allowed-signers"
        self.signers.write_text("fixture@example.test " + key.with_suffix(".pub").read_text())
        self.git("init", "-q", "-b", "main")
        for name, value in (("user.name", "Release Fixture"), ("user.email", "fixture@example.test"),
                            ("gpg.format", "ssh"), ("user.signingkey", str(key)),
                            ("gpg.ssh.program", "ssh-keygen"), ("commit.gpgsign", "true"),
                            ("tag.gpgsign", "true")):
            self.git("config", name, value)
        workflow = self.repo / ".github/workflows/ci.yaml"
        workflow.parent.mkdir(parents=True)
        workflow.write_text("name: previous\n")
        self.base = self.commit()
        workflow.write_text("name: verified\n")
        self.source = self.commit()
        for tag in ("v3.1.0", "v3.1.0-rc.1"):
            self.git("tag", "-s", "-m", "Fixture release", tag)
        self.git("checkout", "-q", "-b", "other", self.base)
        workflow.write_text("name: unrelated\n")
        self.other = self.commit()
        self.git("checkout", "-q", "main")

    def git(self, *arguments):
        return subprocess.check_output(["git", *arguments], cwd=self.repo, env=self.env,
                                       stderr=subprocess.PIPE, text=True).strip()

    def commit(self):
        self.git("add", ".github/workflows/ci.yaml")
        self.git("commit", "-q", "-S", "-m", "test: create signed release fixture")
        return self.git("rev-parse", "HEAD")

    def verify(self, tag="v3.1.0-rc.1", source=None, base=None, workflow=None):
        before = self.git("show-ref")
        status = self.git("status", "--porcelain")
        result = subprocess.run([sys.executable, str(ROOT / "scripts/release-verification.py"), tag,
                                 source or self.source, base or self.base, workflow or self.source,
                                 "--allowed-signers-file", str(self.signers)],
                                cwd=self.repo, env=self.env, capture_output=True, text=True, timeout=10)
        self.assertEqual(self.git("show-ref"), before)
        self.assertEqual(self.git("status", "--porcelain"), status)
        return result

    def test_real_existing_signed_rc_and_stable_identities_pass_without_git_writes(self):
        for tag in ("v3.1.0-rc.1", "v3.1.0"):
            with self.subTest(tag=tag):
                result = self.verify(tag=tag)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_retargeted_and_injected_tags_fail(self):
        for tag in ("v3.1.0-rc.2", "latest", "../notes", "v03.1.0", "v3.1.0;touch marker"):
            with self.subTest(tag=tag):
                self.assertNotEqual(self.verify(tag=tag).returncode, 0)
        self.assertNotEqual(self.verify(source=self.base).returncode, 0)
        self.assertFalse((self.repo / "marker").exists())

    def test_empty_missing_or_nonancestor_history_base_fails(self):
        for base in (self.source, self.other, "0" * 40, "HEAD~1", "A" * 40):
            with self.subTest(base=base):
                self.assertNotEqual(self.verify(base=base).returncode, 0)

    def test_different_dispatched_workflow_fails_before_source_gates(self):
        self.assertNotEqual(self.verify(workflow=self.base).returncode, 0)
        self.assertNotEqual(self.verify(workflow="0" * 40).returncode, 0)
        self.assertNotEqual(self.verify(workflow=self.git("rev-parse", self.source + "^{tree}")).returncode, 0)

    def test_unsigned_and_lightweight_tags_and_untrusted_signers_fail(self):
        self.git("-c", "tag.gpgsign=false", "tag", "v3.1.1-rc.1")
        self.git("-c", "tag.gpgsign=false", "tag", "-a", "-m", "Unsigned", "v3.1.2")
        for tag in ("v3.1.1-rc.1", "v3.1.2"):
            self.assertNotEqual(self.verify(tag=tag).returncode, 0)
        untrusted = self.root / "untrusted"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(untrusted)],
                       env=self.env, check=True, capture_output=True)
        self.signers.write_text("fixture@example.test " + untrusted.with_suffix(".pub").read_text())
        self.assertNotEqual(self.verify().returncode, 0)

    def test_existing_lightweight_stable_tag_with_signed_source_passes(self):
        self.git("-c", "tag.gpgsign=false", "tag", "v3.1.1")
        result = self.verify(tag="v3.1.1")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_signed_tag_on_unsigned_source_commit_fails(self):
        workflow = self.repo / ".github/workflows/ci.yaml"
        workflow.write_text("name: unsigned-source\n")
        self.git("add", ".github/workflows/ci.yaml")
        self.git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "test: unsigned fixture")
        source = self.git("rev-parse", "HEAD")
        self.git("tag", "-s", "-m", "Signed tag over unsigned fixture", "v3.1.3")
        self.assertNotEqual(self.verify(tag="v3.1.3", source=source, workflow=source).returncode, 0)


if __name__ == "__main__":
    unittest.main()
