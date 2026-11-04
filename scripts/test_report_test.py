import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("report", Path(__file__).with_name("test-report.py"))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class ReportContract(unittest.TestCase):
    def events(self):
        return [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="output", Output=m)
                for m in report.MARKERS] + [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="pass")]

    def test_complete_execution_passes(self):
        self.assertEqual(report.summarize(self.events(), "tart", 0)["result"], "passed")

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
