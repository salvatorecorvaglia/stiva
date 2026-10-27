#!/usr/bin/env python3
"""Update a running Compose application, verify health, and retain image rollback."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from contextlib import contextmanager
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
APP = "stiva"
SERVICES = ("stiva",)


def run(argv, *, capture=False, timeout=900):
    result = subprocess.run(
        argv, check=True, text=True, capture_output=capture, timeout=timeout
    )
    return result.stdout if capture else ""


def write_json(path, payload):
    temporary = path.with_suffix(".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 384)
    with os.fdopen(fd, "w") as stream:
        json.dump(payload, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)


@contextmanager
def locked(directory):
    directory.mkdir(parents=True, exist_ok=True, mode=448)
    lock = directory / "lock"
    try:
        fd = os.open(lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 384)
    except FileExistsError as exc:
        raise ValueError(f"An update is already running (lock: {lock})") from exc
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(str(os.getpid()))
        yield
    finally:
        lock.unlink()


class Deployment:
    def __init__(self, args):
        self.args = args
        self.directory = Path(args.state_dir).resolve()
        self.active = self.directory / "active.json"
        self.previous = self.directory / "previous.json"
        self.context = self.directory / "context.json"
        files = args.file or [str(ROOT / "docker-compose.yml")]
        self.base = ["docker", "compose"]
        if args.env_file:
            self.base += ["--env-file", str(Path(args.env_file).resolve())]
        if args.project_name:
            self.base += ["--project-name", args.project_name]
        for file in files:
            self.base += ["-f", str(Path(file).resolve())]

    def command(self):
        if self.active.exists():
            if (
                not self.context.exists()
                or json.loads(self.context.read_text()) != self.base
            ):
                raise ValueError(
                    "Update state belongs to another Compose configuration/project"
                )
        return self.base + (["-f", str(self.active)] if self.active.exists() else [])

    def config(self):
        return json.loads(
            run(self.command() + ["config", "--format", "json"], capture=True)
        )

    def live_images(self):
        images = {}
        for service in SERVICES:
            ids = run(
                self.command() + ["ps", "--all", "-q", service], capture=True
            ).split()
            if len(ids) != 1:
                raise ValueError(
                    f"Expected one running {service} container; deploy the stack first"
                )
            container = json.loads(run(["docker", "inspect", ids[0]], capture=True))[0]
            if (
                not container["State"]["Running"]
                or container["State"].get("Health", {}).get("Status", "") != "healthy"
            ):
                raise ValueError(
                    f"{service} is not healthy; restore a healthy baseline before updating"
                )
            images[service] = container["Image"]
        return images

    def up(self):
        run(
            self.command()
            + [
                "up",
                "-d",
                "--no-build",
                "--no-deps",
                "--pull",
                "never",
                "--wait",
                "--wait-timeout",
                str(self.args.timeout),
                *SERVICES,
            ],
            timeout=self.args.timeout + 60,
        )

    def override(self, images):
        write_json(self.context, self.base)
        write_json(
            self.active,
            {"services": {name: {"image": image} for name, image in images.items()}},
        )

    def rollback(self):
        saved = json.loads(self.previous.read_text())
        if saved["context"] != self.base:
            raise ValueError(
                "Rollback belongs to another Compose configuration/project"
            )
        self.override(saved["images"])
        self.up()
        print("Rollback passed health checks; persistent volumes were retained.")

    def update(self, replacements):
        configured = self.config()["services"]
        if not set(SERVICES).issubset(configured):
            raise ValueError("Compose configuration is missing application services")
        if not replacements or not set(replacements).issubset(SERVICES):
            raise ValueError(
                "Specify --image SERVICE=IMAGE for application services only"
            )
        previous = self.live_images()
        target = dict(previous)
        for service, image in replacements.items():
            if (
                not image
                or image.startswith("-")
                or any((char.isspace() for char in image))
            ):
                raise ValueError("Invalid image reference")
            run(["docker", "pull", image])
            target[service] = run(
                ["docker", "image", "inspect", "--format", "{{.Id}}", image],
                capture=True,
            ).strip()
            if not re.fullmatch("sha256:[0-9a-f]{64}", target[service]):
                raise ValueError(
                    "Could not resolve the downloaded image to a local immutable ID"
                )
            health = configured[service].get("healthcheck")
            if health is None:
                health = json.loads(
                    run(
                        [
                            "docker",
                            "image",
                            "inspect",
                            "--format",
                            "{{json .Config.Healthcheck}}",
                            image,
                        ],
                        capture=True,
                    )
                )
            test = (health or {}).get("test", (health or {}).get("Test", []))
            if (health or {}).get("disable") or not test or test[0] == "NONE":
                raise ValueError(
                    f"{service}: the new deployment must define a readiness health check"
                )
        if previous == target:
            print("The requested images are already running.")
            return
        journal = {"context": self.base, "images": previous}
        write_json(self.previous, journal)
        try:
            self.override(target)
            self.up()
        except (Exception, KeyboardInterrupt) as error:
            print(
                "Update failed; attempting recovery of the previous images.",
                file=sys.stderr,
            )
            try:
                self.rollback()
            except Exception as recovery:
                raise RuntimeError(
                    f"Recovery needs operator attention: {recovery}"
                ) from error
            raise RuntimeError(
                "Update failed; previous images were restored"
            ) from error
        print(
            "Update passed health checks. Use rollback to restore the previous images."
        )


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--file", "-f", action="append", help="Compose file; repeat for overrides"
    )
    parser.add_argument("--env-file")
    parser.add_argument("--project-name")
    parser.add_argument("--state-dir", default=str(ROOT / ".update-state"))
    parser.add_argument("--timeout", type=int, default=180)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("status")
    commands.add_parser("rollback")
    update = commands.add_parser("update")
    update.add_argument(
        "--image", action="append", required=True, metavar="SERVICE=IMAGE"
    )
    args = parser.parse_args(argv)
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    deployment = Deployment(args)
    try:
        with locked(deployment.directory):
            if args.command == "status":
                print(json.dumps(deployment.live_images(), indent=2))
            elif args.command == "rollback":
                deployment.rollback()
            else:
                replacements = {}
                for item in args.image:
                    name, separator, image = item.partition("=")
                    if not separator or name in replacements:
                        raise ValueError("Expected unique SERVICE=IMAGE entries")
                    replacements[name] = image
                deployment.update(replacements)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        parser.exit(1, f"{error}\n")


if __name__ == "__main__":
    main()
