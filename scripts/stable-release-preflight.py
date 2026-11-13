#!/usr/bin/env python3
"""Verify stable publication prerequisites without publishing anything."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.error import HTTPError
from urllib.request import Request, urlopen

spec = importlib.util.spec_from_file_location("candidate", Path(__file__).with_name("candidate-release.py"))
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)

STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
SHA = re.compile(r"[0-9a-f]{40}")


def validate_stable_identity(tag, expected_sha, head_sha, tag_sha):
    if not STABLE.fullmatch(tag):
        raise ValueError("stable tag must be canonical vX.Y.Z")
    if not SHA.fullmatch(expected_sha):
        raise ValueError("stable source must be a complete lowercase commit SHA")
    if expected_sha != head_sha or expected_sha != tag_sha:
        raise ValueError("stable tag, event source and checkout must identify the same commit")


def validate_release_notes(tag):
    notes = Path("docs/releases", tag + ".md")
    if not notes.is_file() or not notes.read_text().strip():
        raise ValueError("reviewed stable release notes must be nonempty")


def validate_latest_release(tag, status, release=None):
    target = STABLE.fullmatch(tag)
    if not target:
        raise ValueError("stable tag must be canonical vX.Y.Z")
    if status == 404:
        return
    if (status != 200 or not isinstance(release, dict)
            or not isinstance(release.get("tag_name"), str)
            or release.get("draft") is not False or release.get("prerelease") is not False):
        raise ValueError("current latest stable release is not proven; refuse publication")
    latest = STABLE.fullmatch(release["tag_name"])
    if not latest:
        raise ValueError("current latest release must have a canonical stable tag")
    if tuple(map(int, target.groups())) < tuple(map(int, latest.groups())):
        raise ValueError("a newer stable release is already latest; refuse publication")


def assert_latest_not_newer(tag):
    token = os.environ.get("GH_TOKEN")
    if not token:
        raise ValueError("GH_TOKEN is required to verify the current latest release")
    request = Request(
        f"https://api.github.com/repos/{candidate.REPOSITORY}/releases/latest",
        headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"},
    )
    try:
        with urlopen(request, timeout=20) as response:
            validate_latest_release(tag, response.status, json.load(response))
    except HTTPError as error:
        validate_latest_release(tag, error.code)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("sha")
    args = parser.parse_args()
    validate_stable_identity(args.tag, args.sha, args.sha, args.sha)
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    tag = subprocess.check_output(
        ["git", "rev-parse", "--verify", f"refs/tags/{args.tag}^{{commit}}"], text=True
    ).strip()
    validate_stable_identity(args.tag, args.sha, head, tag)
    validate_release_notes(args.tag)
    assert_latest_not_newer(args.tag)
    candidate.assert_release_unreserved(args.tag)
    candidate.assert_image_absent(candidate.REPOSITORY, args.tag)
    print(f"Stable publication prerequisites verified for {args.tag}; no writes performed.")


if __name__ == "__main__":
    main()
