import importlib.util
import hashlib
import json
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("report", Path(__file__).with_name("test-report.py"))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class SourceSnapshot(unittest.TestCase):
    def test_snapshot_survives_edits_and_excludes_private_state(self):
        with TemporaryDirectory() as temp:
            root = Path(temp)
            for name in ("assets.go", "go.mod", "go.sum", "flake.nix", "flake.lock", "bases.json",
                         "guest/main.go", "scripts/bootstrap-guest.sh", "internal/provider/launch.go",
                         "tests/integration/c2j-fixture.go.txt"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("original " + name)
            script = root / "scripts/bootstrap-guest.sh"
            script.chmod(0o755)
            for name in (".git/config", ".c2/state", "test-results/old/lease",
                         "scripts/__pycache__/test.pyc", "private-journal.json"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("private")
            destination = root / "test-results/new"
            destination.mkdir()
            report.snapshot(root, destination)
            script.write_text("edited while tests ran")
            copied = destination / "source/scripts/bootstrap-guest.sh"
            self.assertEqual(copied.read_text(), "original scripts/bootstrap-guest.sh")
            self.assertEqual(copied.stat().st_mode & 0o777, 0o755)
            fingerprints = json.loads((destination / "source-sha256.json").read_text())
            self.assertEqual(len(fingerprints), 10)
            for name, digest in fingerprints.items():
                self.assertEqual(hashlib.sha256((destination / "source" / name).read_bytes()).hexdigest(), digest)
            self.assertEqual(len(list(p for p in (destination / "source").rglob("*") if p.is_file())), 10)

    def test_changes_during_capture_fail_before_testing(self):
        with TemporaryDirectory() as temp, patch.object(report, "capture_source", side_effect=[{}, {"changed": (b"", 0)}]):
            with self.assertRaisesRegex(RuntimeError, "Source changed"):
                report.snapshot(Path(temp), Path(temp))
            self.assertFalse((Path(temp) / "source").exists())


class ReportContract(unittest.TestCase):
    def events(self):
        return [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="output", Output=m)
                for m in report.MARKERS + ("IMAGE_UPDATE_PASS:",)] + [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="pass")]

    def test_complete_execution_passes(self):
        self.assertEqual(report.summarize(self.events(), "tart", 0)["result"], "passed")

    def test_image_update_required_only_for_tart(self):
        events = [e for e in self.events() if e.get("Output") != "IMAGE_UPDATE_PASS:"]
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")
        self.assertEqual(report.summarize(events, "orchard", 0)["result"], "passed")

    def test_missing_jobdb_assertion_fails(self):
        for index in range(len(report.MARKERS)):
            events = self.events()
            del events[index]
            self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")

    def test_skipped_live_test_fails(self):
        events = self.events()
        events[-1]["Action"] = "skip"
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")

    def test_failure_elsewhere_cannot_be_hidden(self):
        events = self.events() + [dict(Package="some/package", Test="TestOther", Action="fail")]
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")
        self.assertEqual(report.summarize(self.events(), "tart", 1)["result"], "failed")

    def test_markers_from_another_test_do_not_count(self):
        events = self.events()
        for event in events[:-1]:
            event["Test"] = "TestSmoke"
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")


if __name__ == "__main__":
    unittest.main()
