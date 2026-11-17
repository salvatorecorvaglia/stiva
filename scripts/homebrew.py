#!/usr/bin/env python3
"""Generate a checksum-pinned Homebrew manifest from verified release assets."""

import argparse
import hashlib
import json
from pathlib import Path
import re

CONFIG = json.loads(
    (Path(__file__).resolve().parents[1] / "packaging/homebrew/config.json").read_text()
)
TARGETS = (
    ("macos", "arm", "darwin_arm64"),
    ("macos", "intel", "darwin_amd64"),
    ("linux", "arm", "linux_arm64"),
    ("linux", "intel", "linux_amd64"),
)


def stable_version(tag):
    version = tag.removeprefix("v")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("Homebrew requires a stable version such as v1.2.3")
    return version


def asset_names(tag):
    version = stable_version(tag)
    name = CONFIG["name"]
    if CONFIG["kind"] == "cask":
        return [
            CONFIG["pattern"].format(version=version, arch=CONFIG[arch])
            for arch in ("arm", "intel")
        ]
    suffix = "_python313" if CONFIG["kind"] == "python" else ""
    return [f"{name}_{version}_{target}{suffix}.tar.gz" for _, _, target in TARGETS]


def render(tag, assets):
    version = stable_version(tag)
    name, title = CONFIG["name"], CONFIG["title"]
    base = f"https://github.com/salvatorecorvaglia/{name}/releases/download/v{version}"
    names = asset_names(tag)
    hashes = {}
    for asset in names:
        path = Path(assets) / asset
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError(f"Missing or empty release asset: {asset}")
        with path.open("rb") as stream:
            hashes[asset] = hashlib.file_digest(stream, "sha256").hexdigest()
    if CONFIG["kind"] == "cask":
        arm, intel = names
        return f'''cask "{name}" do
  version "{version}"
  sha256 arm: "{hashes[arm]}",
         intel: "{hashes[intel]}"

  on_arm do
    url "{base}/{arm}"
  end
  on_intel do
    url "{base}/{intel}"
  end

  name "{title}"
  desc "{CONFIG["description"]}"
  homepage "https://github.com/salvatorecorvaglia/{name}"

  livecheck do
    skip "This tap pulls checksum-verified stable release manifests"
  end

  app "{title}.app"
end
'''
    text = f'''class {title.replace(" ", "")} < Formula
  desc "{CONFIG["description"]}"
  homepage "https://github.com/salvatorecorvaglia/{name}"
  version "{version}"
  license "MIT"

'''
    if CONFIG["kind"] == "python":
        text += (
            '  include Language::Python::Virtualenv\n\n  depends_on "python@3.13"\n\n'
        )
    for os_name in ("macos", "linux"):
        text += f"  on_{os_name} do\n"
        for (os_key, arch, _), asset in zip(TARGETS, names, strict=True):
            if os_key == os_name:
                text += (
                    f'    on_{arch} do\n      url "{base}/{asset}"\n'
                    f'      sha256 "{hashes[asset]}"\n    end\n'
                )
        text += "  end\n\n"
    if CONFIG["kind"] == "python":
        text += f'''  def install
    ENV["PIP_NO_INDEX"] = "1"
    venv = virtualenv_create(libexec, "python3.13", system_site_packages: false)
    wheels = Dir["wheels/*.whl"].sort
    odie "Release contains no Python wheels" if wheels.empty?
    wheels.each {{ |wheel| venv.pip_install wheel, build_isolation: false }}
    bin.install_symlink libexec/"bin/{name}"
  end

  test do
    assert_match "{name}", shell_output("#{{bin}}/{name} {CONFIG["test"]}").downcase
  end
end
'''
    else:
        text += f'''  def install
    bin.install "{name}"
  end

  test do
    assert_match "Stiva version #{{version}}", shell_output("#{{bin}}/stiva --version")
  end
end
'''
    return text


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--assets", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--list-assets", action="store_true")
    args = parser.parse_args()
    if args.list_assets:
        print("\n".join(asset_names(args.version)))
        return
    if not args.assets or not args.output:
        parser.error("--assets and --output are required for generation")
    try:
        content = render(args.version, args.assets)
        args.output.mkdir(parents=True, exist_ok=True)
        filename = CONFIG["name"] + ".rb"
        (args.output / filename).write_text(content)
        digest = hashlib.sha256(content.encode()).hexdigest()
        (args.output / "homebrew-checksums.txt").write_text(f"{digest}  {filename}\n")
    except (ValueError, OSError) as error:
        parser.exit(1, f"homebrew: {error}\n")


if __name__ == "__main__":
    main()
