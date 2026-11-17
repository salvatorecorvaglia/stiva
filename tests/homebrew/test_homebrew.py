"""Contract tests for release assets and idempotent tap publication."""

# ruff: noqa: PT009 -- unittest keeps release checks dependency-free.
import base64
import hashlib
import json
import os
import platform
import shutil
import subprocess
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def load(name):
    spec = importlib.util.spec_from_file_location(
        name, ROOT / "scripts" / (name + ".py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


generator = load("homebrew")
publisher = load("publish_homebrew")
with patch.dict(sys.modules, {"homebrew": generator}):
    preparer = load("prepare_homebrew")


class HomebrewTests(unittest.TestCase):
    def manifest(self, version="1.2.3"):
        with tempfile.TemporaryDirectory() as directory:
            for name in generator.asset_names(version):
                (Path(directory) / name).write_bytes(name.encode())
            return generator.render(version, directory)

    def test_exact_assets_hashes_and_version(self):
        content = self.manifest()
        self.assertEqual(publisher.version_of(content), (1, 2, 3))
        for name in generator.asset_names("v1.2.3"):
            self.assertIn(name, content)
            self.assertIn(hashlib.sha256(name.encode()).hexdigest(), content)
        self.assertNotIn(":no_check", content)
        if generator.CONFIG["kind"] == "python":
            self.assertIn('ENV["PIP_NO_INDEX"] = "1"', content)
            self.assertIn("system_site_packages: false", content)
        elif generator.CONFIG["kind"] == "cask":
            self.assertIn('app "' + generator.CONFIG["title"] + '.app"', content)

    def test_prerelease_and_unsafe_versions_rejected(self):
        for version in (
            "1.2.3-beta.1",
            "v1.2.3+build",
            "01.2.3",
            "../../bad",
            "1.2",
            '1.2.3"',
        ):
            with self.subTest(version=version), self.assertRaises(ValueError):
                generator.asset_names(version)

    def test_missing_or_empty_asset_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                generator.render("1.2.3", directory)
            for name in generator.asset_names("1.2.3"):
                (Path(directory) / name).touch()
            with self.assertRaises(ValueError):
                generator.render("1.2.3", directory)

    def test_first_publication_is_scoped_to_expected_file(self):
        content = self.manifest()
        with patch.object(
            publisher, "api", side_effect=[{"default_branch": "main"}, None, {}]
        ) as api:
            self.assertTrue(publisher.publish(content).startswith("Updated "))
        endpoint = api.call_args.args[0]
        folder = "Casks" if generator.CONFIG["kind"] == "cask" else "Formula"
        self.assertTrue(
            endpoint.endswith(folder + "/" + generator.CONFIG["name"] + ".rb")
        )
        payload = api.call_args.kwargs["payload"]
        self.assertEqual(base64.b64decode(payload["content"]).decode(), content)

    def test_unchanged_and_newer_tap_are_not_overwritten(self):
        content = self.manifest()
        for existing in (content, self.manifest("2.0.0")):
            with patch.object(
                publisher,
                "api",
                side_effect=[
                    {"default_branch": "main"},
                    {
                        "content": base64.b64encode(existing.encode()).decode(),
                        "sha": "original",
                    },
                ],
            ) as api:
                publisher.publish(content)
                self.assertEqual(api.call_count, 2)

    @unittest.skipUnless(shutil.which("brew"), "Homebrew is not installed")
    def test_native_homebrew_loads_manifest(self):
        if generator.CONFIG["kind"] == "cask" and platform.system() != "Darwin":
            self.skipTest("Casks require macOS")
        with tempfile.TemporaryDirectory() as directory:
            manifest = Path(directory) / (generator.CONFIG["name"] + ".rb")
            manifest.write_text(self.manifest())
            harness = Path(directory) / "check.rb"
            if generator.CONFIG["kind"] == "cask":
                loader = (
                    'require "cask/cask_loader"\n'
                    "c = Cask::CaskLoader.load(Pathname.new(ARGV.fetch(0)))\n"
                )
                loader += (
                    "puts JSON.generate({version: c.version.to_s, url: c.url.to_s})\n"
                )
            else:
                loader = 'require "formula"\nf = Formulary.factory(ARGV.fetch(0))\n'
                loader += (
                    "puts JSON.generate({version: f.version.to_s, url: f.stable.url})\n"
                )
            harness.write_text('require "json"\n' + loader)
            result = subprocess.run(
                ["brew", "ruby", str(harness), str(manifest)],
                capture_output=True,
                text=True,
                check=False,
                timeout=60,
                env={
                    **os.environ,
                    "HOMEBREW_NO_AUTO_UPDATE": "1",
                    "HOMEBREW_NO_ANALYTICS": "1",
                    "HOMEBREW_DEVELOPER": "0",
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            loaded = json.loads(result.stdout)
            self.assertEqual(loaded["version"], "1.2.3")
            self.assertIn("/v1.2.3/", loaded["url"])

    def test_preparation_rejects_wrong_release_and_missing_assets(self):
        release = {
            "tag_name": "v1.2.3",
            "draft": False,
            "prerelease": True,
            "assets": [],
        }
        with tempfile.TemporaryDirectory() as directory:
            for field in ("prerelease", "draft", "tag_name"):
                invalid = {
                    **release,
                    "prerelease": False,
                    field: True if field != "tag_name" else "v2.0.0",
                }
                with (
                    patch.object(
                        preparer.subprocess,
                        "check_output",
                        return_value=json.dumps(invalid),
                    ),
                    self.assertRaises(ValueError),
                ):
                    preparer.download("v1.2.3", Path(directory))
            release["prerelease"] = False
            with (
                patch.object(
                    preparer.subprocess,
                    "check_output",
                    return_value=json.dumps(release),
                ),
                self.assertRaises(ValueError),
            ):
                preparer.download("v1.2.3", Path(directory))

    def test_preparation_checks_download_digest(self):
        if generator.CONFIG["kind"] == "python":
            self.skipTest("Wheelhouses are supplied by tested native build jobs")
        names = generator.asset_names("1.2.3")
        release = {
            "tag_name": "v1.2.3",
            "draft": False,
            "prerelease": False,
            "assets": [
                {"name": name, "digest": "sha256:" + "0" * 64} for name in names
            ],
        }
        with tempfile.TemporaryDirectory() as directory:

            def download(command, **kwargs):
                filename = command[command.index("--pattern") + 1]
                (Path(directory) / filename).write_bytes(b"corrupted download")

            with (
                patch.object(
                    preparer.subprocess,
                    "check_output",
                    return_value=json.dumps(release),
                ),
                patch.object(preparer.subprocess, "run", side_effect=download),
                self.assertRaisesRegex(ValueError, "digest mismatch"),
            ):
                preparer.download("v1.2.3", Path(directory))

    def test_update_uses_existing_sha(self):
        old = self.manifest("1.0.0")
        with patch.object(
            publisher,
            "api",
            side_effect=[
                {"default_branch": "main"},
                {"content": base64.b64encode(old.encode()).decode(), "sha": "original"},
                {},
            ],
        ) as api:
            publisher.publish(self.manifest())
            self.assertEqual(api.call_args.kwargs["payload"]["sha"], "original")


if __name__ == "__main__":
    unittest.main()
