"""Exercise image promotion without contacting a container registry."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "publish-verified-images.sh"


class VerifiedImagesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        binary = self.root / "bin"
        binary.mkdir()
        docker = binary / "docker"
        docker.write_text('''#!/usr/bin/env python3
import os
from pathlib import Path
import sys
root = Path(os.environ["MOCK_ROOT"])
args = sys.argv[1:]
with (root / "calls").open("a") as log:
    log.write(" ".join(args) + "\\n")
if args == ["load"]:
    (root / "loaded").write_text(sys.stdin.read())
elif args[:2] == ["image", "inspect"]:
    arch = (root / "loaded").read_text().strip()
    if args[-1] == "{{.Id}}":
        print("sha256:" + arch)
    elif args[-1] == "{{.Os}}/{{.Architecture}}":
        print("linux/" + os.environ.get("MOCK_ARCH", arch))
''')
        docker.chmod(0o755)
        skopeo = binary / "skopeo"
        skopeo.write_text('''#!/usr/bin/env python3
import os
from pathlib import Path
import sys
root = Path(os.environ["MOCK_ROOT"])
args = sys.argv[1:]
with (root / "calls").open("a") as log:
    log.write(" ".join(args) + "\\n")
source = next(arg for arg in args if arg.startswith("oci-archive:"))
arch = Path(source.removeprefix("oci-archive:")).read_text().strip()
if args[-1] == "docker-daemon:stiva:ci":
    (root / "loaded").write_text(arch)
else:
    Path(args[args.index("--digestfile") + 1]).write_text("sha256:" + arch)
''')
        skopeo.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{binary}:{os.environ['PATH']}",
                        MOCK_ROOT=str(self.root), GITHUB_SHA="commit123",
                        GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="1",
                        IMAGE_NAME="example/stiva",
                        RELEASE_TAGS="example/stiva:1.0.0\nexample/stiva:latest",
                        GITHUB_STEP_SUMMARY=str(self.root / "summary"))
        for arch in ("amd64", "arm64"):
            directory = self.root / "verified-images" / f"verified-image-{arch}"
            directory.mkdir(parents=True)
            (directory / "image.oci.tar").write_text(arch)
            (directory / "image-id").write_text(f"sha256:{arch}\n")
            (directory / "commit").write_text("commit123\n")
            self.checksums(directory)

    def checksums(self, directory):
        (directory / "SHA256SUMS").write_text("".join(
            f"{hashlib.sha256((directory / name).read_bytes()).hexdigest()}  {name}\n"
            for name in ("image.oci.tar", "image-id", "commit")
        ))

    def run_promotion(self):
        return subprocess.run(["bash", str(SCRIPT)], cwd=self.root, env=self.env,
                              capture_output=True, text=True)

    def test_promotes_both_tested_images_by_digest(self):
        result = self.run_promotion()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = (self.root / "calls").read_text()
        self.assertEqual(calls.count("copy --all --preserve-digests"), 2)
        self.assertIn("imagetools create --tag example/stiva:1.0.0 --tag example/stiva:latest "
                      "example/stiva@sha256:amd64 example/stiva@sha256:arm64", calls)
        self.assertNotIn("build ", calls)

    def test_rejects_corrupt_archive_before_push(self):
        archive = self.root / "verified-images/verified-image-amd64/image.oci.tar"
        archive.write_bytes(b"corrupt")
        self.assertNotEqual(self.run_promotion().returncode, 0)
        self.assertFalse((self.root / "calls").exists())

    def test_rejects_wrong_commit_before_push(self):
        self.env["GITHUB_SHA"] = "other-commit"
        self.assertNotEqual(self.run_promotion().returncode, 0)
        self.assertFalse((self.root / "calls").exists())

    def test_rejects_wrong_architecture_before_push(self):
        self.env["MOCK_ARCH"] = "arm64"
        self.assertNotEqual(self.run_promotion().returncode, 0)
        self.assertNotIn("copy --all", (self.root / "calls").read_text())

    def test_rejects_wrong_image_identity_before_push(self):
        directory = self.root / "verified-images/verified-image-amd64"
        (directory / "image-id").write_text("sha256:other\n")
        self.checksums(directory)
        self.assertNotEqual(self.run_promotion().returncode, 0)
        self.assertNotIn("copy --all", (self.root / "calls").read_text())


if __name__ == "__main__":
    unittest.main()
