#!/usr/bin/env python3
"""Enforce coverage for the packages that protect storage and API behavior.

Go's -coverpkg profiles can repeat a source block across test binaries; merge
those observations before counting statements so cross-package tests count.
"""
import argparse
from pathlib import Path

FLOORS = {"auth": 75.0, "console": 72.0, "s3api": 55.0, "storage": 78.0}
MODULE = "github.com/salvatorecorvaglia/stiva/internal/"


def check(profile: Path) -> bool:
    blocks = {}
    lines = profile.read_text().splitlines()
    if not lines or not lines[0].startswith("mode: "):
        raise ValueError("missing Go coverage mode header")
    for line in lines[1:]:
        location, statements, executions = line.rsplit(maxsplit=2)
        statements, executions = int(statements), int(executions)
        if statements < 0 or executions < 0:
            raise ValueError("negative coverage value")
        previous = blocks.get(location)
        if previous and previous[0] != statements:
            raise ValueError(f"inconsistent statement count: {location}")
        blocks[location] = (statements, executions > 0 or bool(previous and previous[1]))
    passed = True
    for package, floor in FLOORS.items():
        matching = [value for location, value in blocks.items() if location.startswith(f"{MODULE}{package}/")]
        total = sum(statements for statements, _ in matching)
        covered = sum(statements for statements, hit in matching if hit)
        percent = 100 * covered / total if total else 0
        print(f"{package}: {percent:.1f}% (minimum {floor:.1f}%)")
        passed &= bool(total) and percent >= floor
    return passed


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("profile", type=Path)
    args = parser.parse_args()
    try:
        success = check(args.profile)
    except (OSError, ValueError) as exc:
        parser.exit(1, f"Invalid coverage profile: {exc}\n")
    raise SystemExit(0 if success else 1)
