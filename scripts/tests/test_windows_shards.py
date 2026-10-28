"""Every Go test kind must run exactly once across Windows shards."""

import importlib.util
import io
import json
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

spec = importlib.util.spec_from_file_location(
    "windows_shards", Path(__file__).parents[1] / "test-windows.py"
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class WindowsShardTests(unittest.TestCase):
    def test_partition_keeps_examples_fuzz_seeds_and_duplicate_package_names(self):
        listing = "\n".join(
            [
                "TestStorage",
                "TestAuth",
                "TestStorage",
                "FuzzPaths",
                "ExampleClient",
                "TestUpload",
                "ok\tgithub.com/example/storage\t0.123s",
                "? github.com/example/cmd [no test files]",
            ]
        )
        first = set(module.select_tests(listing, 1, 2))
        second = set(module.select_tests(listing, 2, 2))
        self.assertFalse(first & second)
        self.assertEqual(
            first | second,
            {"TestStorage", "TestAuth", "FuzzPaths", "ExampleClient", "TestUpload"},
        )

    def test_order_does_not_depend_on_package_completion_order(self):
        names = ["TestA", "ExampleB", "FuzzC", "TestD"]
        self.assertEqual(
            module.select_tests("\n".join(names), 1, 2),
            module.select_tests("\n".join(reversed(names)), 1, 2),
        )

    def test_top_level_filter_allows_the_selected_parent(self):
        names = module.select_tests("TestStorage\nFuzzStorage\nExampleStorage", 1, 1)
        pattern = "^(?:" + "|".join(re.escape(name) for name in names) + ")$"
        self.assertIsNotNone(re.fullmatch(pattern, "TestStorage"))
        self.assertIsNone(re.fullmatch(pattern, "TestStorageOther"))

    def test_failed_go_run_preserves_report_and_exit_status(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "results.jsonl"
            event = (
                json.dumps({"Action": "output", "Output": "FAIL: storage test\n"})
                + "\n"
            )
            process = MagicMock()
            process.__enter__.return_value = process
            process.stdout = iter([event])
            process.wait.return_value = 1
            listing = subprocess.CompletedProcess([], 0, "TestStorage\nFuzzStorage\n")
            argv = [
                "test-windows.py",
                "--shard",
                "1",
                "--shards",
                "2",
                "--report",
                str(report),
            ]
            with (
                patch.object(sys, "argv", argv),
                patch.object(module.subprocess, "run", return_value=listing),
                patch.object(module.subprocess, "Popen", return_value=process) as run,
                patch.object(sys, "stdout", io.StringIO()),
            ):
                self.assertEqual(module.main(), 1)
            self.assertEqual(report.read_text(), event)
            self.assertIn("-race", run.call_args.args[0])
            self.assertIn("-json", run.call_args.args[0])

    def test_missing_tests_and_invalid_shards_fail_closed(self):
        for listing, shard, shards in [
            ("ok package 0.01s", 1, 2),
            ("TestA", 0, 2),
            ("TestA", 3, 2),
            ("TestA", 2, 2),
        ]:
            with (
                self.subTest(listing=listing, shard=shard, shards=shards),
                self.assertRaises(ValueError),
            ):
                module.select_tests(listing, shard, shards)


if __name__ == "__main__":
    unittest.main()
