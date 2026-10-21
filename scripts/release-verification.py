#!/usr/bin/env python3
"""Read-only identity checks for complete, nonpublishing release verification."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.request import Request, urlopen


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


candidate = load("candidate", "candidate-release.py")
stable = load("stable", "stable-release-preflight.py")


def git(*arguments):
    return subprocess.check_output(["git", *arguments], stderr=subprocess.PIPE)


def validate_signatures(repository, tag, source, tag_object, commit_record, ref_record, tag_record=None):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid repository identity")
    if (commit_record.get("sha") != source
            or commit_record.get("commit", {}).get("verification", {}).get("verified") is not True
            or ref_record.get("ref") != "refs/tags/" + tag
            or ref_record.get("object", {}).get("sha") != tag_object):
        raise ValueError("verified source and exact remote tag identity are required")
    if tag_record is None:
        if (not stable.STABLE.fullmatch(tag) or tag_object != source
                or ref_record.get("object", {}).get("type") != "commit"):
            raise ValueError("only existing stable tags may be lightweight")
    elif (ref_record.get("object", {}).get("type") != "tag"
            or tag_record.get("sha") != tag_object or tag_record.get("tag") != tag
            or tag_record.get("object", {}).get("type") != "commit"
            or tag_record.get("object", {}).get("sha") != source
            or tag_record.get("verification", {}).get("verified") is not True):
        raise ValueError("annotated tag signature must be verified at its exact identity")


def github_signatures(repository, tag, source, tag_object, annotated):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid repository identity")
    token = os.environ.get("GH_TOKEN")
    if not token:
        raise ValueError("authenticated read-only GitHub signature verification is required")

    def read(path):
        request = Request(f"https://api.github.com/repos/{repository}/{path}", headers={
            "Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        })
        with urlopen(request, timeout=20) as response:
            return json.load(response)

    validate_signatures(repository, tag, source, tag_object,
                        read("commits/" + source), read("git/ref/tags/" + tag),
                        read("git/tags/" + tag_object) if annotated else None)


def verify(tag, source, base, workflow, allowed_signers=None):
    for value in (source, base, workflow):
        if not re.fullmatch(r"[0-9a-f]{40}", value):
            raise ValueError("source, history base and workflow must be full lowercase commit SHAs")
    # Validate all user-controlled revisions before passing them to Git.
    if candidate.CANDIDATE.fullmatch(tag):
        validate = candidate.validate_identity
    elif stable.STABLE.fullmatch(tag):
        validate = stable.validate_stable_identity
    else:
        raise ValueError("verification requires an existing canonical stable or RC tag")
    head = git("rev-parse", "HEAD").decode().strip()
    tagged = git("rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}").decode().strip()
    validate(tag, source, head, tagged)
    tag_object = git("rev-parse", "--verify", f"refs/tags/{tag}").decode().strip()
    annotated = git("cat-file", "-t", tag_object).strip() == b"tag"
    if not annotated and not stable.STABLE.fullmatch(tag):
        raise ValueError("RC verification requires an annotated signed tag")
    if allowed_signers is not None:
        if allowed_signers.is_symlink() or not allowed_signers.is_file():
            raise ValueError("offline verification requires an explicit ordinary trusted-signers file")
        configuration = ("-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen",
                         "-c", "gpg.ssh.allowedSignersFile=" + str(allowed_signers.resolve()))
        git(*configuration, "verify-commit", source)
        if annotated:
            git(*configuration, "verify-tag", tag_object)
    else:
        github_signatures(os.environ.get("GITHUB_REPOSITORY", ""), tag, source, tag_object, annotated)
    if base == source:
        raise ValueError("verification history range must not be empty")
    if git("rev-parse", "--verify", f"{base}^{{commit}}").decode().strip() != base:
        raise ValueError("verification base must identify a commit")
    subprocess.run(["git", "merge-base", "--is-ancestor", base, source],
                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
    if git("cat-file", "-t", workflow).strip() != b"commit":
        raise ValueError("workflow identity must identify a commit")
    path = ".github/workflows/ci.yaml"
    if git("show", f"{workflow}:{path}") != Path(path).read_bytes():
        raise ValueError("verification source workflow differs from the dispatched workflow")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("source_sha")
    parser.add_argument("base_sha")
    parser.add_argument("workflow_sha")
    parser.add_argument("--allowed-signers-file", type=Path,
                        help="offline verification against an explicitly supplied SSH trust file")
    args = parser.parse_args()
    try:
        verify(args.tag, args.source_sha, args.base_sha, args.workflow_sha, args.allowed_signers_file)
    except (OSError, ValueError, subprocess.SubprocessError):
        parser.exit(1, "Release verification identity rejected; no publication performed.\n")
    print("Release verification identity checked; no publication performed.")


if __name__ == "__main__":
    main()
