"""Offline deployment tests: readiness failure, rollback, and locks."""

import argparse
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location(
    "updates", ROOT / "scripts/update-deployment.py"
)
updates = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updates)
OLD = "sha256:" + "a" * 64
NEW = "sha256:" + "b" * 64


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.args = argparse.Namespace(
            state_dir=temp.name,
            file=["docker-compose.yml"],
            env_file="test.env",
            project_name="update-tests",
            timeout=10,
        )
        self.deployment = updates.Deployment(self.args)
        self.old = dict.fromkeys(updates.SERVICES, OLD)
        config = {
            "services": {
                name: {"image": OLD, "healthcheck": {"test": ["CMD", "ready"]}}
                for name in updates.SERVICES
            }
        }
        self.deployment.config = Mock(return_value=config)
        self.deployment.live_images = Mock(return_value=self.old)
        self.deployment.up = Mock()
        self.run = patch.object(updates, "run", return_value=NEW).start()
        self.addCleanup(patch.stopall)

    def test_success_pins_images_and_keeps_rollback(self):
        service = updates.SERVICES[-1]
        self.deployment.update({service: "registry/app:2.0"})
        saved = json.loads(self.deployment.previous.read_text())
        self.assertEqual(saved["images"], self.old)
        active = json.loads(self.deployment.active.read_text())["services"]
        self.assertEqual(active[service]["image"], NEW)
        self.deployment.up.assert_called_once()
        self.deployment.rollback()
        active = json.loads(self.deployment.active.read_text())["services"]
        self.assertTrue(all((item["image"] == OLD for item in active.values())))

    def test_failed_readiness_restores_previous_images(self):
        self.deployment.up.side_effect = [RuntimeError("unhealthy"), None]
        with self.assertRaisesRegex(RuntimeError, "previous images were restored"):
            self.deployment.update({updates.SERVICES[-1]: "registry/app:2.0"})
        self.assertEqual(self.deployment.up.call_count, 2)
        active = json.loads(self.deployment.active.read_text())["services"]
        self.assertTrue(all((item["image"] == OLD for item in active.values())))

    def test_failed_recovery_is_reported(self):
        self.deployment.up.side_effect = RuntimeError("unhealthy")
        with self.assertRaisesRegex(RuntimeError, "operator attention"):
            self.deployment.update({updates.SERVICES[-1]: "registry/app:2.0"})
        self.assertTrue(self.deployment.previous.exists())

    def test_unchanged_images_do_not_overwrite_previous_journal(self):
        self.run.return_value = OLD
        self.deployment.update({updates.SERVICES[-1]: "registry/app:1.0"})
        self.deployment.up.assert_not_called()
        self.assertFalse(self.deployment.previous.exists())

    def test_database_and_foreign_services_cannot_be_upgraded(self):
        for name in ("db", "other-service"):
            with self.assertRaisesRegex(ValueError, "application services"):
                self.deployment.update({name: "registry/image:2"})
        self.run.assert_not_called()

    def test_missing_candidate_healthcheck_is_rejected_before_restart(self):
        service = updates.SERVICES[-1]
        del self.deployment.config.return_value["services"][service]["healthcheck"]
        self.run.side_effect = lambda argv, **kwargs: (
            "null" if "{{json .Config.Healthcheck}}" in argv else NEW
        )
        with self.assertRaisesRegex(ValueError, "readiness health check"):
            self.deployment.update({service: "registry/app:2.0"})
        self.deployment.up.assert_not_called()
        self.assertFalse(self.deployment.previous.exists())

    def test_image_healthcheck_can_supply_readiness(self):
        service = updates.SERVICES[-1]
        del self.deployment.config.return_value["services"][service]["healthcheck"]
        self.run.side_effect = lambda argv, **kwargs: (
            json.dumps({"Test": ["CMD", "ready"]})
            if "{{json .Config.Healthcheck}}" in argv
            else NEW
        )
        self.deployment.update({service: "registry/app:2.0"})
        self.deployment.up.assert_called_once()

    def test_bad_image_reference_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "Invalid image"):
            self.deployment.update({updates.SERVICES[-1]: "--bad"})

    def test_saved_project_cannot_be_reused_for_another_stack(self):
        self.deployment.override(self.old)
        self.deployment.base += ["--project-name", "different"]
        with self.assertRaisesRegex(ValueError, "another Compose"):
            self.deployment.command()

    def test_rollback_context_is_verified_before_mutation(self):
        updates.write_json(
            self.deployment.previous, {"context": ["different"], "images": self.old}
        )
        with self.assertRaisesRegex(ValueError, "another Compose"):
            self.deployment.rollback()
        self.deployment.up.assert_not_called()

    def test_rollback_waits_and_never_removes_volumes_or_builds(self):
        self.deployment.up = updates.Deployment.up.__get__(self.deployment)
        self.deployment.up()
        command = self.run.call_args.args[0]
        for flag in ("--no-build", "--no-deps", "--wait", "--pull"):
            self.assertIn(flag, command)
        self.assertNotIn("down", command)
        self.assertNotIn("--volumes", command)

    def test_unhealthy_baseline_is_rejected(self):
        self.deployment.live_images = updates.Deployment.live_images.__get__(
            self.deployment
        )
        self.run.side_effect = [
            "container-id",
            json.dumps(
                [
                    {
                        "Image": OLD,
                        "State": {"Running": True, "Health": {"Status": "unhealthy"}},
                    }
                ]
            ),
        ]
        with self.assertRaisesRegex(ValueError, "not healthy"):
            self.deployment.live_images()

    def test_lock_prevents_concurrent_mutations_and_releases_on_failure(self):
        with updates.locked(self.deployment.directory):
            with self.assertRaisesRegex(ValueError, "already running"):
                with updates.locked(self.deployment.directory):
                    self.fail("second mutation must not run")
        self.assertFalse((self.deployment.directory / "lock").exists())


if __name__ == "__main__":
    unittest.main()
