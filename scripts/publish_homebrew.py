#!/usr/bin/env python3
"""Update only Formula/stiva.rb in the dedicated tap using GH_TOKEN."""

import argparse
import base64
import json
from pathlib import Path
import re
import subprocess


REPOSITORY = "salvatorecorvaglia/homebrew-tap"
ENDPOINT = f"repos/{REPOSITORY}/contents/Formula/stiva.rb"


def api(endpoint, payload=None, allow_missing=False):
    command = ["gh", "api", endpoint]
    if payload is not None:
        command += ["--method", "PUT", "--input", "-"]
    result = subprocess.run(
        command,
        input=json.dumps(payload) if payload is not None else None,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode:
        if allow_missing and "(HTTP 404)" in result.stderr:
            return None
        raise RuntimeError(
            f"GitHub API request failed for {endpoint}; check tap access and GH_TOKEN"
        )
    return json.loads(result.stdout)


def version_of(formula):
    match = re.search(
        r'^  version "([0-9]+)\.([0-9]+)\.([0-9]+)"$', formula, re.MULTILINE
    )
    if not match:
        raise ValueError("Formula must declare a stable version")
    return tuple(map(int, match.groups()))


def publish(formula):
    version = version_of(formula)
    # Verify the repository exists before treating a missing formula as bootstrap.
    repository = api(f"repos/{REPOSITORY}")
    existing = api(ENDPOINT, allow_missing=True)
    payload = {
        "message": f"Update Stiva to {'.'.join(map(str, version))}",
        "content": base64.b64encode(formula.encode()).decode(),
        "branch": repository["default_branch"],
    }
    if existing is not None:
        previous = base64.b64decode(existing["content"]).decode()
        if version_of(previous) > version:
            return "Tap already contains a newer release; skipping downgrade."
        if previous == formula:
            return "Tap formula is already up to date."
        payload["sha"] = existing["sha"]
    api(ENDPOINT, payload=payload)
    return f"Updated {REPOSITORY}/Formula/stiva.rb"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--formula", type=Path, required=True)
    args = parser.parse_args()
    try:
        print(publish(args.formula.read_text()))
    except (OSError, ValueError, RuntimeError, KeyError) as error:
        parser.exit(1, f"homebrew: {error}\n")


if __name__ == "__main__":
    main()
