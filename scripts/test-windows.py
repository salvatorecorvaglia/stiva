#!/usr/bin/env python3
"""Partition top-level Go tests without dropping examples or fuzz seed tests."""

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path


def select_tests(listing: str, shard: int, shards: int) -> list[str]:
    if not 1 <= shard <= shards:
        raise ValueError("shard must be between 1 and the number of shards")
    names = sorted(
        {
            line
            for line in listing.splitlines()
            if re.fullmatch(r"(?:Test|Example|Fuzz)\w*", line)
        }
    )
    if not names:
        raise ValueError("go test did not enumerate any tests")
    selected = names[shard - 1 :: shards]
    if not selected:
        raise ValueError("shard is empty")
    return selected


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--shard", type=int, required=True)
    parser.add_argument("--shards", type=int, required=True)
    parser.add_argument("--report", type=Path, default=Path("test-results.jsonl"))
    args = parser.parse_args()
    print("Enumerating all race-enabled tests, examples and fuzz seeds", flush=True)
    listing = subprocess.run(
        ["go", "test", "-race", "-list", ".", "./..."],
        encoding="utf-8",
        stdout=subprocess.PIPE,
        check=True,
    )
    selected = select_tests(listing.stdout, args.shard, args.shards)
    pattern = "^(?:" + "|".join(re.escape(name) for name in selected) + ")$"
    print(
        f"Running shard {args.shard}/{args.shards}: {len(selected)} top-level tests",
        flush=True,
    )
    command = ["go", "test", "-timeout=10m", "-race", "-json", "-run", pattern, "./..."]
    with args.report.open("w", encoding="utf-8") as report:
        with subprocess.Popen(
            command, encoding="utf-8", stdout=subprocess.PIPE
        ) as process:
            for line in process.stdout:
                report.write(line)
                event = json.loads(line)
                if event.get("Action") == "output":
                    print(event.get("Output", ""), end="", flush=True)
            return process.wait()


if __name__ == "__main__":
    sys.stdout.reconfigure(encoding="utf-8")
    sys.exit(main())
