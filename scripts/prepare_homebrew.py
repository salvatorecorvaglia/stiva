#!/usr/bin/env python3
"""Download exact stable-release assets, checking GitHub asset digests when present."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
from homebrew import CONFIG, asset_names, stable_version


def download(tag, destination):
    if tag != "v" + stable_version(tag):
        raise ValueError("Expected a v-prefixed stable release tag")
    repository = "salvatorecorvaglia/" + CONFIG["name"]
    release = json.loads(
        subprocess.check_output(
            ["gh", "api", f"repos/{repository}/releases/tags/{tag}"], text=True
        )
    )
    if release["tag_name"] != tag or release["draft"] or release["prerelease"]:
        raise ValueError("Expected the exact published stable release")
    assets = {asset["name"]: asset for asset in release["assets"]}
    destination.mkdir(parents=True, exist_ok=True)
    for name in asset_names(tag):
        local = destination / name
        # Python wheelhouses are produced by this workflow, not the earlier release job.
        if CONFIG["kind"] == "python":
            if not local.is_file():
                raise ValueError(f"Missing tested wheelhouse: {name}")
            continue
        if name not in assets:
            raise ValueError(f"Missing release asset: {name}")
        subprocess.run(
            [
                "gh",
                "release",
                "download",
                tag,
                "--repo",
                repository,
                "--pattern",
                name,
                "--dir",
                str(destination),
            ],
            check=True,
        )
        digest = assets[name].get("digest")
        if (
            digest
            and digest != "sha256:" + hashlib.sha256(local.read_bytes()).hexdigest()
        ):
            raise ValueError(f"GitHub asset digest mismatch: {name}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--directory", type=Path, required=True)
    args = parser.parse_args()
    download(args.tag, args.directory)
